package codingharness

import (
	"errors"
	"fmt"

	baseharness "github.com/rumpl/harness"
	"github.com/rumpl/harness/claudecode"
	"github.com/rumpl/harness/codex"
	"github.com/rumpl/harness/opencode"
	"github.com/rumpl/harness/pi"

	"github.com/docker/docker-agent/pkg/config/latest"
)

const (
	TypeClaudeCode = "claude-code"
	TypeCodex      = "codex"
	TypePi         = "pi"
	TypeOpenCode   = "opencode"
)

func NewProvider(cfg *latest.HarnessConfig) (baseharness.Provider, error) {
	if cfg == nil {
		return nil, errors.New("harness config is nil")
	}

	switch cfg.Type {
	case TypeClaudeCode:
		return newClaudeCodeProvider(cfg), nil
	case TypeCodex:
		return codex.New(cfg.Model), nil
	case TypePi:
		return pi.New(cfg.Model), nil
	case TypeOpenCode:
		return newOpenCodeProvider(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported harness type %q", cfg.Type)
	}
}

func newClaudeCodeProvider(cfg *latest.HarnessConfig) baseharness.Provider {
	var opts []claudecode.Option
	if cfg.Effort != "" {
		opts = append(opts, claudecode.WithEffort(claudecode.Effort(cfg.Effort)))
	}
	return claudecode.New(cfg.Model, opts...)
}

func newOpenCodeProvider(cfg *latest.HarnessConfig) baseharness.Provider {
	var opts []opencode.Option
	if cfg.Agent != "" {
		opts = append(opts, opencode.WithAgent(cfg.Agent))
	}
	if cfg.Thinking {
		opts = append(opts, opencode.WithThinking())
	}
	return opencode.New(cfg.Model, opts...)
}
