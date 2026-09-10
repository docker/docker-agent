// Example: wire a YAML-loaded agent without JavaScript or global runtime features.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/signal"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/embeddedchat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools/builtin/api/client"
)

//go:embed agent.yaml
var agentYAML []byte

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func run(ctx context.Context) error {
	providers := provider.NewRegistry(map[string]provider.Factory{
		"anthropic": provider.Adapt(anthropic.NewClient),
	})
	chat, err := embeddedchat.New(ctx, embeddedchat.Config{
		AgentSource: config.NewBytesSource("lean.yaml", agentYAML),
		LoadOpts: []teamloader.Opt{
			teamloader.WithProviderRegistry(providers),
			teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{
				"api": client.Creator(teamloader.NewEnvExpander),
			})),
			teamloader.WithStrict(),
		},
		RuntimeOptions: []runtime.Opt{
			runtime.WithProviderRegistry(providers),
			runtime.WithHarnessFactory(nil),
			runtime.WithCommandEvaluatorFactory(nil),
		},
	})
	if err != nil {
		return err
	}
	defer chat.Close()
	events, err := chat.Send(ctx, "What is docker/docker-agent?")
	if err != nil {
		return err
	}
	for event := range events {
		if event.Err != nil {
			return event.Err
		}
		if event.Tool != nil && event.Tool.NeedsConfirmation {
			// This example never grants unreviewed tool calls.
			if err := chat.Confirm(ctx, runtime.ResumeReject("Approval is not configured in this example.")); err != nil {
				return err
			}
		}
		fmt.Print(event.Text)
	}
	return nil
}
