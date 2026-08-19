package servesafety

import (
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/session"
)

type Source string

const (
	SourceCLI         Source = "command line"
	SourceAgentYAML   Source = "agent configuration"
	SourceRuntimeYAML Source = "runtime configuration"
	SourceDefault     Source = "serve default"
)

type Resolved struct {
	Policy session.SafetyPolicy
	Source Source
}

// Resolve selects a serve safety policy without consulting local user settings.
func Resolve(cli session.SafetyPolicy, agentYAML, runtimeYAML string) (Resolved, error) {
	if cli != "" {
		policy := cli.Normalize()
		if policy != session.SafetyPolicyAutonomous && policy != session.SafetyPolicyStrict && policy != session.SafetyPolicyBalanced && policy != session.SafetyPolicyRestricted {
			return Resolved{}, fmt.Errorf("invalid safety value %q (valid: strict, balanced, restricted, autonomous)", cli)
		}
		return Resolved{Policy: policy, Source: SourceCLI}, nil
	}
	if agentYAML != "" {
		policy, err := yamlPolicy(agentYAML)
		if err != nil {
			return Resolved{}, fmt.Errorf("agent safety: %w", err)
		}
		return Resolved{Policy: policy, Source: SourceAgentYAML}, nil
	}
	if runtimeYAML != "" {
		policy, err := yamlPolicy(runtimeYAML)
		if err != nil {
			return Resolved{}, fmt.Errorf("runtime safety: %w", err)
		}
		return Resolved{Policy: policy, Source: SourceRuntimeYAML}, nil
	}
	return Resolved{Policy: session.SafetyPolicyRestricted, Source: SourceDefault}, nil
}

func yamlPolicy(value string) (session.SafetyPolicy, error) {
	policy := session.SafetyPolicy(value)
	switch policy {
	case session.SafetyPolicyStrict, session.SafetyPolicyBalanced, session.SafetyPolicyRestricted:
		return policy, nil
	case session.SafetyPolicyAutonomous:
		return "", errors.New("autonomous safety in configuration is not supported; use --safety autonomous to opt in")
	default:
		return "", fmt.Errorf("invalid safety value %q (valid: strict, balanced, restricted)", value)
	}
}

// ResumeCeiling caps an existing session at the server's resolved policy.
// An empty persisted policy is a legacy unset state and adopts the server policy.
func ResumeCeiling(persisted, serve session.SafetyPolicy) session.SafetyPolicy {
	persisted = persisted.Normalize()
	if persisted == "" {
		return serve.Normalize()
	}
	return session.MinSafetyPolicy(persisted, serve)
}
