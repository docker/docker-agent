package servesafety

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		cli                    session.SafetyPolicy
		agentYAML, runtimeYAML string
		want                   Resolved
	}{
		{"default", "", "", "", Resolved{session.SafetyPolicyRestricted, SourceDefault}},
		{"runtime", "", "", "balanced", Resolved{session.SafetyPolicyBalanced, SourceRuntimeYAML}},
		{"agent over runtime", "", "strict", "balanced", Resolved{session.SafetyPolicyStrict, SourceAgentYAML}},
		{"CLI over YAML", session.SafetyPolicyAutonomous, "strict", "balanced", Resolved{session.SafetyPolicyAutonomous, SourceCLI}},
		{"CLI aliases normalize", "unsafe", "strict", "balanced", Resolved{session.SafetyPolicyAutonomous, SourceCLI}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Resolve(test.cli, test.agentYAML, test.runtimeYAML)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestResolveRejectsInvalidYAML(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		agent   string
		runtime string
		message string
	}{
		{"agent autonomous", "autonomous", "", "--safety autonomous"},
		{"runtime autonomous", "", "autonomous", "--safety autonomous"},
		{"invalid agent", "unknown", "", `invalid safety value "unknown"`},
		{"invalid runtime", "", "unknown", `invalid safety value "unknown"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Resolve("", test.agent, test.runtime)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestResumeCeiling(t *testing.T) {
	t.Parallel()

	policies := []session.SafetyPolicy{
		session.SafetyPolicyStrict,
		session.SafetyPolicyBalanced,
		session.SafetyPolicyRestricted,
		session.SafetyPolicyAutonomous,
	}
	for _, persisted := range policies {
		for _, serve := range policies {
			t.Run(string(persisted)+"/"+string(serve), func(t *testing.T) {
				assert.Equal(t, session.MinSafetyPolicy(persisted, serve), ResumeCeiling(persisted, serve))
			})
		}
	}

	assert.Equal(t, session.SafetyPolicyRestricted, ResumeCeiling("", session.SafetyPolicyRestricted))
	assert.Equal(t, session.SafetyPolicyAutonomous, ResumeCeiling("unsafe", session.SafetyPolicyAutonomous))
}
