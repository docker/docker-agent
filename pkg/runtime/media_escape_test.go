package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// newEscapeTestRuntime is newMediaTestRuntime plus the agent router the
// native elicitation path dereferences (user-input hooks, agent name).
func newEscapeTestRuntime(t *testing.T) (*LocalRuntime, session.Store) {
	t.Helper()
	r, store, _ := newMediaTestRuntime(t)
	r.agents = newAgentRouter(team.New(), "root")
	return r, store
}

// materializeWithAnswer drives one prompt-directed materialization on a
// separate goroutine — exactly how the runtime loop blocks on an
// elicitation while the embedder keeps consuming events — answers the
// escape confirmation with action, and returns the resulting parts, the
// emitted warnings, and the request event. Bounded waits double as the
// no-deadlock regression: a hang fails the test instead of wedging it.
func materializeWithAnswer(t *testing.T, r *LocalRuntime, sess *session.Session, media []chat.MediaDelta, action tools.ElicitationAction, content map[string]any) ([]chat.MessagePart, *collectingSink, *ElicitationRequestEvent) {
	t.Helper()
	requests := make(chan Event, 1)
	r.OnElicitationRequest(func(e Event) { requests <- e })

	sink := &collectingSink{}
	done := make(chan []chat.MessagePart, 1)
	go func() { done <- r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink) }()

	var req *ElicitationRequestEvent
	select {
	case e := <-requests:
		var ok bool
		req, ok = e.(*ElicitationRequestEvent)
		require.True(t, ok, "the sink must receive an elicitation request event, got %T", e)
	case <-time.After(10 * time.Second):
		t.Fatal("no elicitation request was emitted for the escaping path")
	}
	require.NoError(t, r.ResumeElicitation(t.Context(), action, content, req.ElicitationID))

	select {
	case parts := <-done:
		return parts, sink, req
	case <-time.After(10 * time.Second):
		t.Fatal("materialization did not finish after the elicitation response")
		return nil, nil, nil
	}
}

// escapeAcceptContent is the explicit affirmative form answer required to
// authorize an external write; a bare accept is never enough.
func escapeAcceptContent() map[string]any {
	return map[string]any{MediaEscapeDecisionField: MediaEscapeAcceptChoice}
}

// escapeMedia builds the canonical two-item batch: one prompt-directed
// escaping item plus one plain provider-named sibling, so every outcome
// test also proves sibling preservation.
func escapeMedia(requestedPath string) []chat.MediaDelta {
	return []chat.MediaDelta{
		{Data: []byte{0xAA}, MimeType: "image/png", Name: "cat.png", RequestedPath: requestedPath, Size: 1},
		{Data: []byte{0xBB}, MimeType: "image/png", Name: "sibling.png", Size: 1},
	}
}

// assertSiblingPreserved verifies the non-escaping second item of
// escapeMedia landed normally in the workspace.
func assertSiblingPreserved(t *testing.T, parts []chat.MessagePart, root string) {
	t.Helper()
	require.Len(t, parts, 2, "the sibling item must survive the escape handling")
	sibling := parts[1].Document
	require.NotNil(t, sibling)
	assert.Equal(t, chat.ArtifactRootWorkspace, sibling.Source.ArtifactRoot)
	assert.Equal(t, "sibling.png", sibling.Source.ArtifactPath)
	data, err := os.ReadFile(filepath.Join(root, "sibling.png"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0xBB}, data)
}

// TestMaterializeGeneratedMedia_EscapeAccept is the accept contract: an
// explicit accept writes to the confirmed external target with the regular
// writer mechanics, persists the external root kind plus confirmed path,
// records manifest membership, and the request event carries a safe prompt.
func TestMaterializeGeneratedMedia_EscapeAccept(t *testing.T) {
	r, store := newEscapeTestRuntime(t)
	sess, root := workspaceSession(t, "sess-escape-accept")
	target := filepath.Join(t.TempDir(), "exports", "cat.png")

	parts, sink, req := materializeWithAnswer(t, r, sess, escapeMedia(target), tools.ElicitationActionAccept, escapeAcceptContent())

	assert.Equal(t, "form", req.Mode)
	assert.Equal(t, MediaEscapeDecisionSchema(), req.Schema, "the confirmation must carry the explicit-choice schema")
	assert.Equal(t, mediaEscapeElicitationTitle, req.Meta["cagent/title"])
	assert.Equal(t, sess.ID, req.SessionID)
	assert.NotEmpty(t, req.ElicitationID)
	assert.Contains(t, req.Message, target, "the user must see the exact target being confirmed")
	assert.Contains(t, req.Message, "image/png")
	assert.NotContains(t, req.Message, sess.ID, "the prompt must not leak reference internals")

	doc := parts[0].Document
	require.NotNil(t, doc)
	assert.Equal(t, "cat.png", doc.Name)
	assert.Equal(t, target, doc.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootExternal, doc.Source.ArtifactRoot)
	assert.Equal(t, sess.ID, doc.Source.ArtifactOwnerSessionID)
	assert.True(t, isGeneratedMediaPart(parts[0]), "external parts must keep the strip/no-resend marker")

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte{0xAA}, data)

	file, err := manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, target)
	require.NoError(t, err, "an accepted external write must be manifest-gated like a workspace write")
	assert.Equal(t, chat.ArtifactRootExternal, file.Root)

	assertSiblingPreserved(t, parts, root)
	assert.Empty(t, sink.warnings(), "a confirmed external save must not warn")
}

// TestMaterializeGeneratedMedia_EscapeAcceptCollision: the confirmed target
// already existing must never be overwritten — the dash-suffixed final path
// is what gets persisted and recorded.
func TestMaterializeGeneratedMedia_EscapeAcceptCollision(t *testing.T) {
	r, store := newEscapeTestRuntime(t)
	sess, _ := workspaceSession(t, "sess-escape-collision")
	dir := t.TempDir()
	target := filepath.Join(dir, "cat.png")
	require.NoError(t, os.WriteFile(target, []byte("existing"), 0o644))

	parts, _, _ := materializeWithAnswer(t, r, sess, escapeMedia(target), tools.ElicitationActionAccept, escapeAcceptContent())

	final := filepath.Join(dir, "cat-1.png")
	doc := parts[0].Document
	require.NotNil(t, doc)
	assert.Equal(t, final, doc.Source.ArtifactPath)

	existing, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("existing"), existing, "the pre-existing external file must be untouched")

	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, final)
	require.NoError(t, err)
}

// TestMaterializeGeneratedMedia_EscapeRefused: decline and cancel both
// redirect the bytes into the workspace root under the sanitized basename,
// with a sanitized warning that names only the final workspace path.
func TestMaterializeGeneratedMedia_EscapeRefused(t *testing.T) {
	for _, action := range []tools.ElicitationAction{tools.ElicitationActionDecline, tools.ElicitationActionCancel} {
		t.Run(string(action), func(t *testing.T) {
			r, store := newEscapeTestRuntime(t)
			sess, root := workspaceSession(t, "sess-escape-"+string(action))
			extDir := t.TempDir()
			target := filepath.Join(extDir, "cat.png")

			parts, sink, _ := materializeWithAnswer(t, r, sess, escapeMedia(target), action, nil)

			doc := parts[0].Document
			require.NotNil(t, doc)
			assert.Equal(t, "cat.png", doc.Source.ArtifactPath)
			assert.Equal(t, chat.ArtifactRootWorkspace, doc.Source.ArtifactRoot)

			data, err := os.ReadFile(filepath.Join(root, "cat.png"))
			require.NoError(t, err, "refused bytes must be redirected, never discarded")
			assert.Equal(t, []byte{0xAA}, data)
			_, err = os.Stat(target)
			assert.True(t, os.IsNotExist(err), "nothing may be written outside the workspace without an accept")

			file, err := manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat.png")
			require.NoError(t, err)
			assert.Equal(t, chat.ArtifactRootWorkspace, file.Root)

			warnings := sink.warnings()
			require.Len(t, warnings, 1, "the redirect must be explained")
			assert.Contains(t, warnings[0].Message, "outside the workspace")
			assert.Contains(t, warnings[0].Message, "saved as cat.png")
			assert.NotContains(t, warnings[0].Message, extDir, "the warning must not echo the requested location")

			assertSiblingPreserved(t, parts, root)
		})
	}
}

// TestMaterializeGeneratedMedia_EscapeNonInteractive: with no user to ask,
// the runtime must redirect immediately — no elicitation event, no blocked
// stream.
func TestMaterializeGeneratedMedia_EscapeNonInteractive(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	r.nonInteractive = true
	r.OnElicitationRequest(func(e Event) { t.Errorf("no elicitation must be emitted in non-interactive mode, got %T", e) })
	sess, root := workspaceSession(t, "sess-escape-noninteractive")
	target := filepath.Join(t.TempDir(), "cat.png")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, escapeMedia(target), "root", sink)

	require.Len(t, parts, 2)
	assert.Equal(t, "cat.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootWorkspace, parts[0].Document.Source.ArtifactRoot)
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err))
	require.Len(t, sink.warnings(), 1)
	assertSiblingPreserved(t, parts, root)
}

// TestMaterializeGeneratedMedia_EscapeHeadlessBackground: a background
// session with no elicitation sink auto-declines (with the model-readable
// note) instead of parking the run forever.
func TestMaterializeGeneratedMedia_EscapeHeadlessBackground(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	sess, root := workspaceSession(t, "sess-escape-headless")
	target := filepath.Join(t.TempDir(), "cat.png")

	sink := &collectingSink{}
	ctx := tools.WithoutInteractivePrompts(t.Context())
	parts := r.materializeGeneratedMedia(ctx, sess, escapeMedia(target), "root", sink)

	require.Len(t, parts, 2)
	assert.Equal(t, chat.ArtifactRootWorkspace, parts[0].Document.Source.ArtifactRoot)
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err))
	assert.NotEmpty(t, r.elicitationDeclines.drain(sess.ID), "the auto-decline must leave a model-readable note")
	assertSiblingPreserved(t, parts, root)
}

// TestMaterializeGeneratedMedia_RequestedPathInWorkspace: a prompt-directed
// path contained in the workspace (including one that only cleans to a
// contained path) is honored without any confirmation.
func TestMaterializeGeneratedMedia_RequestedPathInWorkspace(t *testing.T) {
	tests := []struct {
		requested string
		finalPath string
	}{
		{"images/cat.png", "images/cat.png"},
		{"a/../b.png", "b.png"},
	}
	for _, tt := range tests {
		t.Run(tt.requested, func(t *testing.T) {
			r, store := newEscapeTestRuntime(t)
			r.OnElicitationRequest(func(e Event) { t.Errorf("a contained path must not elicit, got %T", e) })
			sess, root := workspaceSession(t, "sess-contained")

			sink := &collectingSink{}
			parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
				{Data: []byte{0x01}, MimeType: "image/png", RequestedPath: tt.requested, Size: 1},
			}, "root", sink)

			require.Len(t, parts, 1)
			assert.Equal(t, tt.finalPath, parts[0].Document.Source.ArtifactPath)
			assert.Equal(t, chat.ArtifactRootWorkspace, parts[0].Document.Source.ArtifactRoot)
			_, err := os.Stat(filepath.Join(root, filepath.FromSlash(tt.finalPath)))
			require.NoError(t, err)
			_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, tt.finalPath)
			require.NoError(t, err)
			assert.Empty(t, sink.warnings())
		})
	}
}

// TestMaterializeGeneratedMedia_RequestedPathUnusable: paths that are
// neither containable nor confirmable (unusable names, unexpandable "~user",
// control characters an elicitation prompt could not display faithfully)
// redirect without ever emitting an elicitation.
func TestMaterializeGeneratedMedia_RequestedPathUnusable(t *testing.T) {
	for name, requested := range map[string]string{
		"reserved name":       "CON.png",
		"unexpandable tilde":  "~nosuchuser/cat.png",
		"control-char target": "/external/bad\nname/cat.png",
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newEscapeTestRuntime(t)
			r.OnElicitationRequest(func(e Event) { t.Errorf("an unusable path must not elicit, got %T", e) })
			sess, root := workspaceSession(t, "sess-unusable")

			sink := &collectingSink{}
			parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
				{Data: []byte{0x01}, MimeType: "image/png", RequestedPath: requested, Size: 1},
			}, "root", sink)

			require.Len(t, parts, 1, "unusable requested paths must not cost the user the item")
			doc := parts[0].Document
			assert.Equal(t, chat.ArtifactRootWorkspace, doc.Source.ArtifactRoot)
			_, err := os.Stat(filepath.Join(root, filepath.FromSlash(doc.Source.ArtifactPath)))
			require.NoError(t, err)
			require.Len(t, sink.warnings(), 1)
			assert.Contains(t, sink.warnings()[0].Message, "saved as "+doc.Source.ArtifactPath)
		})
	}
}

// TestMaterializeGeneratedMedia_EscapeAcceptWriteFails: an accepted external
// write that then fails I/O keeps the existing per-item warning contract
// (sanitized, no raw error) and preserves siblings.
func TestMaterializeGeneratedMedia_EscapeAcceptWriteFails(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	sess, root := workspaceSession(t, "sess-escape-fail")
	target := filepath.Join(t.TempDir(), "cat.png")

	original := workspacemediaWriteExternal
	workspacemediaWriteExternal = func(string, []byte, string) (workspacemedia.Result, error) {
		return workspacemedia.Result{}, os.ErrPermission
	}
	t.Cleanup(func() { workspacemediaWriteExternal = original })

	parts, sink, _ := materializeWithAnswer(t, r, sess, escapeMedia(target), tools.ElicitationActionAccept, escapeAcceptContent())

	require.Len(t, parts, 1, "the failed item is dropped, the sibling survives")
	assert.Equal(t, "sibling.png", parts[0].Document.Source.ArtifactPath)
	_, err := os.ReadFile(filepath.Join(root, "sibling.png"))
	require.NoError(t, err)

	warnings := sink.warnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "Failed to save generated media item 1/2")
	assert.NotContains(t, warnings[0].Message, target, "the warning must not leak the external path")
	assert.NotContains(t, warnings[0].Message, os.ErrPermission.Error(), "the warning must not leak the raw error")
}

// TestMaterializeGeneratedMedia_EscapeAcceptRequiresExplicitChoice: the
// accept ACTION alone must never authorize an external write — only the
// exact affirmative form value does. This covers bare submits, empty
// free-form answers, the safe default choice, and permissive clients that
// accept with arbitrary content.
func TestMaterializeGeneratedMedia_EscapeAcceptRequiresExplicitChoice(t *testing.T) {
	for name, content := range map[string]map[string]any{
		"nil content":      nil,
		"empty content":    {},
		"default choice":   {MediaEscapeDecisionField: MediaEscapeDeclineChoice},
		"free-form answer": {"response": "yes"},
		"non-string value": {MediaEscapeDecisionField: true},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newEscapeTestRuntime(t)
			sess, root := workspaceSession(t, "sess-escape-implicit")
			extDir := t.TempDir()
			target := filepath.Join(extDir, "cat.png")

			parts, sink, _ := materializeWithAnswer(t, r, sess, escapeMedia(target), tools.ElicitationActionAccept, content)

			doc := parts[0].Document
			require.NotNil(t, doc)
			assert.Equal(t, "cat.png", doc.Source.ArtifactPath)
			assert.Equal(t, chat.ArtifactRootWorkspace, doc.Source.ArtifactRoot)
			_, err := os.Stat(target)
			assert.True(t, os.IsNotExist(err), "an accept without the explicit affirmative choice must not write outside the workspace")
			require.Len(t, sink.warnings(), 1, "the redirect must be explained")
			assertSiblingPreserved(t, parts, root)
		})
	}
}

// TestMaterializeGeneratedMedia_EscapeDirectoryTarget: confirming an
// existing directory means "write the generated filename inside it" — the
// user is shown, and confirms, the exact final file path, never a
// dash-suffixed sibling of the directory.
func TestMaterializeGeneratedMedia_EscapeDirectoryTarget(t *testing.T) {
	r, store := newEscapeTestRuntime(t)
	sess, root := workspaceSession(t, "sess-escape-dir")
	dir := filepath.Join(t.TempDir(), "exports")
	require.NoError(t, os.Mkdir(dir, 0o755))

	parts, sink, req := materializeWithAnswer(t, r, sess, escapeMedia(dir), tools.ElicitationActionAccept, escapeAcceptContent())

	final := filepath.Join(dir, "cat.png")
	assert.Contains(t, req.Message, final, "the user must see the exact file that will be written inside the directory")

	doc := parts[0].Document
	require.NotNil(t, doc)
	assert.Equal(t, final, doc.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootExternal, doc.Source.ArtifactRoot)

	data, err := os.ReadFile(final)
	require.NoError(t, err)
	assert.Equal(t, []byte{0xAA}, data)

	siblings, err := os.ReadDir(filepath.Dir(dir))
	require.NoError(t, err)
	require.Len(t, siblings, 1, "nothing may be written next to the confirmed directory")

	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, final)
	require.NoError(t, err)

	assertSiblingPreserved(t, parts, root)
	assert.Empty(t, sink.warnings())
}

// TestMaterializeGeneratedMedia_EscapeDirectoryTargetGenericName: with no
// provider display name the generic fallback (extension included, so the
// confirmed path is the final path) lands inside the confirmed directory.
func TestMaterializeGeneratedMedia_EscapeDirectoryTargetGenericName(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	sess, _ := workspaceSession(t, "sess-escape-dir-generic")
	dir := filepath.Join(t.TempDir(), "exports")
	require.NoError(t, os.Mkdir(dir, 0o755))

	parts, _, req := materializeWithAnswer(t, r, sess, []chat.MediaDelta{
		{Data: []byte{0xAA}, MimeType: "image/png", RequestedPath: dir, Size: 1},
	}, tools.ElicitationActionAccept, escapeAcceptContent())

	final := filepath.Join(dir, "generated-1.png")
	assert.Contains(t, req.Message, final)
	require.Len(t, parts, 1)
	assert.Equal(t, final, parts[0].Document.Source.ArtifactPath)
	_, err := os.Stat(final)
	require.NoError(t, err)
}

// TestMaterializeGeneratedMedia_EscapeTraversalRequested: a prompt-directed
// "../" path is an escape like any absolute one — the user is asked about
// the resolved absolute target and a decline redirects into the workspace.
func TestMaterializeGeneratedMedia_EscapeTraversalRequested(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	sess, root := workspaceSession(t, "sess-escape-traversal")

	parts, sink, req := materializeWithAnswer(t, r, sess, escapeMedia("../escaped-cat.png"), tools.ElicitationActionDecline, nil)

	resolvedTarget := filepath.Join(filepath.Dir(root), "escaped-cat.png")
	assert.Contains(t, req.Message, resolvedTarget, "the user must see the resolved absolute target, not the raw ../ path")
	_, err := os.Stat(resolvedTarget)
	assert.True(t, os.IsNotExist(err), "nothing may be written outside the workspace on decline")

	require.Len(t, parts, 2)
	assert.Equal(t, "escaped-cat.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootWorkspace, parts[0].Document.Source.ArtifactRoot)
	require.Len(t, sink.warnings(), 1)
	assert.Contains(t, sink.warnings()[0].Message, "outside the workspace")
	assertSiblingPreserved(t, parts, root)
}

// TestMaterializeGeneratedMedia_EscapeTildeRequested: "~/..." expands to
// the real home directory and is treated as an escape; without a user to
// ask it redirects into the workspace and never touches home.
func TestMaterializeGeneratedMedia_EscapeTildeRequested(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	r.nonInteractive = true
	r.OnElicitationRequest(func(e Event) { t.Errorf("no elicitation must be emitted in non-interactive mode, got %T", e) })
	sess, root := workspaceSession(t, "sess-escape-tilde")
	filename := fmt.Sprintf("cagent-test-escape-%d.png", time.Now().UnixNano())

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, escapeMedia("~/"+filename), "root", sink)

	require.Len(t, parts, 2)
	assert.Equal(t, filename, parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootWorkspace, parts[0].Document.Source.ArtifactRoot)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(home, filename))
	assert.True(t, os.IsNotExist(err), "a redirected tilde path must never be written under home")
	require.Len(t, sink.warnings(), 1)
	assert.Contains(t, sink.warnings()[0].Message, "outside the workspace")
	assertSiblingPreserved(t, parts, root)
}

// TestMaterializeGeneratedMedia_SymlinkedParentRedirects: a lexically
// workspace-relative path whose parent directory is a symlink out of the
// workspace escapes at I/O time; the writer refuses it and the bytes are
// redirected into the workspace root like any unconfirmed escape.
func TestMaterializeGeneratedMedia_SymlinkedParentRedirects(t *testing.T) {
	requireSymlinkSupport(t)
	r, store := newEscapeTestRuntime(t)
	r.OnElicitationRequest(func(e Event) { t.Errorf("a symlinked-parent escape must redirect, not elicit, got %T", e) })
	sess, root := workspaceSession(t, "sess-escape-symlink-parent")
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0xAA}, MimeType: "image/png", RequestedPath: "link/cat.png", Size: 1},
	}, "root", sink)

	require.Len(t, parts, 1)
	doc := parts[0].Document
	assert.Equal(t, "cat.png", doc.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootWorkspace, doc.Source.ArtifactRoot)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be written through the symlinked parent")
	data, err := os.ReadFile(filepath.Join(root, "cat.png"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0xAA}, data)
	_, err = manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, "cat.png")
	require.NoError(t, err)
	require.Len(t, sink.warnings(), 1)
	assert.Contains(t, sink.warnings()[0].Message, "escapes the workspace")
}
