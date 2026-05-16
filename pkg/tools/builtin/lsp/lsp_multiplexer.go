package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// Multiplexer combines multiple LSP backends into a single toolset.
// It presents one set of lsp_* tools and routes each call to the appropriate
// backend based on the file extension in the tool arguments.
type Multiplexer struct {
	backends []Backend
}

// Backend pairs a raw Tool (used for file-type routing) with an
// optionally-wrapped ToolSet (used for tool enumeration, so that per-toolset
// config like tool filters, instructions, or toon wrappers are respected).
type Backend struct {
	LSP     *Tool
	Toolset tools.ToolSet
}

// lspRouteTarget pairs a backend with the tool handler it produced for a given tool name.
type lspRouteTarget struct {
	lsp     *Tool
	handler tools.ToolHandler
}

// Verify interface compliance.
var (
	_ tools.ToolSet      = (*Multiplexer)(nil)
	_ tools.Startable    = (*Multiplexer)(nil)
	_ tools.Instructable = (*Multiplexer)(nil)
)

// NewLSPMultiplexer creates a multiplexer that routes LSP tool calls
// to the appropriate backend based on file type.
func NewLSPMultiplexer(backends []Backend) *Multiplexer {
	return &Multiplexer{backends: slices.Clone(backends)}
}

func (m *Multiplexer) Start(ctx context.Context) error {
	var started int
	for _, b := range m.backends {
		if err := b.LSP.Start(ctx); err != nil {
			// Clean up previously started backends to avoid resource leaks.
			for _, s := range m.backends[:started] {
				_ = s.LSP.Stop(ctx)
			}
			return fmt.Errorf("starting LSP backend %q: %w", b.LSP.handler.command, err)
		}
		started++
	}
	return nil
}

func (m *Multiplexer) Stop(ctx context.Context) error {
	var errs []error
	for _, b := range m.backends {
		if err := b.LSP.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stopping LSP backend %q: %w", b.LSP.handler.command, err))
		}
	}
	return errors.Join(errs...)
}

// Kind returns the user-facing classification of this toolset.
func (m *Multiplexer) Kind() string { return "LSP" }

// Name returns a comma-separated list of backend command basenames
// (e.g. "gopls,rust-analyzer"). The multiplexer wraps multiple LSP
// toolsets behind a single ToolSet entry, so the /tools dialog needs
// one label that covers them all instead of one row per backend.
func (m *Multiplexer) Name() string {
	names := make([]string, 0, len(m.backends))
	for _, b := range m.backends {
		if n := b.LSP.Name(); n != "" {
			names = append(names, n)
		}
	}
	return strings.Join(names, ",")
}

func (m *Multiplexer) Instructions() string {
	// Combine instructions from all backends, deduplicating identical ones.
	// Typically they share the same base LSP instructions, but individual
	// toolsets may override them via the Instruction config field.
	var parts []string
	seen := make(map[string]bool)
	for _, b := range m.backends {
		instr := tools.GetInstructions(b.Toolset)
		if instr != "" && !seen[instr] {
			seen[instr] = true
			parts = append(parts, instr)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m *Multiplexer) Tools(ctx context.Context) ([]tools.Tool, error) {
	// Collect each backend's tools keyed by name. We build the union of all
	// tool names (not just the first backend's) so that per-backend tool
	// filters don't accidentally hide tools that other backends expose.
	handlersByName := make(map[string][]lspRouteTarget)
	seenTools := make(map[string]tools.Tool) // first definition wins (for schema/description)
	var toolOrder []string                   // preserve insertion order
	for _, b := range m.backends {
		bTools, err := b.Toolset.Tools(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting tools from LSP backend %q: %w", b.LSP.handler.command, err)
		}
		for _, t := range bTools {
			handlersByName[t.Name] = append(handlersByName[t.Name], lspRouteTarget{b.LSP, t.Handler})
			if _, exists := seenTools[t.Name]; !exists {
				seenTools[t.Name] = t
				toolOrder = append(toolOrder, t.Name)
			}
		}
	}

	result := make([]tools.Tool, 0, len(toolOrder))
	for _, name := range toolOrder {
		t := seenTools[name]
		handlers := handlersByName[name]
		if name == ToolNameLSPWorkspace || name == ToolNameLSPWorkspaceSymbols {
			t.Handler = broadcastLSP(handlers)
		} else {
			t.Handler = routeByFile(handlers)
		}
		result = append(result, t)
	}
	return result, nil
}

// routeByFile returns a handler that extracts the "file" field from the JSON
// arguments and dispatches to the backend whose file-type filter matches.
func routeByFile(handlers []lspRouteTarget) tools.ToolHandler {
	return func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
		var args struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return tools.ResultError(fmt.Sprintf("failed to parse file argument: %s", err)), nil
		}
		if args.File == "" {
			return tools.ResultError("file argument is required"), nil
		}
		for _, h := range handlers {
			if h.lsp.HandlesFile(args.File) {
				return h.handler(ctx, tc)
			}
		}
		return tools.ResultError("no LSP server configured for file: " + args.File), nil
	}
}

// broadcastLSP returns a handler that calls every backend best-effort and
// merges the outputs. Individual backend failures are collected rather than
// aborting the entire operation.
func broadcastLSP(handlers []lspRouteTarget) tools.ToolHandler {
	return func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
		var sections []string
		var errs []error
		for _, h := range handlers {
			result, err := h.handler(ctx, tc)
			if err != nil {
				errs = append(errs, fmt.Errorf("backend %s: %w", h.lsp.handler.command, err))
				continue
			}
			if result.IsError {
				sections = append(sections, fmt.Sprintf("[LSP %s] Error: %s", h.lsp.handler.command, result.Output))
			} else if result.Output != "" {
				sections = append(sections, result.Output)
			}
		}
		if len(sections) == 0 && len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		if len(sections) == 0 {
			return tools.ResultSuccess("No results"), nil
		}
		return tools.ResultSuccess(strings.Join(sections, "\n---\n")), nil
	}
}
