// Package copilot implements the GitHub Copilot CLI harness adapter for docker-agent.
// It connects to `copilot --acp --stdio` via the ACP (Agent Client Protocol).
//
// The adapter satisfies [harness.Provider] with no-op stubs for the
// streaming surface (PrintCommand / ParseStreamLine). ACP adapters must be
// driven via RunACP, which speaks JSON-RPC over stdio rather than emitting
// newline-delimited JSON on stdout.
package copilot

import (
	"context"

	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/harness/acp"
)

const adapterName = "copilot"

// Adapter is a thin wrapper over [acp.BaseAdapter] that registers the
// GitHub Copilot CLI as a harness provider.
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

// PrintCommand implements [harness.Provider]. ACP adapters do not support
// print mode; callers should use RunACP.
func (a *Adapter) PrintCommand(_ string) string { return "" }

// InteractiveArgs implements [harness.Provider]. Returned for completeness
// so a host can launch the CLI directly; actual ACP integration goes
// through RunACP.
func (a *Adapter) InteractiveArgs(_ string) []string {
	return []string{"copilot", "--acp", "--stdio"}
}

// ParseStreamLine implements [harness.Provider]. ACP adapters do not emit
// NDJSON on stdout; events arrive via JSON-RPC and are surfaced by RunACP.
func (a *Adapter) ParseStreamLine(_ string) []harness.Event { return nil }

// RunACP is the real entry point for ACP-based execution.
func (a *Adapter) RunACP(ctx context.Context, req harness.SubSessionRequest, callbacks harness.ACPCallbacks) harness.RunResult {
	return a.base.RunACP(ctx, req, callbacks)
}

var _ harness.Provider = (*Adapter)(nil)
