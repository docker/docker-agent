package environment

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInSandbox(t *testing.T) {
	t.Run("outside sandbox", func(t *testing.T) {
		t.Setenv("SANDBOX_VM_ID", "")
		assert.False(t, InSandbox())
	})
	t.Run("inside sandbox", func(t *testing.T) {
		t.Setenv("SANDBOX_VM_ID", "some-uuid")
		assert.True(t, InSandbox())
	})
}
