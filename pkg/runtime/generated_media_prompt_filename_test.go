package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// TestExtractExplicitOutputFilename pins the strict explicit-naming grammar:
// a known cue phrase followed by one quoted/backticked/unquoted filename
// with a known image extension, exactly one candidate, bounded and
// control-character/UTF-8 validated. Bare "as" only counts right after a
// generation verb plus media noun; comparative references to existing files
// ("same style as X", "the one called X"), ordinary input-file mentions,
// and ambiguous prompts must extract nothing.
func TestExtractExplicitOutputFilename(t *testing.T) {
	t.Parallel()

	atBoundName := strings.Repeat("a", maxExplicitOutputFilenameBytes-len(".png")) + ".png"

	tests := []struct {
		name     string
		prompt   string
		wantName string
		wantOK   bool
	}{
		{name: "live repro prompt", prompt: "Generate an image as sunshine.jpg", wantName: "sunshine.jpg", wantOK: true},
		{name: "of-phrase live repro prompt", prompt: "Generate an image of a red panda coding at a terminal as assets/red-panda-terminal.jpg", wantName: "assets/red-panda-terminal.jpg", wantOK: true},
		{name: "of-phrase with quoted name", prompt: "draw a picture of my dog as `pets/rex holiday.png`", wantName: "pets/rex holiday.png", wantOK: true},
		{name: "of-phrase across a sentence boundary is not a second candidate", prompt: "Generate an image of a cat. Save it as x.png", wantName: "x.png", wantOK: true},
		{name: "of-phrase across a newline is not a second candidate", prompt: "Generate an image of a cat\nSave it as x.png", wantName: "x.png", wantOK: true},
		{name: "of-phrase across a CRLF is not a second candidate", prompt: "Generate an image of a cat\r\nSave it as x.png", wantName: "x.png", wantOK: true},
		{name: "of-phrase conjunction into a save-it-as cue dedupes to one candidate", prompt: "make a logo of a wolf and save it as logo.png", wantName: "logo.png", wantOK: true},
		{name: "image-noun conjunction into a save-it-as cue dedupes to one candidate", prompt: "Generate an image of a cat and save it as cat.png", wantName: "cat.png", wantOK: true},
		{name: "save as", prompt: "please save as cat.png", wantName: "cat.png", wantOK: true},
		{name: "save it as", prompt: "make a logo and save it as logo.png", wantName: "logo.png", wantOK: true},
		{name: "name it", prompt: "name it banner.webp", wantName: "banner.webp", wantOK: true},
		{name: "call it", prompt: "call it pic.jpeg", wantName: "pic.jpeg", wantOK: true},
		{name: "verb anchor with 'the'", prompt: "create the logo as logo.png", wantName: "logo.png", wantOK: true},
		{name: "verb anchor without article", prompt: "regenerate image as fixed.png", wantName: "fixed.png", wantOK: true},
		{name: "verb anchor with quoted name", prompt: `draw a picture as "my sun.png"`, wantName: "my sun.png", wantOK: true},
		{name: "save to subdir", prompt: "save to images/out.png", wantName: "images/out.png", wantOK: true},
		{name: "write to", prompt: "write to pics/x.png", wantName: "pics/x.png", wantOK: true},
		{name: "output to", prompt: "output to result.png", wantName: "result.png", wantOK: true},
		{name: "filename colon", prompt: "filename: sunset.png", wantName: "sunset.png", wantOK: true},
		{name: "filename equals", prompt: "filename=sunset.png", wantName: "sunset.png", wantOK: true},
		{name: "filename spaced equals", prompt: "filename = sunset.png", wantName: "sunset.png", wantOK: true},
		{name: "double-quoted with space", prompt: `save it as "my picture.png"`, wantName: "my picture.png", wantOK: true},
		{name: "single-quoted", prompt: "name it 'logo.webp'", wantName: "logo.webp", wantOK: true},
		{name: "backticked subdir", prompt: "save it as `pics/cat.png`", wantName: "pics/cat.png", wantOK: true},
		{name: "trailing sentence punctuation", prompt: "save it as sunshine.jpg.", wantName: "sunshine.jpg", wantOK: true},
		{name: "trailing comma", prompt: "make an image as sunshine.jpg, please", wantName: "sunshine.jpg", wantOK: true},
		{name: "uppercase cue and extension", prompt: "Save As PHOTO.JPG", wantName: "PHOTO.JPG", wantOK: true},
		{name: "traversal is a valid untrusted candidate", prompt: "save to ../evil.png", wantName: "../evil.png", wantOK: true},
		{name: "absolute is a valid untrusted candidate", prompt: "write to /tmp/out.png", wantName: "/tmp/out.png", wantOK: true},
		{name: "exactly at the byte bound", prompt: "save as " + atBoundName, wantName: atBoundName, wantOK: true},

		{name: "input-file mention is not a cue", prompt: "add a border to photo.jpg"},
		{name: "comparative 'same style as' is a reference, not output intent", prompt: "Generate an image in the same style as sunshine.jpg"},
		{name: "comparative inside an of-phrase is a reference, not output intent", prompt: "Generate an image of a cat in the same style as old-render.png"},
		{name: "'like' inside an of-phrase is a reference, not output intent", prompt: "Generate an image of a panda just like my avatar as panda.png"},
		{name: "of-phrase without an 'as' cue", prompt: "Generate an image of assets/red-panda-terminal.jpg"},
		{name: "comparative 'same background as' is a reference, not output intent", prompt: "Make a banner with the same background as assets/bg.png"},
		{name: "comparative 'exact palette as' is a reference, not output intent", prompt: "Generate an image with the exact palette as sunshine.jpg"},
		{name: "comparative 'identical colors as' is a reference, not output intent", prompt: "Generate an image with identical colors as assets/ref.png"},
		{name: "'exact palette' inside an of-phrase is a reference, not output intent", prompt: "Generate an image of a beach with the exact palette as sunshine.jpg"},
		{name: "'identical colors' inside an of-phrase is a reference, not output intent", prompt: "Generate an image of a beach with identical colors as assets/ref.png"},
		{name: "comparative 'as tall as' inside an of-phrase is a reference, not output intent", prompt: "Generate an image of a tower as tall as tree.png"},
		{name: "attribute prepositions in an of-phrase subject are conservatively rejected", prompt: "Generate an image of a cat in a hat as cat-hat.png"},
		{name: "'the one called' references an existing file", prompt: "Generate an image similar to the one called old-render.png"},
		{name: "'called' without imperative output context", prompt: "an image called loop.gif"},
		{name: "bare 'as' without a generation-verb anchor", prompt: "as sunshine.jpg, please"},
		{name: "comparative 'as wide as' after a media noun", prompt: "make the image as wide as banner.png"},
		{name: "plural noun breaks the verb anchor", prompt: "generate images such as sunset.png"},
		{name: "plain filename mention without cue", prompt: "here is photo.jpg"},
		{name: "no filename at all", prompt: "generate an image of a sunset"},
		{name: "prose 'as' without a filename", prompt: "make it as good as new"},
		{name: "cue with non-image extension", prompt: `save as "notes.txt"`},
		{name: "cue with bare word", prompt: "call it Bob"},
		{name: "extension only", prompt: "save it as .png"},
		{name: "quoted extension only", prompt: `call it ".png"`},
		{name: "two candidates are ambiguous", prompt: "save it as a.png and call it b.png"},
		{name: "duplicate candidates are still ambiguous", prompt: "save as x.png, yes save as x.png"},
		{name: "distinct of-phrase and cue candidates are ambiguous", prompt: "Generate an image of a dog as dog.png and save it as cat.png"},
		{name: "same name at distinct occurrences is still ambiguous", prompt: "make an image of a sun as sun.png and save it as sun.png"},
		{name: "control character in name", prompt: "save as bad\x01name.png"},
		{name: "invalid UTF-8 in name", prompt: "save as \xff.png"},
		{name: "over the byte bound", prompt: "save as " + strings.Repeat("a", maxExplicitOutputFilenameBytes) + ".png"},
		{name: "empty prompt", prompt: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, ok := extractExplicitOutputFilename(tt.prompt)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

// TestApplyUserPromptRequestedPath pins the fallback's guard conditions:
// exactly one blob, still unnamed after marker pairing, unambiguous prompt.
func TestApplyUserPromptRequestedPath(t *testing.T) {
	t.Parallel()

	t.Run("names the single unnamed blob", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{Name: "provider"}}
		applyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Equal(t, "sunshine.jpg", media[0].RequestedPath)
	})

	t.Run("a marker-named blob keeps its path", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{RequestedPath: "marker.png"}}
		applyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Equal(t, "marker.png", media[0].RequestedPath, "marker precedence must win over the prompt")
	})

	t.Run("multi-blob turns get no fallback", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{}, {}}
		applyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Empty(t, media[0].RequestedPath)
		assert.Empty(t, media[1].RequestedPath)
	})

	t.Run("partially marker-named multi-blob turns get no fallback", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{RequestedPath: "named.png"}, {}}
		applyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Empty(t, media[1].RequestedPath)
	})

	t.Run("ambiguous prompt leaves the blob unnamed", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{}}
		applyUserPromptRequestedPath(media, "save as a.png or save as b.png")
		assert.Empty(t, media[0].RequestedPath)
	})

	t.Run("comparative reference leaves the blob unnamed", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{}}
		applyUserPromptRequestedPath(media, "Generate an image in the same style as sunshine.jpg")
		assert.Empty(t, media[0].RequestedPath)
	})
}

// promptWorkspaceSession is workspaceSession plus the triggering user
// message, so materialization sees the prompt the fallback must parse.
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
// unchanged: with no user to ask (non-interactive), the bytes are redirected
// into the workspace under the sanitized basename.
func TestUserPromptEscapingFilenameRedirectsThroughEscapePolicy(t *testing.T) {
	r, _ := newEscapeTestRuntime(t)
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
	assert.True(t, os.IsNotExist(err), "nothing may land outside the workspace without confirmation")
	require.Len(t, sink.warnings(), 1, "the redirect must be explained to the user")
}

// TestComparativeReferencePromptsNeverNameGeneratedMedia is the
// false-capture regression for prompts that mention a filename only as a
// comparison or reference to an existing file: no extraction, so the blob
// keeps the generic name and the escape policy is never engaged (no
// confirmation, no redirect warning, no file under the referenced name).
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
			r, _ := newEscapeTestRuntime(t)
			// Non-interactive: a falsely extracted escaping path would surface
			// as a redirect warning instead of deadlocking on a confirmation.
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
