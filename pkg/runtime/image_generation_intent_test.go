package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasExplicitImageGenerationIntent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		prompt string
		want   bool
	}{
		{name: "generate image", prompt: "Generate an image of Docker and friends", want: true},
		{name: "draw picture", prompt: "draw a picture of a red panda", want: true},
		{name: "render logo", prompt: "Please render the logo in watercolor", want: true},
		{name: "create filename", prompt: "Create an image as assets/sunshine.png", want: true},
		{name: "regenerate thumbnail", prompt: "Regenerate the thumbnail", want: true},
		{name: "ordinary text", prompt: "Explain how image generation works"},
		{name: "capability question", prompt: "Can you generate images?"},
		{name: "referenced input", prompt: "Describe the image called old-render.png"},
		{name: "non-imperative noun", prompt: "The generated image looks good"},
		{name: "negated phrase still matches", prompt: "do not generate an image", want: true},
		{name: "singular capability question matches", prompt: "can you generate an image?", want: true},
		{name: "contextual phrase matches", prompt: "explain why the instruction says create an image", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, hasExplicitImageGenerationIntent(tc.prompt))
		})
	}
}
