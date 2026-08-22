package base

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMediaFileInstruction(t *testing.T) {
	t.Parallel()

	assert.Contains(t, MediaFileInstruction, "[media-file: relative/path.ext]")
	assert.Contains(t, MediaFileInstruction, "exactly one marker line per generated image")
	assert.Contains(t, MediaFileInstruction, "same order as the images")
	assert.Contains(t, MediaFileInstruction, "specific file name or path")
	assert.Contains(t, MediaFileInstruction, "Never emit a marker line for an image you did not generate")
}

func TestOutputRequestIncompatibleError(t *testing.T) {
	t.Parallel()

	err := &OutputRequestIncompatibleError{Incompatibilities: []OutputIncompatibility{
		OutputIncompatibleTools,
		OutputIncompatibleStructuredOutput,
	}}

	assert.Equal(t,
		"this model is configured for image output (output_capabilities.image) and does not support tools, structured output in the same request; use a separate model or request for that combination",
		err.Error(),
	)
}
