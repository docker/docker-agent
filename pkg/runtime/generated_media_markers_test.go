package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// TestParseMediaFileMarkerLine pins the exact marker grammar: whole line,
// lowercase prefix at column zero, closing bracket at end, non-empty path
// with no edge whitespace, no control characters, valid UTF-8, bounded.
func TestParseMediaFileMarkerLine(t *testing.T) {
	t.Parallel()

	atBoundPath := strings.Repeat("a", maxMediaFileMarkerLineBytes-len(mediaFileMarkerPrefix)-len(mediaFileMarkerSuffix))

	tests := []struct {
		name     string
		line     string
		wantPath string
		wantOK   bool
	}{
		{name: "simple", line: "[media-file: cat.png]", wantPath: "cat.png", wantOK: true},
		{name: "nested path with internal space", line: "[media-file: images/my cat.png]", wantPath: "images/my cat.png", wantOK: true},
		{name: "trailing CR from a CRLF line", line: "[media-file: cat.png]\r", wantPath: "cat.png", wantOK: true},
		{name: "bracket inside the path", line: "[media-file: a]b.png]", wantPath: "a]b.png", wantOK: true},
		{name: "traversal is a valid untrusted path", line: "[media-file: ../escape.png]", wantPath: "../escape.png", wantOK: true},
		{name: "absolute is a valid untrusted path", line: "[media-file: /tmp/x.png]", wantPath: "/tmp/x.png", wantOK: true},
		{name: "exactly at the byte bound", line: mediaFileMarkerPrefix + atBoundPath + mediaFileMarkerSuffix, wantPath: atBoundPath, wantOK: true},

		{name: "leading indentation", line: " [media-file: cat.png]"},
		{name: "uppercase prefix", line: "[MEDIA-FILE: cat.png]"},
		{name: "missing space after colon", line: "[media-file:cat.png]"},
		{name: "trailing text after bracket", line: "[media-file: cat.png] extra"},
		{name: "backtick-quoted", line: "`[media-file: cat.png]`"},
		{name: "empty path", line: "[media-file:]"},
		{name: "whitespace-only path", line: "[media-file: ]"},
		{name: "trailing space in path", line: "[media-file: cat.png ]"},
		{name: "leading space in path", line: "[media-file:  cat.png]"},
		{name: "missing closing bracket", line: "[media-file: cat.png"},
		{name: "control character in path", line: "[media-file: a\tb.png]"},
		{name: "invalid UTF-8 in path", line: "[media-file: \xff.png]"},
		{name: "over the byte bound", line: mediaFileMarkerPrefix + atBoundPath + "a" + mediaFileMarkerSuffix},
		{name: "bare prefix only", line: "[media-file: "},
		{name: "empty line", line: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path, ok := parseMediaFileMarkerLine(tt.line)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantPath, path)
		})
	}
}

// runMarkerFilter feeds input to a fresh filter in chunks of chunkSize bytes
// and returns the concatenated visible text (including the Finish tail) and
// the extracted paths.
func runMarkerFilter(input string, chunkSize int) (string, []string) {
	var f mediaFileMarkerFilter
	var out strings.Builder
	for start := 0; start < len(input); start += chunkSize {
		end := min(start+chunkSize, len(input))
		out.WriteString(f.Push(input[start:end]))
	}
	out.WriteString(f.Finish())
	return out.String(), f.paths
}

// TestMediaFileMarkerFilter pins stripping and extraction on whole inputs,
// then proves chunk-split invariance: every chunk size from a single byte
// upward must yield byte-identical visible text and identical paths.
func TestMediaFileMarkerFilter(t *testing.T) {
	t.Parallel()

	overBoundLine := mediaFileMarkerPrefix + strings.Repeat("a", maxMediaFileMarkerLineBytes) + mediaFileMarkerSuffix

	tests := []struct {
		name      string
		input     string
		wantText  string
		wantPaths []string
	}{
		{name: "marker alone with newline", input: "[media-file: cat.png]\n", wantText: "", wantPaths: []string{"cat.png"}},
		{name: "marker terminated by end of stream", input: "[media-file: cat.png]", wantText: "", wantPaths: []string{"cat.png"}},
		{name: "CRLF marker", input: "[media-file: cat.png]\r\n", wantText: "", wantPaths: []string{"cat.png"}},
		{name: "marker between prose lines", input: "before\n[media-file: a.png]\nafter", wantText: "before\nafter", wantPaths: []string{"a.png"}},
		{name: "multiple markers keep response order", input: "one\n[media-file: first.png]\n[media-file: second.png]\ntwo\n", wantText: "one\ntwo\n", wantPaths: []string{"first.png", "second.png"}},
		{name: "indented lookalike stays visible", input: " [media-file: cat.png]\n", wantText: " [media-file: cat.png]\n"},
		{name: "suffixed lookalike stays visible", input: "[media-file: cat.png] done\n", wantText: "[media-file: cat.png] done\n"},
		{name: "uppercase lookalike stays visible", input: "[MEDIA-FILE: cat.png]\n", wantText: "[MEDIA-FILE: cat.png]\n"},
		{name: "empty-path lookalike stays visible", input: "[media-file:]\n", wantText: "[media-file:]\n"},
		{name: "control character diverges to text", input: "[media-file: a\tb.png]\n", wantText: "[media-file: a\tb.png]\n"},
		{name: "over-bound line streams as text", input: overBoundLine + "\n", wantText: overBoundLine + "\n"},
		{name: "unterminated non-marker tail is flushed", input: "trailing [media", wantText: "trailing [media"},
		{name: "unterminated prefix-only tail is flushed", input: "[media-file: half", wantText: "[media-file: half"},
		{name: "blank lines survive", input: "a\n\n[media-file: x.png]\n\nb\n", wantText: "a\n\n\nb\n", wantPaths: []string{"x.png"}},
		{name: "CRLF prose preserved byte-for-byte", input: "line one\r\nline two\r\n", wantText: "line one\r\nline two\r\n"},
		{name: "plain prose untouched", input: "no markers here, just text.", wantText: "no markers here, just text."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for chunkSize := 1; chunkSize <= max(len(tt.input), 1); chunkSize++ {
				text, paths := runMarkerFilter(tt.input, chunkSize)
				require.Equal(t, tt.wantText, text, "visible text must be split-invariant (chunk size %d)", chunkSize)
				require.Equal(t, tt.wantPaths, paths, "extracted paths must be split-invariant (chunk size %d)", chunkSize)
			}
		})
	}
}

// TestApplyMediaFileRequestedPaths pins positional pairing: marker i names
// blob i (overriding any pre-set requested path), unpaired blobs keep their
// existing value, extra markers are ignored.
func TestApplyMediaFileRequestedPaths(t *testing.T) {
	t.Parallel()

	t.Run("fewer markers than blobs", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{Name: "a"}, {Name: "b", RequestedPath: "pre-set.png"}}
		applyMediaFileRequestedPaths(media, []string{"named.png"})
		assert.Equal(t, "named.png", media[0].RequestedPath)
		assert.Equal(t, "pre-set.png", media[1].RequestedPath, "an unpaired blob keeps its existing requested path")
	})

	t.Run("more markers than blobs", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{Name: "a"}}
		applyMediaFileRequestedPaths(media, []string{"one.png", "two.png"})
		assert.Equal(t, "one.png", media[0].RequestedPath)
	})

	t.Run("marker overrides a pre-set path", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{RequestedPath: "provider.png"}}
		applyMediaFileRequestedPaths(media, []string{"marker.png"})
		assert.Equal(t, "marker.png", media[0].RequestedPath)
	})

	t.Run("no markers is a no-op", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{RequestedPath: "keep.png"}}
		applyMediaFileRequestedPaths(media, nil)
		assert.Equal(t, "keep.png", media[0].RequestedPath)
	})
}

// markerTurnMedia runs handleStream over a marker-bearing stream and returns
// the aggregated media, ready for materialization. It also proves the
// persisted text is marker-free.
func markerTurnMedia(t *testing.T, stream *mockStream, wantContent string) []chat.MediaDelta {
	t.Helper()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))
	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)
	assert.Equal(t, wantContent, res.Content, "the persisted text must carry no marker line")
	return res.Media
}

// TestMarkerNamedMediaMaterializesEndToEnd is the full naming-protocol
// integration: a streamed "[media-file: sunshine.jpg]" marker names the PNG
// blob, the writer corrects the extension to the actual MIME type, the
// correction notice is visible, and the persisted part references the final
// workspace path.
func TestMarkerNamedMediaMaterializesEndToEnd(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-marker-e2e")

	stream := newStreamBuilder().
		AddContent("Here you go!\n[media-file: sunshine.jpg]\n").
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "provider-name.png").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "Here you go!\n")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "sunshine.png", parts[0].Document.Source.ArtifactPath, "the marker name must win over the provider name, with the MIME-derived extension")
	_, err := os.Stat(filepath.Join(root, "sunshine.png"))
	require.NoError(t, err)

	warnings := sink.warnings()
	require.Len(t, warnings, 1, "the extension correction must be user-visible")
	assert.Contains(t, warnings[0].Message, "sunshine.png")
	assert.Contains(t, warnings[0].Message, `".jpg"`)
}

// TestMarkerEscapedPathRedirectsThroughEscapePolicy proves a traversing
// marker path reaches the existing escape policy unchanged: with no user to
// ask (non-interactive), the bytes are redirected into the workspace under
// the sanitized basename instead of being written outside or discarded.
func TestMarkerEscapedPathRedirectsThroughEscapePolicy(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
	r.nonInteractive = true
	sess, root := workspaceSession(t, "sess-marker-escape")

	stream := newStreamBuilder().
		AddContent("[media-file: ../outside.png]\n").
		AddMedia([]byte{0xAA}, "image/png", "").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "outside.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, chat.ArtifactRootWorkspace, parts[0].Document.Source.ArtifactRoot)
	_, err := os.Stat(filepath.Join(root, "outside.png"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(filepath.Dir(root), "outside.png"))
	assert.True(t, os.IsNotExist(err), "nothing may land outside the workspace without confirmation")
	require.Len(t, sink.warnings(), 1, "the redirect must be explained to the user")
}

// TestUnmarkedBlobFallsBackToProviderThenGenericName proves the precedence
// tail end to end: in a two-blob turn with one marker, the second blob still
// materializes under its provider display name, and with neither marker nor
// provider name the generic generated-N name is used.
func TestUnmarkedBlobFallsBackToProviderThenGenericName(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-marker-fallback")

	stream := newStreamBuilder().
		AddContent("[media-file: named.png]\n").
		AddMultiMedia(
			chat.MediaDelta{Data: []byte{0x01}, MimeType: "image/png", Name: "first", Size: 1},
			chat.MediaDelta{Data: []byte{0x02}, MimeType: "image/png", Name: "provider-pick", Size: 1},
			chat.MediaDelta{Data: []byte{0x03}, MimeType: "image/png", Size: 1},
		).
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 3, "every returned blob must materialize, marker or not")
	assert.Empty(t, sink.warnings())
	assert.Equal(t, "named.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, "provider-pick.png", parts[1].Document.Source.ArtifactPath)
	assert.Equal(t, "generated-3.png", parts[2].Document.Source.ArtifactPath)
	for _, name := range []string{"named.png", "provider-pick.png", "generated-3.png"} {
		_, err := os.Stat(filepath.Join(root, name))
		require.NoError(t, err)
	}
}
