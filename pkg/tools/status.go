package tools

import (
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// Kinder is implemented by ToolSets that can produce a short,
// user-friendly classification of themselves (e.g. "MCP", "Remote MCP",
// "LSP"). The string is meant for human display in surfaces like the
// /tools dialog. Toolsets that do not implement Kinder are surfaced
// without a Kind, which the renderer treats as "Built-in".
type Kinder interface {
	Kind() string
}

// ToolsetStatus is a snapshot of a single toolset's lifecycle, suitable for
// status surfaces (TUI /tools dialog, JSON status endpoints, logs).
//
// The fields are intentionally flat and self-describing so they can be
// rendered without the renderer needing to import lifecycle.
type ToolsetStatus struct {
	// Name is the toolset name as configured in the agent YAML (or a
	// derived label when the YAML has no name).
	Name string
	// Kind is a short, user-friendly label such as "MCP", "Remote MCP"
	// or "LSP". Empty when the toolset does not implement Kinder; the
	// TUI renders that case as "Built-in".
	Kind string
	// Description is the user-visible Describer.Describe() output, never
	// containing secrets. Empty when the toolset does not implement
	// Describer. Kept for non-TUI surfaces (logs, JSON status); the TUI
	// uses Name + Kind instead because Description tends to leak Go
	// implementation detail ("mcp(stdio cmd=docker)").
	Description string
	// State is the lifecycle state. For toolsets that don't implement
	// Statable, the runtime sets it to StateReady when the toolset has a
	// usable tool list and StateStopped otherwise — matching what the
	// user actually observes.
	State lifecycle.State
	// LastError is the most recent failure recorded by the supervisor, or
	// nil. Toolsets that don't implement Statable always report nil.
	LastError error
	// RestartCount is the number of supervisor restarts since the last
	// successful Ready transition. Zero for toolsets without a supervisor.
	RestartCount int
}
