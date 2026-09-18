//go:build js

package builtins

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvironmentInfo(t *testing.T) {
	t.Setenv("SHELL", "/opt/sentinel/zsh")

	got := environmentInfo("/")
	assert.Equal(t, `Here is useful information about the environment you are running in:
	<env>
	Working directory: /
	Operating System: js
	CPU Architecture: wasm
	</env>`, got)
	assert.NotContains(t, got, "sentinel", "the process SHELL never reaches the model")
}
