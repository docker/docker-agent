package artifacts

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// Service stores and resolves generated media under its owning session.
// Store, Now, and Write are fixed before use; cached provenance is concurrency-safe.
type Service struct {
	Store session.Store
	Now   func() time.Time
	Write func(string, string, []byte, string) (workspacemedia.Result, error)
	roots generatedFileCache
}

// Materialize writes each streamed [chat.MediaDelta] into the
// owning session's workspace (the effective WorkingDir resolved via
// [session.ResolveWorkingDir]) through [workspacemedia.Write] and returns
// the corresponding document parts, so the persisted assistant message
// keeps only a relative, owner-qualified workspace reference
// ([chat.ArtifactRootWorkspace]) rather than raw bytes. sess.ID becomes the
// reference's permanent owner (see chat.DocumentSource) — it never changes
// even if this message is later copied into a branched or forked session.
// Each successful write is also recorded in the session store's
// generated-media manifest ([session.GeneratedMediaManifest]), the trust
// anchor a resolver must consult before reading a workspace path back.
//
// The requested filename is the prompt-directed path when one exists
// ([chat.MediaDelta.RequestedPath], populated by media-file marker pairing
// in handleStream, or — for a turn whose single blob no marker named — by
// deterministic explicit-filename extraction from the triggering user
// message, see [ApplyUserPromptRequestedPath]), otherwise the sanitized
// provider display name, otherwise a generic "generated-N"; the writer owns MIME/extension
// correction and collision suffixing, and the part persists the exact final
// path it returns. A corrected extension additionally surfaces a bounded
// user-visible notice naming the final path. A prompt-directed path that
// escapes the workspace (absolute, "..", or "~"-rooted) is redirected into
// the workspace root under the sanitized basename with a bounded warning.
//
// When no workspace root is available (no provenance anywhere in the parent
// chain, or a malformed stored value) every item fails with the same
// per-item warning contract as a write failure — there is deliberately no
// data-dir fallback, so generated files never land outside the workspace.
//
// A materialization failure drops that one media item, logs the detailed
// error (including the workspace root) to the debug log only, and emits a
// runtime [WarningEvent] carrying nothing but safe display metadata — the
// exact 1-based failed item index and total batch count, the sanitized MIME
// type, the sanitized provider-supplied name (or [FallbackDisplayName]
// when that name is empty, whitespace-only, or missing — never omitted,
// exactly like the strip_generated_media.go placeholder), and a fixed
// classified reason from [SaveFailureReason] (a retry-with-debug
// hint when the cause is unclassified, never raw error text) — so the failure
// is observable to the user/caller without leaking the absolute workspace
// path or a raw OS error (which could contain that path) into a surface a
// user might paste into a bug report or share screen. Both the name AND the
// MIME type are provider-supplied, untrusted strings — SanitizeMimeType
// (shared with strip_generated_media.go's placeholder text) strips control
// characters and newlines the same way chat.SanitizeDisplayName does for
// the name, applies the same [chat.MaxSanitizedFieldBytes] field bound, and
// falls back to [fallbackMimeType] for empty/invalid input. Every formatted
// warning/notice is additionally capped at [MaxWarningBytes].
// Only the sanitized MIME type is ever persisted into the resulting
// [chat.Document]. One item's failure must not affect a sibling that saves
// successfully in the same reply, and must not lose the (already generated)
// accompanying text either.
func (r *Service) Materialize(ctx context.Context, sess *session.Session, media []chat.MediaDelta, agentName string, warn func(string)) []chat.MessagePart {
	// Runs after marker pairing (handleStream already filled RequestedPath
	// for marker-named blobs) and before any write, so marker precedence and
	// the untrusted-path pipeline below apply unchanged.
	ApplyUserPromptRequestedPath(media, sess.GetLastUserMessageContent())

	root, rootErr := session.ResolveWorkingDir(ctx, sess, r.sessionLookup())
	if rootErr != nil {
		slog.DebugContext(ctx, "No workspace root for generated media; dropping every media item, keeping the rest of the turn",
			"agent", agentName, "session_id", sess.ID, "error", rootErr)
	} else {
		// Seed the resolver cache so live inline rendering of this turn's
		// media does not have to re-resolve the root from the store.
		r.roots.setRoot(sess.ID, root)
	}

	parts := make([]chat.MessagePart, 0, len(media))
	for i, m := range media {
		safeName := chat.SanitizeDisplayName(m.Name)
		safeMimeType := SanitizeMimeType(m.MimeType)
		warnItemFailed := func(err error) {
			slog.DebugContext(ctx, "Failed to materialize generated media into the workspace; dropping it, keeping the rest of the turn",
				"agent", agentName, "session_id", sess.ID, "workspace_root", root, "mime_type", m.MimeType, "index", i+1, "error", err)
			if warn == nil {
				return
			}
			displayName := safeName
			if displayName == "" {
				displayName = FallbackDisplayName
			}
			warning := fmt.Sprintf("Failed to save generated media item %d/%d (%s, %s). %s",
				i+1, len(media), safeMimeType, displayName, SaveFailureReason(err))
			warn(chat.TruncateUTF8Bytes(warning, MaxWarningBytes))
		}

		if rootErr != nil {
			warnItemFailed(rootErr)
			continue
		}

		res, err := r.writeGeneratedMedia(generatedMediaItem{
			workspaceRoot: root,
			requestedPath: m.RequestedPath,
			providerName:  safeName,
			genericName:   fmt.Sprintf("generated-%d", i+1),
			data:          m.Data,
			mimeType:      m.MimeType,
			agentName:     agentName,
			index:         i + 1,
			total:         len(media),
		}, warn)
		if err != nil {
			warnItemFailed(err)
			continue
		}
		manifestRecorded := true
		if err := r.recordGeneratedFile(ctx, sess.ID, chat.ArtifactRootWorkspace, res.RelPath, safeMimeType); err != nil {
			manifestRecorded = false
			// Keep the saved file, but warn that missing manifest authorization prevents display.
			slog.DebugContext(ctx, "Failed to record generated media in the manifest; the file was written but may not display inline",
				"agent", agentName, "session_id", sess.ID, "rel_path", res.RelPath, "error", err)
			if warn != nil {
				warning := fmt.Sprintf("Saved generated media %s but could not record it for display; it may not render inline. %s", res.RelPath, RetryWithDebugAdvice)
				warn(chat.TruncateUTF8Bytes(warning, MaxWarningBytes))
			}
		}

		if manifestRecorded {
			if err := r.recordGeneratedBlob(ctx, sess.ID, res.RelPath, m.Data); err != nil {
				slog.DebugContext(ctx, "Failed to store portable generated media; keeping the workspace file",
					"agent", agentName, "session_id", sess.ID, "rel_path", res.RelPath, "error", err)
				if warn != nil {
					warning := generatedBlobWarning(res.RelPath)
					warn(chat.TruncateUTF8Bytes(warning, MaxWarningBytes))
				}
			}
		}

		if res.ExtensionCorrected && warn != nil {
			notice := fmt.Sprintf("Saved generated media as %s: the requested extension %q does not match the returned %s data",
				res.RelPath, res.RequestedExtension, safeMimeType)
			warn(chat.TruncateUTF8Bytes(notice, MaxWarningBytes))
		}

		parts = append(parts, chat.MessagePart{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     path.Base(res.RelPath),
				MimeType: safeMimeType,
				Size:     m.Size,
				Source: chat.DocumentSource{
					ArtifactPath:           res.RelPath,
					ArtifactRoot:           chat.ArtifactRootWorkspace,
					ArtifactOwnerSessionID: sess.ID,
				},
			},
		})
	}
	return parts
}

// sessionLookup adapts the runtime's session store to [session.Lookup] for
// parent-chain WorkingDir resolution; nil when no store is configured.
func (r *Service) sessionLookup() session.Lookup {
	if r.Store == nil {
		return nil
	}
	return r.Store.GetSession
}

// recordGeneratedFile writes one manifest record after a successful write.
// Materialization is the only writer of the manifest; resolvers always read
// it back so authorization reflects current store state.
func (r *Service) recordGeneratedFile(ctx context.Context, sessionID string, root chat.ArtifactRootKind, finalPath, mimeType string) error {
	manifest, ok := r.Store.(session.GeneratedMediaManifest)
	if !ok {
		return fmt.Errorf("session store %T does not implement the generated-media manifest", r.Store)
	}
	file := session.GeneratedFile{
		SessionID: sessionID,
		RelPath:   finalPath,
		Root:      root,
		MimeType:  mimeType,
		CreatedAt: r.Now(),
	}
	if err := manifest.AddGeneratedFile(ctx, file); err != nil {
		return err
	}
	return nil
}

func (r *Service) recordGeneratedBlob(ctx context.Context, sessionID, finalPath string, data []byte) error {
	blobs, ok := r.Store.(session.GeneratedMediaBlobStore)
	if !ok {
		return fmt.Errorf("session store %T does not implement generated-media blob storage", r.Store)
	}
	return blobs.AddGeneratedBlob(ctx, sessionID, finalPath, data)
}

func generatedBlobWarning(finalPath string) string {
	return fmt.Sprintf("Saved generated media %s in the workspace, but could not keep a portable copy with the session. %s", finalPath, RetryWithDebugAdvice)
}
