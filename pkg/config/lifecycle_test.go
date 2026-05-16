package config

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// parseToolset is a small helper that runs a YAML through the public
// validating Toolset unmarshal path.
func parseToolset(t *testing.T, in string) latest.Toolset {
	t.Helper()
	var ts latest.Toolset
	require.NoError(t, yaml.Unmarshal([]byte(in), &ts))
	return ts
}

func TestLifecycleConfig_DefaultsAreNil(t *testing.T) {
	t.Parallel()
	ts := parseToolset(t, "type: mcp\ncommand: gopls\n")
	assert.Nil(t, ts.Lifecycle)
}

func TestLifecycleConfig_RoundTrips(t *testing.T) {
	t.Parallel()
	in := `
type: mcp
command: gopls
lifecycle:
  profile: resilient
  required: true
  startup_timeout: 30s
  call_timeout: 60s
  restart: on_failure
  max_restarts: 5
  backoff:
    initial: 1s
    max: 32s
    multiplier: 2
    jitter: 0.2
`
	ts := parseToolset(t, in)
	require.NotNil(t, ts.Lifecycle)
	assert.Equal(t, latest.LifecycleProfileResilient, ts.Lifecycle.Profile)
	assert.True(t, ts.Lifecycle.IsRequired())
	assert.Equal(t, 30*time.Second, ts.Lifecycle.StartupTimeout.Duration)
	assert.Equal(t, 60*time.Second, ts.Lifecycle.CallTimeout.Duration)
	assert.Equal(t, "on_failure", ts.Lifecycle.Restart)
	assert.Equal(t, 5, ts.Lifecycle.MaxRestarts)
	require.NotNil(t, ts.Lifecycle.Backoff)
	assert.Equal(t, time.Second, ts.Lifecycle.Backoff.Initial.Duration)
	assert.Equal(t, 32*time.Second, ts.Lifecycle.Backoff.Max.Duration)
	assert.InDelta(t, 2.0, ts.Lifecycle.Backoff.Multiplier, 0.001)
	assert.InDelta(t, 0.2, ts.Lifecycle.Backoff.Jitter, 0.001)
}

func TestLifecycleConfig_ProfileDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		profile  string
		required bool
	}{
		{latest.LifecycleProfileStrict, true},
		{latest.LifecycleProfileResilient, false},
		{latest.LifecycleProfileBestEffort, false},
		{"", false}, // empty profile defaults to resilient
	}
	for _, tc := range cases {
		var l *latest.LifecycleConfig
		if tc.profile != "" {
			l = &latest.LifecycleConfig{Profile: tc.profile}
		}
		assert.Equal(t, tc.required, l.IsRequired(), "profile=%q", tc.profile)
	}
}

func TestLifecycleConfig_ExplicitRequiredOverridesProfile(t *testing.T) {
	t.Parallel()
	f := false
	l := &latest.LifecycleConfig{Profile: latest.LifecycleProfileStrict, Required: &f}
	assert.False(t, l.IsRequired())
}

func TestLifecycleConfig_RejectsBadProfile(t *testing.T) {
	t.Parallel()
	in := `
type: mcp
command: gopls
lifecycle:
  profile: nope
`
	var ts latest.Toolset
	err := yaml.Unmarshal([]byte(in), &ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profile")
}

func TestLifecycleConfig_RejectsBadRestart(t *testing.T) {
	t.Parallel()
	in := `
type: mcp
command: gopls
lifecycle:
  restart: maybe
`
	var ts latest.Toolset
	err := yaml.Unmarshal([]byte(in), &ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restart")
}

func TestLifecycleConfig_RejectsBadJitter(t *testing.T) {
	t.Parallel()
	in := `
type: mcp
command: gopls
lifecycle:
  backoff:
    jitter: 2
`
	var ts latest.Toolset
	err := yaml.Unmarshal([]byte(in), &ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jitter")
}

func TestLifecycleConfig_RejectsOnNonMcpLsp(t *testing.T) {
	t.Parallel()
	in := `
type: shell
lifecycle:
  profile: strict
`
	var ts latest.Toolset
	err := yaml.Unmarshal([]byte(in), &ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lifecycle")
}
