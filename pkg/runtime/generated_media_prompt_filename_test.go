package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func promptWorkspaceSession(t *testing.T, id, prompt string) (*session.Session, string) {
	t.Helper()
	sess, root := workspaceSession(t, id)
	sess.AddMessage(session.UserMessage(prompt))
	return sess, root
}

// TestUserPromptFilenameNamesSingleUnmarkedBlob is the live-repro
// integration: the exact prompt "Generate an image as sunshine.jpg", a model
// that ignores the marker instruction, and one PNG blob. The prompt filename
// must name the file, with the writer still correcting the extension to the
// actual MIME type and surfacing the correction notice.
func TestUserPromptFilenameNamesSingleUnmarkedBlob(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := promptWorkspaceSession(t, "sess-prompt-name", "Generate an image as sunshine.jpg")

	stream := newStreamBuilder().
		AddContent("Here you go!\n").
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "provider-name.png").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "Here you go!\n")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "sunshine.png", parts[0].Document.Source.ArtifactPath, "the prompt filename must win over the provider name, with the MIME-derived extension")
	_, err := os.Stat(filepath.Join(root, "sunshine.png"))
	require.NoError(t, err)

	warnings := sink.warnings()
	require.Len(t, warnings, 1, "the extension correction must be user-visible")
	assert.Contains(t, warnings[0].Message, "sunshine.png")
	assert.Contains(t, warnings[0].Message, `".jpg"`)
}

// TestUserPromptOfPhraseFilenameMaterializesEndToEnd is the live repro of
// the "of <subject> as <path>" prompt form: a model that ignores the marker
// instruction returns one generic unnamed PNG blob, and the prompt-directed
// subdirectory path must still become the persisted final path — with the
// writer's MIME/extension correction — and the part's Document.Name the
// final basename the UI labels the image with.
func TestUserPromptOfPhraseFilenameMaterializesEndToEnd(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := promptWorkspaceSession(t, "sess-prompt-of-phrase",
		"Generate an image of a red panda coding at a terminal as assets/red-panda-terminal.jpg")

	stream := newStreamBuilder().
		AddContent("Here is your red panda coding at a terminal:\n").
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "Here is your red panda coding at a terminal:\n")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "assets/red-panda-terminal.png", parts[0].Document.Source.ArtifactPath,
		"the prompt-directed path must win over the generic generated-N name, with the MIME-derived extension")
	assert.Equal(t, "red-panda-terminal.png", parts[0].Document.Name,
		"the persisted display name must be the final basename")
	_, err := os.Stat(filepath.Join(root, "assets", "red-panda-terminal.png"))
	require.NoError(t, err)

	warnings := sink.warnings()
	require.Len(t, warnings, 1, "the extension correction must be user-visible")
	assert.Contains(t, warnings[0].Message, "assets/red-panda-terminal.png")
	assert.Contains(t, warnings[0].Message, `".jpg"`)
}

// TestUserPromptOfPhraseFilenameCollisionSuffixPersists: when the
// prompt-directed target already exists, the collision-suffixed path the
// writer actually used is what the part persists — ArtifactPath and
// Document.Name both name the suffixed file, never the requested one.
func TestUserPromptOfPhraseFilenameCollisionSuffixPersists(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := promptWorkspaceSession(t, "sess-prompt-of-phrase-collision",
		"Generate an image of a red panda coding at a terminal as assets/red-panda-terminal.png")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "assets", "red-panda-terminal.png"), []byte{0x01}, 0o644))

	stream := newStreamBuilder().
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "assets/red-panda-terminal-1.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, "red-panda-terminal-1.png", parts[0].Document.Name)
	_, err := os.Stat(filepath.Join(root, "assets", "red-panda-terminal-1.png"))
	require.NoError(t, err)
	assert.Empty(t, sink.warnings(), "a collision suffix alone is not a user-visible correction")
}

// TestOverlappingGrammarPromptsMaterializeEndToEnd pins the end-to-end
// behavior of prompts where the of-phrase grammar overlaps the explicit-cue
// grammar: one occurrence captured by both grammars (conjunction swallowed
// into the subject, or nothing once CR/LF stops the subject) must still
// name the file, while genuinely distinct candidates stay ambiguous and
// keep the generic generated-N name.
func TestOverlappingGrammarPromptsMaterializeEndToEnd(t *testing.T) {
	tests := map[string]struct {
		prompt   string
		wantPath string
	}{
		"of-phrase conjunction":  {"make a logo of a wolf and save it as logo.png", "logo.png"},
		"image-noun conjunction": {"Generate an image of a cat and save it as cat.png", "cat.png"},
		"newline-separated cue":  {"Generate an image of a cat\nSave it as x.png", "x.png"},
		"distinct candidates":    {"Generate an image of a dog as dog.png and save it as cat.png", "generated-1.png"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newMediaTestRuntime(t)
			sess, root := promptWorkspaceSession(t, "sess-prompt-overlap-"+name, tt.prompt)

			stream := newStreamBuilder().
				AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "").
				AddStopWithUsage(1, 1).
				Build()
			media := markerTurnMedia(t, stream, "")

			sink := &collectingSink{}
			parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

			require.Len(t, parts, 1)
			assert.Equal(t, tt.wantPath, parts[0].Document.Source.ArtifactPath)
			assert.Equal(t, tt.wantPath, parts[0].Document.Name)
			_, err := os.Stat(filepath.Join(root, tt.wantPath))
			require.NoError(t, err)
			assert.Empty(t, sink.warnings())
		})
	}
}

// TestMarkerOverridesUserPromptFilename proves precedence: a compliant model
// emitting a marker wins over the explicit filename in the prompt.
func TestMarkerOverridesUserPromptFilename(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := promptWorkspaceSession(t, "sess-prompt-marker", "Generate an image as sunshine.jpg")

	stream := newStreamBuilder().
		AddContent("[media-file: marker-pick.png]\n").
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "marker-pick.png", parts[0].Document.Source.ArtifactPath)
	_, err := os.Stat(filepath.Join(root, "sunshine.png"))
	assert.True(t, os.IsNotExist(err), "the prompt filename must not be used when a marker named the blob")
}

// TestUserPromptFilenameSkipsMultiBlobTurns: one prompt filename cannot
// unambiguously name one of several blobs, so both fall back to the
// provider-then-generic naming unchanged.
func TestUserPromptFilenameSkipsMultiBlobTurns(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := promptWorkspaceSession(t, "sess-prompt-multi", "Generate an image as sunshine.jpg")

	stream := newStreamBuilder().
		AddMultiMedia(
			chat.MediaDelta{Data: []byte{0x01}, MimeType: "image/png", Name: "provider-pick", Size: 1},
			chat.MediaDelta{Data: []byte{0x02}, MimeType: "image/png", Size: 1},
		).
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 2)
	assert.Equal(t, "provider-pick.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, "generated-2.png", parts[1].Document.Source.ArtifactPath)
	_, err := os.Stat(filepath.Join(root, "sunshine.png"))
	assert.True(t, os.IsNotExist(err))
}

// TestUserPromptWithoutExplicitFilenameKeepsProviderFallback: a prompt with
// no explicit filename leaves the existing provider-name fallback untouched.
func TestUserPromptWithoutExplicitFilenameKeepsProviderFallback(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, _ := promptWorkspaceSession(t, "sess-prompt-none", "Draw me a sunny landscape please")

	stream := newStreamBuilder().
		AddMedia([]byte{0x01}, "image/png", "provider-pick").
		AddStopWithUsage(1, 1).
		Build()
	media := markerTurnMedia(t, stream, "")

	sink := &collectingSink{}
	parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

	require.Len(t, parts, 1)
	assert.Equal(t, "provider-pick.png", parts[0].Document.Source.ArtifactPath)
	assert.Empty(t, sink.warnings())
}

// TestUserPromptEscapingFilenameRedirectsThroughEscapePolicy proves an
// extracted traversing filename reaches the existing escape policy
// unchanged: the bytes are redirected
// into the workspace under the sanitized basename.
func TestUserPromptEscapingFilenameRedirectsThroughEscapePolicy(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	r.nonInteractive = true
	sess, root := workspaceSession(t, "sess-prompt-escape")
	sess.AddMessage(session.UserMessage("save to ../outside.png"))

	stream := newStreamBuilder().
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
	assert.True(t, os.IsNotExist(err), "nothing may land outside the workspace")
	require.Len(t, sink.warnings(), 1, "the redirect must be explained to the user")
}

// TestComparativeReferencePromptsNeverNameGeneratedMedia is the
// false-capture regression for prompts that mention a filename only as a
// comparison or reference to an existing file: no extraction, so the blob
// keeps the generic name and the escape policy is never engaged (no
// redirect warning or file under the referenced name).
func TestComparativeReferencePromptsNeverNameGeneratedMedia(t *testing.T) {
	prompts := map[string]string{
		"same style":                 "Generate an image in the same style as sunshine.jpg",
		"same background":            "Make a banner with the same background as assets/bg.png",
		"the one called":             "Generate an image similar to the one called old-render.png",
		"same style traversing":      "Generate an image in the same style as ../sunshine.jpg",
		"exact palette":              "Generate an image with the exact palette as sunshine.jpg",
		"identical colors":           "Generate an image with identical colors as assets/ref.png",
		"exact palette of-phrase":    "Generate an image of a beach with the exact palette as sunshine.jpg",
		"identical colors of-phrase": "Generate an image of a beach with identical colors as assets/ref.png",
		"exact palette traversing":   "Generate an image of a beach with the exact palette as ../sunshine.jpg",
	}

	for name, prompt := range prompts {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newMediaTestRuntime(t)
			r.nonInteractive = true
			sess, root := promptWorkspaceSession(t, "sess-prompt-comparative-"+name, prompt)

			stream := newStreamBuilder().
				AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "").
				AddStopWithUsage(1, 1).
				Build()
			media := markerTurnMedia(t, stream, "")

			sink := &collectingSink{}
			parts := r.materializeGeneratedMedia(t.Context(), sess, media, "root", sink)

			require.Len(t, parts, 1)
			assert.Equal(t, "generated-1.png", parts[0].Document.Source.ArtifactPath, "a comparative filename mention must never name the output")
			assert.Empty(t, sink.warnings(), "the escape policy must never be engaged for a comparative mention")
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Len(t, entries, 1, "only the generically named file may exist in the workspace")
			assert.Equal(t, "generated-1.png", entries[0].Name())
		})
	}
}
