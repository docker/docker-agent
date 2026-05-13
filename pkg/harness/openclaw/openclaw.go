// Package openclaw implements the OpenClaw harness adapter for docker-agent.
// It connects to `openclaw acp` via the ACP (Agent Client Protocol).
package openclaw

import (
	"context"

	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/harness/acp"
)

const adapterName = "openclaw"

// Adapter implements harness.ACPAdapter for OpenClaw.
type Adapter struct {
	base acp.BaseAdapter
}

func init() {
	harness.Register(&Adapter{
		base: acp.BaseAdapter{
			BinaryName:  "openclaw",
			DefaultArgs: []string{"acp"},
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
func (a *Adapter) Run(ctx context.Context, req harness.SubSessionRequest) {
	a.base.Run(ctx, req)
}

// RunACP implements harness.ACPAdapter.
func (a *Adapter) RunACP(ctx context.Context, req harness.SubSessionRequest, callbacks harness.ACPCallbacks) {
	a.base.RunACP(ctx, req, callbacks)
}

var _ harness.ACPAdapter = (*Adapter)(nil)
