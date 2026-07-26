package runtime

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// newMediaTestRuntime builds the minimal LocalRuntime materialization needs:
// a session store (for parent-chain WorkingDir lookup). It also confines the
// process data dir to a throwaway temp dir so every test can prove no
// generated file falls back there.
func newMediaTestRuntime(t *testing.T) (*LocalRuntime, session.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	paths.SetDataDir(dataDir)
	t.Cleanup(func() { paths.SetDataDir("") })
	store := session.NewInMemorySessionStore()
	return &LocalRuntime{sessionStore: store}, store, dataDir
}

// workspaceSession returns a session owning a real, writable workspace root.
func workspaceSession(t *testing.T, id string) (*session.Session, string) {
	t.Helper()
	root := t.TempDir()
	return &session.Session{ID: id, WorkingDir: root}, root
}

// assertNoFilesUnder proves the no-data-dir-fallback contract: materialization
// must never create a file under the managed data dir.
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

// TestMaterializeGeneratedMedia_WritesIntoWorkspace is the core contract:
// a generated item lands in the owning session's workspace at the exact
// final relative path the writer returns, the persisted part carries the
// workspace root kind plus that path, and nothing is created under the
// managed data dir (no fallback).
func TestMaterializeGeneratedMedia_WritesIntoWorkspace(t *testing.T) {
	r, _, dataDir := newMediaTestRuntime(t)
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

	assertNoFilesUnder(t, dataDir)
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
	r, _, dataDir := newMediaTestRuntime(t)
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
		assertSafeWarningMessage(t, w.Message, sess.ID, "")
	}

	assertNoFilesUnder(t, dataDir)

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
	assertSafeWarningMessage(t, warnings[0].Message, sess.ID, root)
	assertNoFilesUnder(t, dataDir)
}

// TestMaterializeGeneratedMedia_PartialSuccess_SingleBatchCall: ONE call with
// a two-item batch where exactly one sibling fails (injected through the
// workspacemediaWrite seam) must keep the surviving sibling's file and
// part, and warn only for the failing one.
func TestMaterializeGeneratedMedia_PartialSuccess_SingleBatchCall(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
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
	assertSafeWarningMessage(t, warnings[0].Message, sess.ID, root)

	data, err := os.ReadFile(filepath.Join(root, "cat.png"))
	require.NoError(t, err, "the surviving sibling must actually be readable back from the workspace")
	assert.Equal(t, []byte{0x01}, data)
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
