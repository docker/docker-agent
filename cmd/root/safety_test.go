package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateSafetyFlag(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "strict", "balanced", "restricted", "autonomous"} {
		t.Run(value, func(t *testing.T) {
			assert.NoError(t, validateSafetyFlag(value))
		})
	}

	err := validateSafetyFlag("unsafe")
	assert.EqualError(t, err, `invalid --safety value "unsafe" (valid: strict, balanced, restricted, autonomous)`)
}
