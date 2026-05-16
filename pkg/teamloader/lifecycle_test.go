package teamloader

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestLifecyclePolicyFromConfig_NilUsesResilientDefaults(t *testing.T) {
	t.Parallel()
	p := lifecyclePolicyFromConfig("test", nil)
	assert.Equal(t, lifecycle.RestartOnFailure, p.Restart)
	assert.Equal(t, 5, p.MaxAttempts)
	assert.NotNil(t, p.Logger)
}

func TestLifecyclePolicyFromConfig_StrictProfile(t *testing.T) {
	t.Parallel()
	p := lifecyclePolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileStrict,
	})
	assert.Equal(t, lifecycle.RestartNever, p.Restart)
	assert.Equal(t, -1, p.MaxAttempts)
}

func TestLifecyclePolicyFromConfig_BestEffortProfile(t *testing.T) {
	t.Parallel()
	p := lifecyclePolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileBestEffort,
	})
	assert.Equal(t, lifecycle.RestartNever, p.Restart)
}

func TestLifecyclePolicyFromConfig_ExplicitOverrides(t *testing.T) {
	t.Parallel()
	cfg := &latest.LifecycleConfig{
		Profile:     latest.LifecycleProfileResilient,
		Restart:     "always",
		MaxRestarts: 12,
		Backoff: &latest.BackoffConfig{
			Initial:    latest.Duration{Duration: 500 * time.Millisecond},
			Max:        latest.Duration{Duration: 10 * time.Second},
			Multiplier: 1.5,
			Jitter:     0.3,
		},
	}
	p := lifecyclePolicyFromConfig("test", cfg)
	assert.Equal(t, lifecycle.RestartAlways, p.Restart)
	assert.Equal(t, 12, p.MaxAttempts)
	assert.Equal(t, 500*time.Millisecond, p.Backoff.Initial)
	assert.Equal(t, 10*time.Second, p.Backoff.Max)
	assert.InDelta(t, 1.5, p.Backoff.Multiplier, 0.001)
	assert.InDelta(t, 0.3, p.Backoff.Jitter, 0.001)
}

func TestLifecyclePolicyFromConfig_PartialOverridesKeepProfileDefaults(t *testing.T) {
	t.Parallel()
	cfg := &latest.LifecycleConfig{
		Profile:     latest.LifecycleProfileResilient,
		MaxRestarts: 7,
	}
	p := lifecyclePolicyFromConfig("test", cfg)
	assert.Equal(t, lifecycle.RestartOnFailure, p.Restart, "profile default preserved")
	assert.Equal(t, 7, p.MaxAttempts, "explicit override applied")
}

func TestParseRestart(t *testing.T) {
	t.Parallel()
	cases := map[string]lifecycle.Restart{
		"":           lifecycle.RestartOnFailure,
		"on_failure": lifecycle.RestartOnFailure,
		"never":      lifecycle.RestartNever,
		"always":     lifecycle.RestartAlways,
	}
	for in, want := range cases {
		assert.Equal(t, want, parseRestart(in), "input=%q", in)
	}
}
