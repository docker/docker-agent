package v15

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEffectiveCallTimeout(t *testing.T) {
	t.Parallel()

	var nilConfig *LifecycleConfig
	assert.Equal(t, time.Duration(0), nilConfig.EffectiveCallTimeout(), "nil config means no timeout")

	assert.Equal(t, time.Duration(0), (&LifecycleConfig{}).EffectiveCallTimeout(), "zero value means no timeout")

	assert.Equal(t, time.Duration(0), (&LifecycleConfig{Profile: LifecycleProfileStrict}).EffectiveCallTimeout(),
		"call_timeout has no profile default, even under strict")

	assert.Equal(t, 45*time.Second, (&LifecycleConfig{
		CallTimeout: Duration{Duration: 45 * time.Second},
	}).EffectiveCallTimeout(), "an explicit value is returned as-is")
}
