package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

type toolSet struct {
	tools.ToolSet

	instruction string
}

func (t toolSet) Instructions() string {
	return t.instruction
}

func TestWithEmptyInstructions(t *testing.T) {
	inner := &toolSet{}

	wrapped := WithInstructions(inner, "")

	assert.Same(t, wrapped, inner)
}

func TestWithInstructions_replace(t *testing.T) {
	inner := &toolSet{
		instruction: "Existing instructions",
	}

	wrapped := WithInstructions(inner, "New instructions")

	assert.Equal(t, "New instructions", tools.GetInstructions(wrapped))
}

func TestWithInstructions_add(t *testing.T) {
	inner := &toolSet{
		instruction: "Existing instructions",
	}

	wrapped := WithInstructions(inner, "{ORIGINAL_INSTRUCTIONS}\nMore instructions")

	assert.Equal(t, "Existing instructions\nMore instructions", tools.GetInstructions(wrapped))
}
