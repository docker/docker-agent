// Package copilot implements the GitHub Copilot CLI harness adapter for docker-agent.
// It connects to `copilot --acp --stdio` via the ACP (Agent Client Protocol).
package copilot

import (
	"context"

	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/harness/acp"
)

const adapterName = "copilot"

// Adapter implements harness.ACPAdapter for the GitHub Copilot CLI.
type Adapter struct {
	base acp.BaseAdapter
}

func init() {
	harness.Register(&Adapter{
		base: acp.BaseAdapter{
			BinaryName:  "copilot",
			DefaultArgs: []string{"--acp", "--stdio"},
		},
	})
}

// Name returns the harness type identifier.
func (a *Adapter) Name() string { return adapterName }

// Capabilities returns the static capability declaration.
func (a *Adapter) Capabilities() harness.AdapterCapabilities {
	return harness.AdapterCapabilities{
		Protocol: harness.ProtocolACP,
		Requires: harness.HostRequirements{
			ToolExecutor: true,
			Permission:   true,
		},
		Features: harness.AdapterFeatures{
			SystemPrompt:  true,
			Reasoning:     true,
			TextDeltas:    true,
			MultiTurn:     true,
			StreamingArgs: false,
		},
	}
}

// Run implements harness.HarnessAdapter (required for interface compliance).
// ACP adapters should be called via RunACP.
func (a *Adapter) Run(ctx context.Context, req harness.SubSessionRequest) {
	a.base.Run(ctx, req)
}

// RunACP implements harness.ACPAdapter.
func (a *Adapter) RunACP(ctx context.Context, req harness.SubSessionRequest, callbacks harness.ACPCallbacks) {
	a.base.RunACP(ctx, req, callbacks)
}

var _ harness.ACPAdapter = (*Adapter)(nil)
