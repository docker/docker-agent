package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// newMediaTestRuntime builds the minimal LocalRuntime materialization needs:
// a session store (for parent-chain WorkingDir lookup and the generated-media
// manifest) and a clock. It also confines the process data dir to a throwaway
// temp dir so every test can prove no generated file falls back there.
func newMediaTestRuntime(t *testing.T) (*LocalRuntime, session.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	paths.SetDataDir(dataDir)
	t.Cleanup(func() { paths.SetDataDir("") })
	store := session.NewInMemorySessionStore()
	return &LocalRuntime{sessionStore: store, now: time.Now}, store, dataDir
}

// workspaceSession returns a session owning a real, writable workspace root.
func workspaceSession(t *testing.T, id string) (*session.Session, string) {
	t.Helper()
	root := t.TempDir()
	return &session.Session{ID: id, WorkingDir: root}, root
}

func manifestOf(t *testing.T, store session.Store) session.GeneratedMediaManifest {
	t.Helper()
	manifest, ok := store.(session.GeneratedMediaManifest)
	require.True(t, ok, "the built-in store must implement the generated-media manifest")
	return manifest
}

// assertNoFilesUnder proves the no-data-dir-fallback contract: materialization
// must never create a file under the managed data dir anymore.
func assertNoFilesUnder(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			t.Errorf("unexpected file %s under the data dir: generated media must only land in the workspace", p)
		}
		return nil
	})
	require.NoError(t, err)
}

// collectingSink is a minimal [EventSink] that records every emitted
// event, for tests that need to inspect exactly what was emitted (not
// just observe side effects via a channel).
type collectingSink struct {
	events []Event
}

func (s *collectingSink) Emit(e Event) { s.events = append(s.events, e) }

func (s *collectingSink) warnings() []*WarningEvent {
	var out []*WarningEvent
	for _, e := range s.events {
		if w, ok := e.(*WarningEvent); ok {
			out = append(out, w)
		}
	}
	return out
}

// TestMaterializeGeneratedMedia_WritesIntoWorkspace is the core Phase-2.3
// contract: a generated item lands in the owning session's workspace at the
// exact final relative path the writer returns, the persisted part carries
// the workspace root kind plus that path, the manifest records the write,
// and nothing is created under the managed data dir (no fallback).
func TestMaterializeGeneratedMedia_WritesIntoWorkspace(t *testing.T) {
	r, store, dataDir := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-workspace")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01, 0x02}, MimeType: "image/png", Name: "cat.png", Size: 2},
	}, "root", sink)

	require.Len(t, parts, 1)
	assert.Empty(t, sink.warnings(), "a clean save must never emit a warning")

	doc := parts[0].Document
	require.NotNil(t, doc)
	assert.Equal(t, "cat.png", doc.Name)
	assert.Equal(t, "image/png", doc.MimeType)
	assert.Equal(t, "cat.png", doc.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootWorkspace, doc.Source.ArtifactRoot)
	assert.Equal(t, sess.ID, doc.Source.ArtifactOwnerSessionID)
	assert.Empty(t, doc.Source.InlineData, "the part must reference the workspace file, never carry bytes")

	data, err := os.ReadFile(filepath.Join(root, "cat.png"))
	require.NoError(t, err, "the generated file must be a real, visible workspace file")
	assert.Equal(t, []byte{0x01, 0x02}, data)

	file, err := manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat.png")
	require.NoError(t, err, "a successful write must be recorded in the manifest")
	assert.Equal(t, "image/png", file.MimeType)
	assert.False(t, file.CreatedAt.IsZero())

	assertNoFilesUnder(t, dataDir)
}

// TestMaterializeGeneratedMedia_InheritsWorkspaceFromParent proves the root
// comes from session.ResolveWorkingDir with the runtime's store as parent
// lookup: a sub-session without provenance of its own writes into its
// parent's workspace.
func TestMaterializeGeneratedMedia_InheritsWorkspaceFromParent(t *testing.T) {
	r, store, _ := newMediaTestRuntime(t)
	parent, root := workspaceSession(t, "parent")
	require.NoError(t, store.AddSession(t.Context(), parent))
	sub := &session.Session{ID: "sub", ParentID: parent.ID}

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sub, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1)
	assert.Empty(t, sink.warnings())
	assert.FileExists(t, filepath.Join(root, "cat.png"))
	assert.Equal(t, "sub", parts[0].Document.Source.ArtifactOwnerSessionID,
		"the owner is the generating session, even when the root comes from an ancestor")
}

// TestMaterializeGeneratedMedia_CollisionWritesSuffixedPath pins that an
// existing workspace file is never overwritten: the writer's dash-suffixed
// result is what gets persisted, displayed, and recorded in the manifest.
func TestMaterializeGeneratedMedia_CollisionWritesSuffixedPath(t *testing.T) {
	r, store, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-collision")
	require.NoError(t, os.WriteFile(filepath.Join(root, "cat.png"), []byte("user file"), 0o644))

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1)
	assert.Empty(t, sink.warnings())
	assert.Equal(t, "cat-1.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, "cat-1.png", parts[0].Document.Name)

	existing, err := os.ReadFile(filepath.Join(root, "cat.png"))
	require.NoError(t, err)
	assert.Equal(t, "user file", string(existing), "the pre-existing workspace file must be untouched")
	assert.FileExists(t, filepath.Join(root, "cat-1.png"))

	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat-1.png")
	require.NoError(t, err, "the manifest must record the FINAL (suffixed) path")
	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat.png")
	require.ErrorIs(t, err, session.ErrGeneratedFileNotFound, "the user's colliding file must never enter the manifest")
}

// TestMaterializeGeneratedMedia_ExtensionCorrectedNotice covers the writer's
// MIME/extension correction surfacing as a bounded, user-visible notice that
// names the exact final path.
func TestMaterializeGeneratedMedia_ExtensionCorrectedNotice(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-mime")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "photo.jpg", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "photo.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, "image/png", parts[0].Document.MimeType)
	assert.FileExists(t, filepath.Join(root, "photo.png"))

	warnings := sink.warnings()
	require.Len(t, warnings, 1, "the correction must surface exactly one notice")
	msg := warnings[0].Message
	assert.Contains(t, msg, "photo.png", "the notice must name the final path the user will find")
	assert.Contains(t, msg, ".jpg", "the notice must mention the requested extension that was replaced")
	assert.Contains(t, msg, "image/png")
	assertBoundedSingleLineUTF8(t, msg)
}

// TestMaterializeGeneratedMedia_EmptyNameFallback: a media delta with no
// display name (a real provider can legitimately omit InlineData.DisplayName)
// must fall back to the deterministic generic name, with the writer supplying
// the MIME-derived (or .bin) extension.
func TestMaterializeGeneratedMedia_EmptyNameFallback(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-empty-name")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "", Name: "   ", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1)
	assert.Empty(t, sink.warnings(), "a successful save must never emit a warning")
	assert.Equal(t, "generated-1.bin", parts[0].Document.Name)
	assert.Equal(t, "generated-1.bin", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, "application/octet-stream", parts[0].Document.MimeType,
		"an empty MIME type must persist the sanitized fallback, never the raw empty string")
	assert.FileExists(t, filepath.Join(root, "generated-1.bin"))
}

// TestMaterializeGeneratedMedia_ReservedProviderNameFallsBackToGeneric: a
// provider display name the workspace writer refuses even after display
// sanitization (Windows-reserved device names survive it) must not cost the
// user the item — it falls back to the generic name instead. There is no
// prompt-directed path to honor at this stage, so no confirmation flow.
func TestMaterializeGeneratedMedia_ReservedProviderNameFallsBackToGeneric(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-reserved")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "CON.png", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1)
	assert.Empty(t, sink.warnings())
	assert.Equal(t, "generated-1.png", parts[0].Document.Source.ArtifactPath)
	assert.FileExists(t, filepath.Join(root, "generated-1.png"))
}

// TestMaterializeGeneratedMedia_NoWorkspaceRoot covers the required
// no-root behavior: a session with no WorkingDir provenance anywhere gets
// the existing-style sanitized per-item warning for EVERY item, produces no
// parts (the caller keeps the turn's text), and never falls back to the
// managed data dir.
func TestMaterializeGeneratedMedia_NoWorkspaceRoot(t *testing.T) {
	r, store, dataDir := newMediaTestRuntime(t)
	sess := &session.Session{ID: "sess-no-root"}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
		{Data: []byte{0x02}, MimeType: "image/jpeg", Name: "dog.jpg", Size: 1},
	}, "root", sink)

	assert.Empty(t, parts, "without a workspace root no media item may survive")

	warnings := sink.warnings()
	require.Len(t, warnings, 2, "every item gets its own numbered warning")
	assert.Contains(t, warnings[0].Message, "1/2")
	assert.Contains(t, warnings[0].Message, "cat.png")
	assert.Contains(t, warnings[1].Message, "2/2")
	assert.Contains(t, warnings[1].Message, "dog.jpg")
	for _, w := range warnings {
		assert.Contains(t, w.Message, "No session workspace is available to save into.",
			"a missing workspace must surface its classified reason")
		assertSafeWarningMessage(t, w.Message, sess.ID, "")
	}

	assertNoFilesUnder(t, dataDir)
	_, err := manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat.png")
	require.ErrorIs(t, err, session.ErrGeneratedFileNotFound, "nothing was written, so nothing may be recorded")

	// The detailed cause (including the session ID) belongs in the debug
	// log, where an operator investigating the failure should look.
	assert.Contains(t, logBuf.String(), sess.ID)
}

// TestMaterializeGeneratedMedia_UnwritableRoot: valid provenance pointing at
// a root that cannot be opened (deleted workspace) fails per item with the
// standard sanitized warning — and must not leak the absolute root path.
func TestMaterializeGeneratedMedia_UnwritableRoot(t *testing.T) {
	r, _, dataDir := newMediaTestRuntime(t)
	root := filepath.Join(t.TempDir(), "deleted-workspace")
	sess := &session.Session{ID: "sess-unwritable", WorkingDir: root}

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
	}, "root", sink)

	assert.Empty(t, parts)
	warnings := sink.warnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "cat.png")
	assert.Contains(t, warnings[0].Message, "1/1")
	assert.Contains(t, warnings[0].Message, "The save location no longer exists.",
		"a deleted workspace root must surface its classified reason")
	assertSafeWarningMessage(t, warnings[0].Message, sess.ID, root)
	assertNoFilesUnder(t, dataDir)
}

// TestMaterializeGeneratedMedia_PartialSuccess_SingleBatchCall: ONE call with
// a two-item batch where exactly one sibling fails (injected through the
// workspacemediaWrite seam) must keep the surviving sibling's file, part,
// and manifest record, and warn only for the failing one — the manifest is
// written strictly per successful write.
func TestMaterializeGeneratedMedia_PartialSuccess_SingleBatchCall(t *testing.T) {
	r, store, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-partial")

	orig := workspacemediaWrite
	workspacemediaWrite = func(workspaceRoot, requestedPath string, data []byte, mimeType string) (workspacemedia.Result, error) {
		if mimeType == "image/jpeg" {
			return workspacemedia.Result{}, errors.New("injected failure for deterministic partial-success test")
		}
		return orig(workspaceRoot, requestedPath, data, mimeType)
	}
	t.Cleanup(func() { workspacemediaWrite = orig })

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
		{Data: []byte{0x02}, MimeType: "image/jpeg", Name: "dog.jpg", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1, "exactly the surviving sibling must produce a document part")
	assert.Equal(t, "cat.png", parts[0].Document.Name)

	warnings := sink.warnings()
	require.Len(t, warnings, 1, "exactly one warning for the one failing sibling")
	assert.Contains(t, warnings[0].Message, "2/2", "the failing item's index/total must reflect its real position in the batch")
	assert.Contains(t, warnings[0].Message, "dog.jpg")
	assert.Contains(t, warnings[0].Message, "image/jpeg")
	assert.Contains(t, warnings[0].Message, retryWithDebugAdvice,
		"an unclassified failure must carry the retry-with-debug advice")
	assert.NotContains(t, warnings[0].Message, "injected failure", "the raw error text must never reach the warning")
	assertSafeWarningMessage(t, warnings[0].Message, sess.ID, root)

	data, err := os.ReadFile(filepath.Join(root, "cat.png"))
	require.NoError(t, err, "the surviving sibling must actually be readable back from the workspace")
	assert.Equal(t, []byte{0x01}, data)

	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat.png")
	require.NoError(t, err)
	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "dog.jpg")
	require.ErrorIs(t, err, session.ErrGeneratedFileNotFound, "a failed write must never be recorded in the manifest")
}

// TestMaterializeGeneratedMedia_ClassifiedWriteFailureReasons drives every
// classified writer-failure category through the workspacemediaWrite seam
// and proves the per-item warning carries exactly the fixed classified
// sentence — while the raw error (with its embedded secret path) reaches
// only the debug log, never the warning.
func TestMaterializeGeneratedMedia_ClassifiedWriteFailureReasons(t *testing.T) {
	const secretPath = "/secret/root/cat.png"

	cases := []struct {
		name       string
		writeErr   error
		wantReason string
	}{
		{
			name:       "not writable",
			writeErr:   fmt.Errorf("claim %q: %w", secretPath, fs.ErrPermission),
			wantReason: "The save location is not writable.",
		},
		{
			name:       "read-only filesystem",
			writeErr:   fmt.Errorf("open workspace root: %w", &fs.PathError{Op: "open", Path: secretPath, Err: syscall.EROFS}),
			wantReason: "The save location is not writable.",
		},
		{
			name:       "collision exhaustion",
			writeErr:   fmt.Errorf("%w: %q after 10000 attempts", workspacemedia.ErrNameExhausted, secretPath),
			wantReason: "Every candidate filename is already taken.",
		},
		{
			// The provider-named flow retries ErrPathEscape once under the
			// generic name; the seam fails both attempts, so the refusal
			// itself must reach the user as the classified reason.
			name:       "requested path refused",
			writeErr:   fmt.Errorf("%w: %q: absolute path", workspacemedia.ErrPathEscape, secretPath),
			wantReason: "The requested save path was refused.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, dataDir := newMediaTestRuntime(t)
			sess, root := workspaceSession(t, "sess-classified-"+tc.name)

			var logBuf bytes.Buffer
			prevLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prevLogger) })

			orig := workspacemediaWrite
			workspacemediaWrite = func(string, string, []byte, string) (workspacemedia.Result, error) {
				return workspacemedia.Result{}, tc.writeErr
			}
			t.Cleanup(func() { workspacemediaWrite = orig })

			sink := &collectingSink{}
			parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
				{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
			}, "root", sink)

			assert.Empty(t, parts)
			warnings := sink.warnings()
			require.Len(t, warnings, 1)
			msg := warnings[0].Message
			assert.Contains(t, msg, "1/1")
			assert.Contains(t, msg, "cat.png")
			assert.Contains(t, msg, tc.wantReason)
			assert.NotContains(t, msg, retryWithDebugAdvice, "a classified failure must show its reason, not the debug fallback")
			assert.NotContains(t, msg, secretPath, "the warning must never leak a path embedded in the error")
			assertSafeWarningMessage(t, msg, sess.ID, root)

			assert.Contains(t, logBuf.String(), secretPath, "the detailed error must still reach the debug log")
			assertNoFilesUnder(t, dataDir)
		})
	}
}

// storeWithoutManifest hides the built-in store's GeneratedMediaManifest
// implementation: interface embedding only promotes session.Store's own
// method set, so the type assertion in recordGeneratedFile fails.
type storeWithoutManifest struct{ session.Store }

// TestMaterializeGeneratedMedia_ManifestFailureKeepsFileAndWarns: when the
// manifest cannot record a successful write, the file is already a real
// workspace deliverable — the reference is kept and the user is warned that
// inline display may refuse to render it (resolution fails closed on the
// missing manifest record).
func TestMaterializeGeneratedMedia_ManifestFailureKeepsFileAndWarns(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	r.sessionStore = storeWithoutManifest{r.sessionStore}
	sess, root := workspaceSession(t, "sess-no-manifest")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "image/png", Name: "cat.png", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1, "the written workspace file must keep its reference")
	assert.FileExists(t, filepath.Join(root, "cat.png"))

	warnings := sink.warnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "cat.png")
	assert.Contains(t, warnings[0].Message, "could not record it for display")
	assert.Contains(t, warnings[0].Message, retryWithDebugAdvice,
		"the manifest cause is unclassified storage internals, so the warning must carry the retry-with-debug advice")
	assert.NotContains(t, warnings[0].Message, "see debug log")
	assertBoundedSingleLineUTF8(t, warnings[0].Message)
}

// TestMaterializeGeneratedMedia_OneFailure_MaliciousMimeType covers the
// "sanitize ALL WarningEvent-visible metadata" contract on the new failure
// path: a malicious/malformed MIME type or name (control characters, an
// embedded newline that could forge an extra terminal/log line, traversal)
// must be neutralized in the warning message.
func TestMaterializeGeneratedMedia_OneFailure_MaliciousMimeType(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess := &session.Session{ID: "sess-malicious-mime"} // no root: deterministic failure

	const maliciousMime = "image/png\nWARNING: fake injected line\x00\x1b[31mred\x1b[0m"
	const maliciousName = "../../etc/passwd\x00.png\nWARNING: fake injected line"

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: maliciousMime, Name: maliciousName, Size: 1},
	}, "root", sink)

	assert.Empty(t, parts)
	warnings := sink.warnings()
	require.Len(t, warnings, 1)
	msg := warnings[0].Message

	assert.NotContains(t, msg, "\n", "a newline in the MIME type or name must never split the warning into extra lines")
	assert.NotContains(t, msg, "\x00", "a NUL byte must never reach the warning")
	assert.NotContains(t, msg, "\x1b", "a terminal escape sequence must never reach the warning")
	assert.NotContains(t, msg, "..", "a traversal sequence in the name must never reach the warning")
	assert.NotContains(t, msg, "/etc/passwd", "the raw malicious path fragment must never reach the warning")
	assertSafeWarningMessage(t, msg, sess.ID, "")
}

// TestMaterializeGeneratedMedia_OneFailure_EmptyNameFallbackInWarning: an
// empty or whitespace-only provider-supplied display name must still surface
// [fallbackDisplayName] in the failure WarningEvent — never be silently
// omitted — using exactly the same canonical fallback the placeholder text
// uses. The MIME type is left empty too, so [fallbackMimeType] must appear.
func TestMaterializeGeneratedMedia_OneFailure_EmptyNameFallbackInWarning(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess := &session.Session{ID: "sess-empty-name-warning"} // no root: deterministic failure

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: "", Name: "   ", Size: 1},
	}, "root", sink)

	assert.Empty(t, parts)
	warnings := sink.warnings()
	require.Len(t, warnings, 1)
	msg := warnings[0].Message
	assert.Contains(t, msg, "generated media", "an empty/whitespace-only name must fall back to the canonical display name, not be omitted")
	assert.Contains(t, msg, "application/octet-stream")
	assert.NotContains(t, msg, "()", "the name must never be omitted, leaving an empty parenthetical")
	assertSafeWarningMessage(t, msg, sess.ID, "")
}

// TestMaterializeGeneratedMedia_OneFailure_OverlongMetadataStaysBounded: a
// provider-supplied name (built from a multi-byte rune, so truncation must
// land on a rune boundary) and MIME type both well past
// [chat.MaxSanitizedFieldBytes] must still produce a warning that is valid
// UTF-8, single-line, control-character-free, and within
// [maxPlaceholderOrWarningBytes] overall.
func TestMaterializeGeneratedMedia_OneFailure_OverlongMetadataStaysBounded(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess := &session.Session{ID: "sess-overlong-warning"} // no root: deterministic failure

	// "é" is 2 UTF-8 bytes; 200 repetitions is 400 bytes, comfortably past
	// the 128-byte field bound, and an odd byte-count truncation point
	// would split the rune if TruncateUTF8Bytes were not rune-boundary safe.
	longName := strings.Repeat("é", 200)
	longMimeType := "image/" + strings.Repeat("x", 300)

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0x01}, MimeType: longMimeType, Name: longName, Size: 1},
	}, "root", sink)

	assert.Empty(t, parts)
	warnings := sink.warnings()
	require.Len(t, warnings, 1)
	msg := warnings[0].Message

	assertSafeWarningMessage(t, msg, sess.ID, "")
	assert.NotContains(t, msg, longName, "the full 400-byte name must have been truncated, not passed through")
	assert.NotContains(t, msg, longMimeType, "the full 306-byte MIME type must have been truncated, not passed through")
	assert.Contains(t, msg, "é", "the sanitized (truncated) multi-byte name must still be present")
}

// assertSafeWarningMessage asserts the "no absolute paths or raw OS errors"
// requirement: the warning must never contain the workspace root, the data
// dir, the raw session ID, or common OS-error phrasing that could leak a
// path indirectly. It also asserts the shared cross-output bound (valid
// UTF-8, single line, no control characters, <=512 bytes) every WarningEvent
// and placeholder line must satisfy — see assertBoundedSingleLineUTF8 in
// transforms_test.go. workspaceRoot may be "" when the test never had one.
func assertSafeWarningMessage(t *testing.T, msg, sessionID, workspaceRoot string) {
	t.Helper()
	assertBoundedSingleLineUTF8(t, msg)
	if workspaceRoot != "" {
		assert.NotContains(t, msg, workspaceRoot, "warning must not leak the absolute workspace root")
	}
	assert.NotContains(t, msg, paths.GetDataDir(), "warning must not leak the absolute data-dir path")
	assert.NotContains(t, msg, sessionID, "warning must not leak the raw session ID")
	for _, needle := range []string{"permission denied", "not a directory", "no such file", "open ", "mkdir "} {
		assert.NotContains(t, strings.ToLower(msg), needle, "warning must not leak raw OS error text")
	}
}
