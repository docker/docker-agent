package codemode

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestCodeModeTool_Tools(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	toolSet, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolSet, 1)

	fetchTool := toolSet[0]
	assert.Equal(t, "run_tools_with_javascript", fetchTool.Name)
	assert.Equal(t, "code mode", fetchTool.Category)
	assert.NotNil(t, fetchTool.Handler)

	inputSchema, err := json.Marshal(fetchTool.Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"required": [
		"script"
	],
	"properties": {
		"script": {
			"type": "string",
			"description": "Script to execute"
		}
	},
	"additionalProperties": false
}`, string(inputSchema))

	outputSchema, err := json.Marshal(fetchTool.OutputSchema)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"required": [
		"value",
		"stdout",
		"stderr"
	],
	"properties": {
		"stderr": {
			"type": "string",
			"description": "The standard error of the console"
		},
		"stdout": {
			"type": "string",
			"description": "The standard output of the console"
		},
		"value": {
			"type": "string",
			"description": "The value returned by the script"
		},
		"tool_calls": {
			"type": ["null", "array"],
			"description": "The list of tool calls made during script execution, only included on failure",
			"items": {
				"type": "object",
				"additionalProperties": false,
				"required": ["name", "arguments"],
				"properties": {
					"name": {
						"type": "string",
						"description": "The name of the tool that was called"
					},
					"arguments": {
						"description": "The arguments passed to the tool"
					},
					"result": {
						"type": "string",
						"description": "The raw response returned by the tool"
					},
					"error": {
						"type": "string",
						"description": "The error message, if the tool call failed"
					}
				}
			}
		}
	},
	"additionalProperties": false
}`, string(outputSchema))
}

func TestCodeModeTool_TypeScriptDeclarationsInDescription(t *testing.T) {
	t.Parallel()

	tool := Wrap(&testToolSet{tools: []tools.Tool{{
		Name:         "find_item",
		Description:  "Find an item",
		Parameters:   map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []any{"id"}},
		OutputSchema: map[string]any{"type": "boolean"},
	}}})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Contains(t, allTools[0].Description, "interface FindItemInput")
	assert.Contains(t, allTools[0].Description, "id: string;")
	assert.Contains(t, allTools[0].Description, "type FindItemOutput = boolean;")
	assert.Contains(t, allTools[0].Description, "declare function FindItem(args: FindItemInput): FindItemOutput;")
	assert.NotContains(t, allTools[0].Description, "Where Input follows the following JSON schema")
}

func TestCodeModeTool_Instructions(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	instructions := tools.GetInstructions(tool)

	assert.Empty(t, instructions)
}

func TestCodeModeTool_StartStop(t *testing.T) {
	t.Parallel()
	inner := &testToolSet{}

	tool := Wrap(inner)

	assert.Equal(t, 0, inner.start)
	assert.Equal(t, 0, inner.stop)

	startable := tool.(tools.Startable)
	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.start)
	assert.Equal(t, 0, inner.stop)

	err = startable.Stop(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.start)
	assert.Equal(t, 1, inner.stop)
}

func TestCodeModeTool_CallHello(t *testing.T) {
	t.Parallel()
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "hello_world",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("Hello, World!"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return HelloWorld();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "Hello, World!", scriptResult.Value)
	require.Empty(t, scriptResult.StdErr)
	require.Empty(t, scriptResult.StdOut)
}

func TestCodeModeTool_CallToolWithNonIdentifierName(t *testing.T) {
	t.Parallel()
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "hello-world",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("Hello, World!"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Contains(t, allTools[0].Description, "declare function HelloWorld(args: HelloWorldInput): HelloWorldOutput;")

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return HelloWorld();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "Hello, World!", scriptResult.Value)
}

func TestCodeModeTool_CallEcho(t *testing.T) {
	t.Parallel()
	type EchoArgs struct {
		Message string `json:"message" jsonschema:"Message to echo"`
	}

	tool := Wrap(&testToolSet{
		tools: []tools.Tool{{
			Name: "echo",
			Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("ECHO"), nil
			}),
			Parameters: tools.MustSchemaFor[EchoArgs](),
		}},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return echo({'message':'ECHO'});"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "ECHO", scriptResult.Value)
	require.Empty(t, scriptResult.StdErr)
	require.Empty(t, scriptResult.StdOut)
}

// TestCodeModeTool_StartKeepsHealthyToolsetsOnError verifies that when one
// toolset fails to start, its peers stay started (no rollback) and the
// failure is reported as a tools.PartialStartError so the StartableToolSet
// wrapper keeps run_tools_with_javascript available while still surfacing
// the error (#3978).
func TestCodeModeTool_StartKeepsHealthyToolsetsOnError(t *testing.T) {
	t.Parallel()
	failing := &testToolSet{startErr: assert.AnError}
	healthy := &testToolSet{}

	tool := Wrap(healthy, failing).(tools.Startable)

	err := tool.Start(t.Context())
	require.ErrorIs(t, err, assert.AnError)
	require.True(t, tools.IsPartialStart(err), "partial failure must be reported as PartialStartError")
	assert.Equal(t, 1, failing.start, "failing toolset should have attempted start")
	assert.Equal(t, 1, healthy.start, "healthy toolset should have attempted start")
	assert.Equal(t, 0, healthy.stop, "healthy toolset must not be rolled back on a peer's failure")
}

// TestCodeModeTool_PartialStartExposesHealthyTools verifies the degraded-mode
// contract: after a partial start, Tools() still returns
// run_tools_with_javascript with the healthy toolsets' declarations (the
// failed toolset's are omitted), scripts can call the healthy tools, the
// failed toolset alone is retried on the next Start, and a successful retry
// restores its declarations.
func TestCodeModeTool_PartialStartExposesHealthyTools(t *testing.T) {
	t.Parallel()
	healthy := &testToolSet{
		tools: []tools.Tool{
			{
				Name: "fetch_url",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("fetched"), nil
				}),
			},
			{Name: "todo_write", Category: "todo"},
		},
	}
	failing := &testToolSet{
		startErr: assert.AnError,
		tools:    []tools.Tool{{Name: "broken_tool"}},
	}

	tool := Wrap(healthy, failing)
	startable := tool.(tools.Startable)
	reporter := tool.(tools.StartReporter)

	require.Error(t, startable.Start(t.Context()))
	assert.False(t, reporter.IsStarted(), "degraded wrapper must report unstarted so the failed toolset is retried")

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 2)
	assert.Equal(t, "run_tools_with_javascript", allTools[0].Name)
	assert.Contains(t, allTools[0].Description, "declare function FetchUrl", "healthy toolset must stay declared")
	assert.NotContains(t, allTools[0].Description, "BrokenTool", "failed toolset must be omitted")
	assert.Equal(t, "todo_write", allTools[1].Name, "todo exclusion must be preserved in degraded mode")

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return fetch_url();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)
	var scriptResult ScriptResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &scriptResult))
	assert.Equal(t, "fetched", scriptResult.Value, "healthy tools must stay callable from scripts")

	// Retry: only the failed toolset is started again.
	require.Error(t, startable.Start(t.Context()))
	assert.Equal(t, 1, healthy.start, "healthy toolset must not be restarted on retry")
	assert.Equal(t, 2, failing.start, "failed toolset must be retried")

	// Recovery: the toolset comes back and its declarations reappear.
	failing.startErr = nil
	require.NoError(t, startable.Start(t.Context()))
	assert.True(t, reporter.IsStarted())

	allTools, err = tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 2)
	assert.Contains(t, allTools[0].Description, "declare function BrokenTool", "recovered toolset must be declared again")
}

// reportingToolSet is a testToolSet that also reports its live lifecycle
// state (tools.StartReporter) and supports in-place recovery
// (tools.Restartable), like a supervisor-backed MCP toolset. A background
// session death is simulated by flipping started to false.
type reportingToolSet struct {
	testToolSet

	started    bool
	restarts   int
	restartErr error
}

var (
	_ tools.StartReporter = (*reportingToolSet)(nil)
	_ tools.Restartable   = (*reportingToolSet)(nil)
)

func (r *reportingToolSet) Start(ctx context.Context) error {
	if err := r.testToolSet.Start(ctx); err != nil {
		return err
	}
	r.started = true
	return nil
}

func (r *reportingToolSet) Restart(context.Context) error {
	r.restarts++
	if r.restartErr != nil {
		return r.restartErr
	}
	r.started = true
	return nil
}

func (r *reportingToolSet) IsStarted() bool { return r.started }

// TestCodeModeTool_InnerDeathIsDetectedAndRecoveredViaRestart covers the
// "started successfully, then died" arc for a supervisor-backed inner
// toolset (e.g. MCP): the composite must detect the death through the
// inner's tools.StartReporter, degrade (omit the dead inner while keeping
// the healthy one listed), and recover it via Restart — not Start, which
// can be a no-op on a supervisor still holding the dead session. A failed
// recovery surfaces as a PartialStartError and is retried.
func TestCodeModeTool_InnerDeathIsDetectedAndRecoveredViaRestart(t *testing.T) {
	t.Parallel()
	healthy := &testToolSet{tools: []tools.Tool{{Name: "fetch_url"}}}
	flaky := &reportingToolSet{testToolSet: testToolSet{tools: []tools.Tool{{Name: "flaky_tool"}}}}

	tool := Wrap(healthy, flaky)
	startable := tool.(tools.Startable)
	reporter := tool.(tools.StartReporter)

	// Initial start: everything up.
	require.NoError(t, startable.Start(t.Context()))
	assert.True(t, reporter.IsStarted())
	assert.Equal(t, 1, flaky.start)

	// The inner dies in the background: the composite reports degraded and
	// omits the dead inner's declarations so listing keeps working.
	flaky.started = false
	assert.False(t, reporter.IsStarted(), "a dead inner must degrade the composite")

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Contains(t, allTools[0].Description, "FetchUrl", "healthy toolset must stay declared")
	assert.NotContains(t, allTools[0].Description, "FlakyTool", "dead inner must be omitted")

	// Failed recovery: the dead inner is recovered via Restart, its peers
	// are left alone, and the composite stays degraded.
	flaky.restartErr = assert.AnError
	err = startable.Start(t.Context())
	require.ErrorIs(t, err, assert.AnError)
	assert.True(t, tools.IsPartialStart(err))
	assert.Equal(t, 1, flaky.restarts, "dead inner must be recovered via Restart")
	assert.Equal(t, 1, flaky.start, "dead inner must not be blindly re-Started")
	assert.Equal(t, 1, healthy.start, "healthy peer must not be restarted")
	assert.False(t, reporter.IsStarted())

	// Successful Restart on the next recovery attempt brings the inner back.
	flaky.restartErr = nil
	flaky.startErr = assert.AnError // Start must not be used while recovering
	require.NoError(t, startable.Start(t.Context()))
	assert.Equal(t, 2, flaky.restarts)
	assert.True(t, reporter.IsStarted())

	allTools, err = tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Contains(t, allTools[0].Description, "FlakyTool", "recovered inner must be declared again")
}

// blockingToolSet is an inner toolset whose Start wedges: it ignores ctx
// and blocks until release is closed, like an unresponsive MCP server.
// entered is closed once Start is inside the blocking section.
type blockingToolSet struct {
	entered chan struct{}
	release chan struct{}
}

var (
	_ tools.ToolSet   = (*blockingToolSet)(nil)
	_ tools.Startable = (*blockingToolSet)(nil)
)

func newBlockingToolSet() *blockingToolSet {
	return &blockingToolSet{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (b *blockingToolSet) Start(context.Context) error {
	close(b.entered)
	<-b.release
	return nil
}

func (b *blockingToolSet) Stop(context.Context) error { return nil }

// TestCodeModeTool_ToolsNotBlockedByWedgedInnerStart pins the short-lock
// contract behind #3978: c.mu is never held across inner lifecycle calls,
// so while one inner's Start is wedged (ignoring its context), Tools() and
// IsStarted() return promptly and run_tools_with_javascript keeps exposing
// the healthy peer instead of queueing behind the mutex.
func TestCodeModeTool_ToolsNotBlockedByWedgedInnerStart(t *testing.T) {
	t.Parallel()
	healthy := &testToolSet{tools: []tools.Tool{{Name: "fetch_url"}}}
	wedged := newBlockingToolSet()
	releaseWedged := sync.OnceFunc(func() { close(wedged.release) })
	defer releaseWedged() // unblock the Start goroutine even if an assertion fails first

	tool := Wrap(healthy, wedged)

	// Capture the test context here: goroutines below may outlive a t.Fatal
	// path and must not call t.Context() themselves.
	ctx := t.Context()
	startDone := make(chan error, 1)
	go func() { startDone <- tool.(tools.Startable).Start(ctx) }()

	select {
	case <-wedged.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wedged inner Start was never entered")
	}

	type listing struct {
		started bool
		tools   []tools.Tool
		err     error
	}
	listed := make(chan listing, 1)
	go func() {
		started := tool.(tools.StartReporter).IsStarted()
		allTools, err := tool.Tools(ctx)
		listed <- listing{started: started, tools: allTools, err: err}
	}()

	select {
	case res := <-listed:
		require.NoError(t, res.err)
		assert.False(t, res.started, "composite must report unstarted while an inner Start is in flight")
		require.NotEmpty(t, res.tools)
		assert.Equal(t, "run_tools_with_javascript", res.tools[0].Name)
		assert.Contains(t, res.tools[0].Description, "FetchUrl", "healthy peer must stay declared")
	case <-time.After(5 * time.Second):
		t.Fatal("Tools() blocked behind a wedged inner Start")
	}

	releaseWedged()
	select {
	case err := <-startDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the wedged inner was released")
	}
}

// TestCodeModeTool_PartialStartAuthClassification pins how a partial start
// is classified for the authorization special-handling: a batch where every
// failed inner deferred on OAuth is auth-only (silently deferrable), while a
// mixed batch (auth + real failure) must NOT satisfy IsAuthorizationRequired
// — otherwise the real failure would be hidden behind the silent deferral.
// A healthy peer keeps each batch genuinely partial; total failures are
// tools.TotalStartError (see TestCodeModeTool_TotalStartFailureIsNotPartial).
func TestCodeModeTool_PartialStartAuthClassification(t *testing.T) {
	t.Parallel()
	authErr := &tools.AuthorizationRequiredError{URL: "https://example.test/mcp"}

	authOnly := Wrap(&testToolSet{}, &testToolSet{startErr: authErr}).(tools.Startable)
	err := authOnly.Start(t.Context())
	require.True(t, tools.IsPartialStart(err))
	assert.True(t, tools.IsAuthorizationRequired(err),
		"an auth-only partial start must keep the silent OAuth-deferral handling")

	mixed := Wrap(&testToolSet{}, &testToolSet{startErr: authErr}, &testToolSet{startErr: assert.AnError}).(tools.Startable)
	err = mixed.Start(t.Context())
	require.True(t, tools.IsPartialStart(err))
	assert.False(t, tools.IsAuthorizationRequired(err),
		"a mixed batch must not be classified auth-only: the non-auth failure needs surfacing")
	require.ErrorIs(t, err, assert.AnError, "the non-auth cause must stay reachable via errors.Is")
}

// TestCodeModeTool_PartialStartLostAfterStartClassification pins how the
// composite classifies partial failures for the wrapper's recovery notice:
// an inner that never came up (initial failure) leaves LostAfterStart
// false, while an inner that started successfully and was lost since sets
// it, so only real post-start losses can mark the wrapper's recovery streak.
func TestCodeModeTool_PartialStartLostAfterStartClassification(t *testing.T) {
	t.Parallel()
	healthy := &testToolSet{}
	flaky := &reportingToolSet{testToolSet: testToolSet{startErr: assert.AnError}}

	startable := Wrap(healthy, flaky).(tools.Startable)

	// Initial failure: the inner never started, so nothing was lost.
	var partial *tools.PartialStartError
	require.ErrorAs(t, startable.Start(t.Context()), &partial)
	assert.False(t, partial.LostAfterStart, "an initial inner failure is not a post-start loss")

	// The inner recovers, then dies in the background and fails to restart.
	flaky.startErr = nil
	require.NoError(t, startable.Start(t.Context()))
	flaky.started = false
	flaky.restartErr = assert.AnError

	require.ErrorAs(t, startable.Start(t.Context()), &partial)
	assert.True(t, partial.LostAfterStart, "a started-then-lost inner must be classified as post-start loss")
}

// TestCodeModeTool_TotalStartFailureIsNotPartial pins the total-failure
// contract: when every inner toolset fails and none is available, Start
// returns a tools.TotalStartError instead of a tools.PartialStartError. A
// PartialStartError would latch the StartableToolSet wrapper as started and
// expose run_tools_with_javascript with an empty function list; the
// non-partial error keeps the wrapper unlatched so the next Start is a cold
// retry.
func TestCodeModeTool_TotalStartFailureIsNotPartial(t *testing.T) {
	t.Parallel()
	failingA := &testToolSet{startErr: assert.AnError, tools: []tools.Tool{{Name: "tool_a"}}}
	failingB := &testToolSet{startErr: assert.AnError, tools: []tools.Tool{{Name: "tool_b"}}}

	tool := Wrap(failingA, failingB)
	wrapper := tools.NewStartable(tool)

	err := wrapper.Start(t.Context())
	require.ErrorIs(t, err, assert.AnError)
	assert.False(t, tools.IsPartialStart(err), "total failure must not be reported as partial")
	assert.False(t, wrapper.IsStarted(), "total failure must not latch the wrapper")

	// Cold retry: every inner is started again, and a successful retry
	// exposes all declarations.
	failingA.startErr = nil
	failingB.startErr = nil
	require.NoError(t, wrapper.Start(t.Context()))
	assert.True(t, wrapper.IsStarted())
	assert.Equal(t, 2, failingA.start)
	assert.Equal(t, 2, failingB.start)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Contains(t, allTools[0].Description, "ToolA")
	assert.Contains(t, allTools[0].Description, "ToolB")
}

// TestCodeModeTool_TotalMixedFailureIsNotAuthRequired pins the auth
// classification of a mixed total failure (one inner deferred on OAuth, one
// failed for real): it must be non-partial — so the wrapper does not latch
// — and must NOT satisfy IsAuthorizationRequired, otherwise the real
// failure would be suppressed behind the silent auth-deferral handling.
// Both causes stay reachable via errors.Is/errors.As.
func TestCodeModeTool_TotalMixedFailureIsNotAuthRequired(t *testing.T) {
	t.Parallel()
	authErr := &tools.AuthorizationRequiredError{URL: "https://example.test/mcp"}

	tool := Wrap(&testToolSet{startErr: authErr}, &testToolSet{startErr: assert.AnError}).(tools.Startable)
	err := tool.Start(t.Context())
	require.Error(t, err)
	assert.False(t, tools.IsPartialStart(err), "total failure must not be reported as partial")
	assert.False(t, tools.IsAuthorizationRequired(err),
		"a mixed total failure must not be classified auth-only: the real failure needs surfacing")
	require.ErrorIs(t, err, assert.AnError, "the non-auth cause must stay reachable via errors.Is")
	var target *tools.AuthorizationRequiredError
	require.ErrorAs(t, err, &target, "the auth cause must stay reachable via errors.As")
}

// TestCodeModeTool_TotalAuthDeferralKeepsAuthClassification pins that a
// total failure where every inner deferred on OAuth is still classified
// authorization-required even though it is no longer a PartialStartError,
// so the initial deferral stays silent and the dialog appears naturally on
// the first interactive turn.
func TestCodeModeTool_TotalAuthDeferralKeepsAuthClassification(t *testing.T) {
	t.Parallel()
	authErr := &tools.AuthorizationRequiredError{URL: "https://example.test/mcp"}

	tool := Wrap(&testToolSet{startErr: authErr}).(tools.Startable)
	err := tool.Start(t.Context())
	require.Error(t, err)
	assert.False(t, tools.IsPartialStart(err), "total failure must not be reported as partial")
	assert.True(t, tools.IsAuthorizationRequired(err),
		"an all-auth total failure must keep the silent OAuth-deferral handling")
}

// TestCodeModeTool_StartStopWrappedToolSet verifies that Start/Stop find
// Startable through a StartableToolSet wrapper via tools.As.
func TestCodeModeTool_StartStopWrappedToolSet(t *testing.T) {
	t.Parallel()
	inner := &testToolSet{}
	wrapped := tools.NewStartable(inner)

	tool := Wrap(wrapped).(tools.Startable)

	err := tool.Start(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.start)

	err = tool.Stop(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.stop)
}

type testToolSet struct {
	tools    []tools.Tool
	start    int
	stop     int
	startErr error
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*testToolSet)(nil)
	_ tools.Startable = (*testToolSet)(nil)
)

func (t *testToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return t.tools, nil
}

func (t *testToolSet) Start(context.Context) error {
	t.start++
	return t.startErr
}

func (t *testToolSet) Stop(context.Context) error {
	t.stop++
	return nil
}

// capableToolSet is a testToolSet that also implements the capability
// interfaces codeModeTool must forward (Elicitable, Sampleable,
// SampleableWithTools, OAuthCapable, ChangeNotifier) so tests can assert
// that codeModeTool actually forwards to inner toolsets instead of
// silently dropping them (the regression this file guards against: an
// MCP toolset wrapped by code_mode_tools never got its OAuth elicitation
// handler wired up, so its authorization dialog never surfaced).
type capableToolSet struct {
	testToolSet

	elicitationHandler        tools.ElicitationHandler
	samplingHandler           tools.SamplingHandler
	samplingWithToolsHandler  tools.SamplingWithToolsHandler
	oauthSuccessHandler       func()
	managedOAuth              bool
	unmanagedOAuthRedirectURI string
	toolsChangedHandler       func()
}

var (
	_ tools.Elicitable          = (*capableToolSet)(nil)
	_ tools.Sampleable          = (*capableToolSet)(nil)
	_ tools.SampleableWithTools = (*capableToolSet)(nil)
	_ tools.OAuthCapable        = (*capableToolSet)(nil)
	_ tools.ChangeNotifier      = (*capableToolSet)(nil)
)

func (c *capableToolSet) SetElicitationHandler(handler tools.ElicitationHandler) {
	c.elicitationHandler = handler
}

func (c *capableToolSet) SetSamplingHandler(handler tools.SamplingHandler) {
	c.samplingHandler = handler
}

func (c *capableToolSet) SetSamplingWithToolsHandler(handler tools.SamplingWithToolsHandler) {
	c.samplingWithToolsHandler = handler
}

func (c *capableToolSet) SetOAuthSuccessHandler(handler func()) {
	c.oauthSuccessHandler = handler
}

func (c *capableToolSet) SetManagedOAuth(managed bool) {
	c.managedOAuth = managed
}

func (c *capableToolSet) SetUnmanagedOAuthRedirectURI(uri string) {
	c.unmanagedOAuthRedirectURI = uri
}

func (c *capableToolSet) SetToolsChangedHandler(handler func()) {
	c.toolsChangedHandler = handler
}

// TestCodeModeTool_ForwardsCapabilityHandlers verifies that codeModeTool
// forwards elicitation, sampling, OAuth, and tool-list-changed handlers to
// every inner toolset that supports them. Before this fix, codeModeTool
// implemented none of these capability interfaces, so
// tools.ConfigureHandlers (called by the runtime once per turn) could never
// reach an MCP toolset hidden behind code_mode_tools — its OAuth
// elicitation handler stayed nil forever and the authorization dialog
// never surfaced.
func TestCodeModeTool_ForwardsCapabilityHandlers(t *testing.T) {
	t.Parallel()
	capable := &capableToolSet{}
	// A plain toolset without any capability must be tolerated (As returns
	// ok=false) rather than panicking.
	plain := &testToolSet{}

	tool := Wrap(capable, plain)

	elicitHandler := func(context.Context, *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{}, nil
	}
	samplingHandler := func(context.Context, *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
		return nil, nil
	}
	samplingWithToolsHandler := func(context.Context, *mcp.CreateMessageWithToolsParams) (*mcp.CreateMessageWithToolsResult, error) {
		return nil, nil
	}
	oauthCalled := false

	// tools.ConfigureHandlers is the exact call the runtime makes once per
	// turn (see configureToolsetHandlers in pkg/runtime/loop.go); routing
	// through it here instead of casting to each capability interface
	// directly keeps this test aligned with the real call site.
	tools.ConfigureHandlers(tool, elicitHandler, samplingHandler, samplingWithToolsHandler,
		func() { oauthCalled = true }, true, "http://127.0.0.1:1234/callback")

	require.NotNil(t, capable.elicitationHandler)
	require.NotNil(t, capable.samplingHandler)
	require.NotNil(t, capable.samplingWithToolsHandler)
	require.NotNil(t, capable.oauthSuccessHandler)
	capable.oauthSuccessHandler()
	assert.True(t, oauthCalled)
	assert.True(t, capable.managedOAuth)
	assert.Equal(t, "http://127.0.0.1:1234/callback", capable.unmanagedOAuthRedirectURI)

	changedCalled := false
	tool.(tools.ChangeNotifier).SetToolsChangedHandler(func() { changedCalled = true })
	require.NotNil(t, capable.toolsChangedHandler)
	capable.toolsChangedHandler()
	assert.True(t, changedCalled)
}

// TestCodeModeTool_ForwardsCapabilityHandlersThroughStartableWrapper verifies
// that the capability forwarding also finds an inner toolset wrapped in a
// tools.StartableToolSet, matching how real MCP toolsets are wired
// (tools.NewStartable(mcpToolset)) before being handed to codemode.Wrap.
func TestCodeModeTool_ForwardsCapabilityHandlersThroughStartableWrapper(t *testing.T) {
	t.Parallel()
	capable := &capableToolSet{}
	wrapped := tools.NewStartable(capable)

	tool := Wrap(wrapped)

	handler := func(context.Context, *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{}, nil
	}
	tool.(tools.Elicitable).SetElicitationHandler(handler)

	assert.NotNil(t, capable.elicitationHandler)
}

// TestCodeModeTool_SuccessNoToolCalls verifies that successful execution does not include tool calls.
func TestCodeModeTool_SuccessNoToolCalls(t *testing.T) {
	t.Parallel()
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "get_data",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("data"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return get_data();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Success case should not include tool calls
	assert.Equal(t, "data", scriptResult.Value)
	assert.Empty(t, scriptResult.ToolCalls, "successful execution should not include tool_calls")
}

// TestCodeModeTool_FailureIncludesToolCalls verifies that failed execution includes tool call history.
func TestCodeModeTool_FailureIncludesToolCalls(t *testing.T) {
	t.Parallel()
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "first_tool",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("first result"), nil
				}),
			},
			{
				Name: "second_tool",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("second result"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	// Script calls tools successfully but then throws a runtime error
	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"var a = first_tool(); var b = second_tool(); throw new Error('runtime error');"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Failure case should include tool calls
	assert.Contains(t, scriptResult.Value, "runtime error")
	require.Len(t, scriptResult.ToolCalls, 2, "failed execution should include tool_calls")

	// Verify first tool call
	assert.Equal(t, "first_tool", scriptResult.ToolCalls[0].Name)
	assert.Equal(t, "first result", scriptResult.ToolCalls[0].Result)
	assert.Empty(t, scriptResult.ToolCalls[0].Error)

	// Verify second tool call
	assert.Equal(t, "second_tool", scriptResult.ToolCalls[1].Name)
	assert.Equal(t, "second result", scriptResult.ToolCalls[1].Result)
	assert.Empty(t, scriptResult.ToolCalls[1].Error)
}

// TestCodeModeTool_FailureIncludesToolError verifies that tool errors are captured in tool call history.
func TestCodeModeTool_FailureIncludesToolError(t *testing.T) {
	t.Parallel()
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "failing_tool",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return nil, assert.AnError
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return failing_tool();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Script fails due to tool error
	assert.Contains(t, scriptResult.Value, "assert.AnError")
	require.Len(t, scriptResult.ToolCalls, 1, "failed execution should include tool_calls")

	// Verify the tool call recorded the error
	assert.Equal(t, "failing_tool", scriptResult.ToolCalls[0].Name)
	assert.Empty(t, scriptResult.ToolCalls[0].Result)
	assert.Contains(t, scriptResult.ToolCalls[0].Error, "assert.AnError")
}

// TestCodeModeTool_FailureIncludesToolArguments verifies that tool arguments are captured.
func TestCodeModeTool_FailureIncludesToolArguments(t *testing.T) {
	t.Parallel()
	type TestArgs struct {
		Value string `json:"value" jsonschema:"Test value"`
	}

	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "tool_with_args",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("result"), nil
				}),
				Parameters: tools.MustSchemaFor[TestArgs](),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"tool_with_args({'value': 'test123'}); throw new Error('forced error');"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Verify the tool call captured the arguments
	require.Len(t, scriptResult.ToolCalls, 1)
	assert.Equal(t, "tool_with_args", scriptResult.ToolCalls[0].Name)
	assert.Equal(t, map[string]any{"value": "test123"}, scriptResult.ToolCalls[0].Arguments)
	assert.Equal(t, "result", scriptResult.ToolCalls[0].Result)
}
