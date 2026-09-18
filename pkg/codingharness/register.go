package codingharness

import (
	"context"

	baseharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

// Factory builds the runtime driver for a `harness:` configuration using this
// package's Claude Code, Codex, Pi and OpenCode implementations. Register it
// with runtime.RegisterHarness(codingharness.Factory); the CLI and
// loaderdefaults.Opts do so, embedders that build teams in code and use
// harness agents must do it themselves.
func Factory(cfg *latest.HarnessConfig) (harness.Provider, error) {
	p, err := NewProvider(cfg)
	if err != nil {
		return nil, err
	}
	return provider{p}, nil
}

// provider adapts a rumpl/harness Provider to harness.Provider.
type provider struct {
	baseharness.Provider
}

func (p provider) Run(ctx context.Context, prompt string, handle func(harness.Event)) error {
	return baseharness.Run(ctx, p.Provider, prompt, adapt(handle))
}

func (p provider) Resume(ctx context.Context, sessionID, prompt string, handle func(harness.Event)) error {
	return baseharness.Resume(ctx, p.Provider, sessionID, prompt, adapt(handle))
}

func adapt(handle func(harness.Event)) func(baseharness.Event) {
	return func(ev baseharness.Event) {
		out := harness.Event{
			Type:       harness.EventType(ev.Type),
			Text:       ev.Text,
			Result:     ev.Result,
			SessionID:  ev.SessionID,
			ToolID:     ev.ToolID,
			ToolName:   ev.ToolName,
			ToolArgs:   ev.ToolArgs,
			ToolOutput: ev.ToolOutput,
			ToolError:  ev.ToolError,
			Reasoning:  ev.Reasoning,
		}
		if ev.Usage != nil {
			out.Usage = &harness.Usage{
				InputTokens:              ev.Usage.InputTokens,
				OutputTokens:             ev.Usage.OutputTokens,
				CacheReadInputTokens:     ev.Usage.CacheReadInputTokens,
				CacheCreationInputTokens: ev.Usage.CacheCreationInputTokens,
				TotalCostUSD:             ev.Usage.TotalCostUSD,
			}
		}
		handle(out)
	}
}
