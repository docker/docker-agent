package artifacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
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

			name, ok := ExtractExplicitOutputFilename(tt.prompt)
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
		ApplyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Equal(t, "sunshine.jpg", media[0].RequestedPath)
	})

	t.Run("a marker-named blob keeps its path", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{RequestedPath: "marker.png"}}
		ApplyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Equal(t, "marker.png", media[0].RequestedPath, "marker precedence must win over the prompt")
	})

	t.Run("multi-blob turns get no fallback", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{}, {}}
		ApplyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Empty(t, media[0].RequestedPath)
		assert.Empty(t, media[1].RequestedPath)
	})

	t.Run("partially marker-named multi-blob turns get no fallback", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{RequestedPath: "named.png"}, {}}
		ApplyUserPromptRequestedPath(media, "Generate an image as sunshine.jpg")
		assert.Empty(t, media[1].RequestedPath)
	})

	t.Run("ambiguous prompt leaves the blob unnamed", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{}}
		ApplyUserPromptRequestedPath(media, "save as a.png or save as b.png")
		assert.Empty(t, media[0].RequestedPath)
	})

	t.Run("comparative reference leaves the blob unnamed", func(t *testing.T) {
		t.Parallel()
		media := []chat.MediaDelta{{}}
		ApplyUserPromptRequestedPath(media, "Generate an image in the same style as sunshine.jpg")
		assert.Empty(t, media[0].RequestedPath)
	})
}

// promptWorkspaceSession is workspaceSession plus the triggering user
// message, so materialization sees the prompt the fallback must parse.
