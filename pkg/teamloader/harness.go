package teamloader

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// defaultBinaryForType returns the default binary name for a harness type.
func defaultBinaryForType(harnessType string) string {
	switch harnessType {
	case "claude-code":
		return "claude"
	case "codex":
		return "codex"
	case "opencode":
		return "opencode"
	case "copilot":
		return "copilot"
	case "openclaw":
		return "openclaw"
	default:
		return harnessType
	}
}

// installHintForType returns a human-readable install hint for a harness type.
func installHintForType(harnessType string) string {
	switch harnessType {
	case "claude-code":
		return "npm install -g @anthropic-ai/claude-code"
	case "codex":
		return "npm install -g @openai/codex"
	case "opencode":
		return "npm install -g opencode-ai"
	case "copilot":
		return "npm install -g @github/copilot-cli"
	case "openclaw":
		return "npm install -g openclaw"
	default:
		return "check the harness documentation for installation instructions"
	}
}

// buildHarnessSpec converts a config HarnessConfig to an agent.HarnessSpec,
// verifying the harness binary is available on PATH.
func buildHarnessSpec(cfg *latest.HarnessConfig) (*agent.HarnessSpec, error) {
	binary := cfg.Command
	if binary == "" {
		binary = defaultBinaryForType(cfg.Type)
	}

	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf(
			"harness binary %q not found on PATH for harness type %q\n"+
				"  install with: %s",
			binary, cfg.Type, installHintForType(cfg.Type),
		)
	}

	var policy *agent.PermissionPolicy
	if cfg.PermissionPolicy != nil {
		policy = &agent.PermissionPolicy{
			Mode: agent.PermissionMode(cfg.PermissionPolicy.Mode),
		}
	}

	timeout := cfg.Timeout.Duration
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	return &agent.HarnessSpec{
		Type:             cfg.Type,
		Command:          binary,
		Args:             cfg.Args,
		Env:              cfg.Env,
		WorkingDir:       cfg.WorkingDir,
		Timeout:          timeout,
		Config:           cfg.Config,
		PermissionPolicy: policy,
	}, nil
}
