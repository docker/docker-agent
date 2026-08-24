package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/tools"
)

const prompt = `Run a Javascript script to call MCP tools.

Instead of calling individual MCP tools directly, use this to run a Javascript script that calls as many tools as needed.
This allows you to combine multiple MCP tool calls in a single request, perform conditional logic,
and manipulate the results before returning them.

Instructions:
 - The script has access to all the tools as plain javascript functions.
 - "await"/"async" are never needed. All the tool calls are synchronous.
 - The script must return a string result.
 - "console.*" functions can be used to print debug information.
 - It's often encouraged to group multiple tool calls in a single script to reduce the number of LLM interactions.
   And it allows to do conditional logic based on tool calls.

Available tools/functions:

`

func Wrap(toolsets ...tools.ToolSet) tools.ToolSet {
	return &codeModeTool{
		toolsets: toolsets,
		states:   make([]innerState, len(toolsets)),
	}
}

// innerState tracks the lifecycle of one inner toolset. Never-started
// toolsets (innerIdle) are still listed so Tools() keeps working for
// direct, unstarted use; toolsets whose last Start attempt failed
// (innerFailed) and previously-running toolsets that died or failed to
// recover (innerLost) are omitted until a later recovery succeeds.
type innerState int8

const (
	innerIdle innerState = iota
	innerStarted
	innerFailed
	innerLost
)

type codeModeTool struct {
	toolsets []tools.ToolSet

	// lifecycleMu serializes Start and Stop against each other so their
	// state transitions cannot interleave (the outer StartableToolSet
	// wrapper already single-flights them, but direct use of Wrap must
	// stay safe). It is held across inner Start/Restart/Stop calls, so
	// Tools/IsStarted/runJavascript must never take it.
	lifecycleMu sync.Mutex

	// mu guards states and is only held for short in-memory sections:
	// no inner-toolset method (Start, Restart, Stop, IsStarted, Tools)
	// is ever called with mu held, so a wedged inner blocking on I/O
	// cannot stall concurrent Tools/IsStarted/runJavascript. Lock order:
	// lifecycleMu before mu; mu is a leaf.
	mu     sync.Mutex
	states []innerState
}

// Verify interface compliance
var (
	_ tools.ToolSet             = (*codeModeTool)(nil)
	_ tools.Startable           = (*codeModeTool)(nil)
	_ tools.StartReporter       = (*codeModeTool)(nil)
	_ tools.Named               = (*codeModeTool)(nil)
	_ tools.Elicitable          = (*codeModeTool)(nil)
	_ tools.Sampleable          = (*codeModeTool)(nil)
	_ tools.SampleableWithTools = (*codeModeTool)(nil)
	_ tools.OAuthCapable        = (*codeModeTool)(nil)
	_ tools.ChangeNotifier      = (*codeModeTool)(nil)
)

// Name implements tools.Named; loader-created, so no registry WithName wrapper.
func (c *codeModeTool) Name() string {
	return "code_mode"
}

type RunToolsWithJavascriptArgs struct {
	Script string `json:"script" jsonschema:"Script to execute"`
}

func isExcludedTool(tool tools.Tool) bool {
	return tool.Category == "todo"
}

// availableToolsets returns the inner toolsets whose tools may be exposed:
// every toolset except those whose last Start attempt failed (innerFailed,
// innerLost) and those that started successfully but report dead since (a
// dead inner cannot list its tools — e.g. an MCP toolset without a live
// session errors out — and would take run_tools_with_javascript down with
// it).
func (c *codeModeTool) availableToolsets() []tools.ToolSet {
	states := c.snapshotStates()

	available := make([]tools.ToolSet, 0, len(c.toolsets))
	for i, t := range c.toolsets {
		switch states[i] {
		case innerFailed:
			continue
		case innerStarted, innerLost:
			if innerDied(t) {
				continue
			}
		}
		available = append(available, t)
	}
	return available
}

// snapshotStates returns a copy of the per-toolset states for lock-free
// inspection. The snapshot may lag an in-flight Start/Stop; that is the
// point — readers must not wait on inner lifecycle I/O.
func (c *codeModeTool) snapshotStates() []innerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.states)
}

func (c *codeModeTool) state(i int) innerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[i]
}

func (c *codeModeTool) setState(i int, s innerState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[i] = s
}

// innerDied reports whether a previously-started inner toolset says it is no
// longer running (e.g. its MCP session was lost in the background). Toolsets
// without a tools.StartReporter cannot report death and count as alive.
func innerDied(t tools.ToolSet) bool {
	reporter, ok := tools.As[tools.StartReporter](t)
	return ok && !reporter.IsStarted()
}

func (c *codeModeTool) Tools(ctx context.Context) ([]tools.Tool, error) {
	var (
		functionsDoc  []string
		excludedTools []tools.Tool
	)

	for _, toolset := range c.availableToolsets() {
		allTools, err := toolset.Tools(ctx)
		if err != nil {
			return nil, err
		}

		for _, tool := range allTools {
			if isExcludedTool(tool) {
				excludedTools = append(excludedTools, tool)
			} else {
				functionsDoc = append(functionsDoc, toolToTypeScript(tool))
			}
		}
	}

	allTools := []tools.Tool{{
		Name:        "run_tools_with_javascript",
		Category:    "code mode",
		Description: prompt + strings.Join(functionsDoc, "\n"),
		Parameters:  tools.MustSchemaFor[RunToolsWithJavascriptArgs](),
		Handler: tools.NewRuntimeHandler(func(ctx context.Context, args RunToolsWithJavascriptArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
			result, err := c.runJavascript(ctx, rt, args.Script)
			if err != nil {
				return nil, err
			}

			buf, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("marshaling script's result: %w", err)
			}

			return tools.ResultSuccess(string(buf)), nil
		}),
		OutputSchema: tools.MustSchemaFor[ScriptResult](),
		Annotations: tools.ToolAnnotations{
			Title: "Run tools with Javascript",
		},
	}}

	allTools = append(allTools, excludedTools...)

	return allTools, nil
}

// Start brings every inner toolset up: cold starts (innerIdle), retries of
// toolsets whose initial start failed (innerFailed), and recoveries of
// toolsets that started successfully but died since — detected live through
// their tools.StartReporter, then remembered as innerLost. Recoveries of a
// Restartable inner go through Restart, never blindly through Start: an MCP
// supervisor's Start can be a no-op while it still holds the dead session,
// and Restart also waits for an in-flight background reconnect instead of
// racing it. This mirrors StartableToolSet's own recovery dispatch.
//
// A failing toolset does not abort its peers: healthy toolsets stay started
// and their functions remain exposed, while failed ones are omitted from
// Tools() until a later Start succeeds. Failures are reported through
// tools.PartialStartError so the StartableToolSet wrapper keeps
// run_tools_with_javascript available, still surfaces the warning, and —
// because IsStarted() reports false while any inner toolset is down —
// retries the failed subset on the next turn. When every inner toolset
// fails there is no healthy subset worth exposing: Start returns a
// tools.TotalStartError instead, so the wrapper stays unlatched —
// run_tools_with_javascript is not listed with an empty function list —
// and the next turn retries from cold.
func (c *codeModeTool) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	var (
		errs []error
		lost bool
	)
	for i, t := range c.toolsets {
		// The attempt runs without c.mu held so a wedged inner cannot
		// stall Tools/IsStarted; its outcome is committed under c.mu.
		next, err := startInner(ctx, t, c.state(i))
		c.setState(i, next)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tools.DescribeToolSet(t), err))
			if next == innerLost {
				lost = true
			}
		}
	}
	switch {
	case len(errs) == 0:
		return nil
	case len(errs) == len(c.toolsets):
		// Total failure: nothing is available, so degraded mode has no
		// healthy subset to preserve. The dedicated non-partial type keeps
		// the all-causes auth classification (a bare errors.Join would let
		// one OAuth deferral hide a real failure). See the doc comment above.
		return tools.NewTotalStartError(errs...)
	default:
		err := tools.NewPartialStartError(errs...)
		// Only post-start loss (an inner that was running and died) may
		// trigger the wrapper's recovery notice; retried initial failures
		// must stay silent.
		err.LostAfterStart = lost
		return err
	}
}

// startInner runs one inner toolset's start/recovery attempt and returns
// the resulting state. It must be called without c.mu held: Start, Restart
// and the IsStarted death probe can block on I/O or take the inner's own
// locks.
func startInner(ctx context.Context, t tools.ToolSet, state innerState) (innerState, error) {
	s, ok := tools.As[tools.Startable](t)
	if !ok {
		return innerStarted, nil
	}
	if state == innerStarted || state == innerLost {
		if !innerDied(t) {
			// Running, or a lost toolset whose supervisor already
			// reconnected in the background: nothing to do.
			return innerStarted, nil
		}
		state = innerLost
	}
	recovering := state == innerLost
	var err error
	if restarter, ok := tools.As[tools.Restartable](t); recovering && ok {
		err = restarter.Restart(ctx)
	} else {
		err = s.Start(ctx)
	}
	if err != nil {
		if recovering {
			return innerLost, err
		}
		return innerFailed, err
	}
	return innerStarted, nil
}

// IsStarted implements tools.StartReporter for the StartableToolSet wrapper:
// it reports false while any inner toolset is not running — never started,
// failed, or started-then-died per its own reporter — so the wrapper
// re-invokes Start on the next turn and the degraded subset is retried.
func (c *codeModeTool) IsStarted() bool {
	states := c.snapshotStates()

	for i, t := range c.toolsets {
		if states[i] != innerStarted {
			return false
		}
		if innerDied(t) {
			return false
		}
	}
	return true
}

func (c *codeModeTool) Stop(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	for i := range c.states {
		c.states[i] = innerIdle
	}
	c.mu.Unlock()

	var errs []error
	for _, t := range c.toolsets {
		if s, ok := tools.As[tools.Startable](t); ok {
			if err := s.Stop(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// SetElicitationHandler forwards the handler to every inner toolset that
// supports elicitation (e.g. an MCP toolset driving an OAuth flow). Without
// this, code_mode_tools wrapping hides the inner MCP toolset behind
// codeModeTool, which As can't unwrap (it wraps N toolsets, not one), so the
// handler would never reach it and any OAuth elicitation would find no
// handler wired up and defer forever.
func (c *codeModeTool) SetElicitationHandler(handler tools.ElicitationHandler) {
	for _, t := range c.toolsets {
		if e, ok := tools.As[tools.Elicitable](t); ok {
			e.SetElicitationHandler(handler)
		}
	}
}

// SetSamplingHandler forwards the handler to every inner toolset that
// supports sampling. See SetElicitationHandler for why forwarding is needed.
func (c *codeModeTool) SetSamplingHandler(handler tools.SamplingHandler) {
	for _, t := range c.toolsets {
		if s, ok := tools.As[tools.Sampleable](t); ok {
			s.SetSamplingHandler(handler)
		}
	}
}

// SetSamplingWithToolsHandler forwards the handler to every inner toolset
// that supports sampling-with-tools. See SetElicitationHandler for why
// forwarding is needed.
func (c *codeModeTool) SetSamplingWithToolsHandler(handler tools.SamplingWithToolsHandler) {
	for _, t := range c.toolsets {
		if s, ok := tools.As[tools.SampleableWithTools](t); ok {
			s.SetSamplingWithToolsHandler(handler)
		}
	}
}

// SetOAuthSuccessHandler forwards the handler to every inner toolset that
// supports OAuth. See SetElicitationHandler for why forwarding is needed.
func (c *codeModeTool) SetOAuthSuccessHandler(handler func()) {
	for _, t := range c.toolsets {
		if o, ok := tools.As[tools.OAuthCapable](t); ok {
			o.SetOAuthSuccessHandler(handler)
		}
	}
}

// SetManagedOAuth forwards the managed-OAuth flag to every inner toolset
// that supports OAuth. See SetElicitationHandler for why forwarding is needed.
func (c *codeModeTool) SetManagedOAuth(managed bool) {
	for _, t := range c.toolsets {
		if o, ok := tools.As[tools.OAuthCapable](t); ok {
			o.SetManagedOAuth(managed)
		}
	}
}

// SetUnmanagedOAuthRedirectURI forwards the unmanaged-OAuth redirect URI to
// every inner toolset that supports OAuth. See SetElicitationHandler for why
// forwarding is needed.
func (c *codeModeTool) SetUnmanagedOAuthRedirectURI(uri string) {
	for _, t := range c.toolsets {
		if o, ok := tools.As[tools.OAuthCapable](t); ok {
			o.SetUnmanagedOAuthRedirectURI(uri)
		}
	}
}

// SetToolsChangedHandler forwards the handler to every inner toolset that
// can report a tool-list change (e.g. an MCP server sending
// ToolListChanged). See SetElicitationHandler for why forwarding is needed.
func (c *codeModeTool) SetToolsChangedHandler(handler func()) {
	for _, t := range c.toolsets {
		if n, ok := tools.As[tools.ChangeNotifier](t); ok {
			n.SetToolsChangedHandler(handler)
		}
	}
}
