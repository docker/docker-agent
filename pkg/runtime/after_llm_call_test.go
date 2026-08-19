package runtime

import (
	"context"
	"encoding/json"
	stdruntime "runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// mockModelStoreWithCost returns a model carrying a fixed pricing
// table so after_llm_call can compute a non-nil per-turn cost. The
// zero mockModelStore returns a nil model, which exercises the
// unpriced (nil cost) path instead.
type mockModelStoreWithCost struct {
	ModelStore

	cost modelsdev.Cost
}

func (m mockModelStoreWithCost) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	c := m.cost
	return &modelsdev.Model{Cost: &c}, nil
}

// TestAfterLLMCallHook_PopulatesModelID is a regression test for the
// doc/impl mismatch where [hooks.Input.ModelID] is documented as
// populated for after_llm_call but executeAfterLLMCallHooks never
// actually set it — handlers reading model_id always saw an empty
// string. A single successful turn must dispatch after_llm_call with
// ModelID equal to the provider's canonical "<provider>/<model>" id.
func TestAfterLLMCallHook_PopulatesModelID(t *testing.T) {
	t.Parallel()

	const (
		hookName = "test-after-llm-model-id"
		modelID  = "test/mock-model"
	)

	var captured atomic.Pointer[hooks.Input]

	stream := newStreamBuilder().
		AddContent("ok").
		AddStopWithUsage(1, 1).
		Build()
	prov := &mockProvider{id: modelID, stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			AfterLLMCall: []latest.HookDefinition{
				{Type: "builtin", Command: hookName},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		hookName,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			snap := *in
			captured.Store(&snap)
			return nil, nil
		},
	))

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	got := captured.Load()
	require.NotNil(t, got, "after_llm_call hook must fire on a successful turn")
	assert.Equal(t, modelID, got.ModelID,
		"after_llm_call payload must include the canonical model id; "+
			"see pkg/hooks/types.go:177-186 for the documented contract")
}

// captureAfterLLMCall runs a single successful turn against the given
// model store and returns the after_llm_call payload the runtime
// dispatched, together with the session so callers can cross-check the
// hook cost against what the session recorded. Usage is fixed at 10
// input / 5 output tokens so callers can assert an exact computed cost.
func captureAfterLLMCall(t *testing.T, store ModelStore) (*hooks.Input, *session.Session) {
	t.Helper()

	const hookName = "test-after-llm-usage-cost"

	var captured atomic.Pointer[hooks.Input]

	stream := newStreamBuilder().
		AddContent("ok").
		AddStopWithUsage(10, 5).
		Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			AfterLLMCall: []latest.HookDefinition{
				{Type: "builtin", Command: hookName},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(store),
	)
	require.NoError(t, err)

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		hookName,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			snap := *in
			captured.Store(&snap)
			return nil, nil
		},
	))

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	got := captured.Load()
	require.NotNil(t, got, "after_llm_call hook must fire on a successful turn")
	return got, sess
}

// TestAfterLLMCallHook_PopulatesUsageAndCost pins the priced-call
// contract: when the model has a pricing table, after_llm_call carries
// the provider's token usage and a non-nil Cost equal to the value the
// runtime records on the assistant message (same computeMessageCost
// call, threaded to both).
func TestAfterLLMCallHook_PopulatesUsageAndCost(t *testing.T) {
	t.Parallel()

	rate := modelsdev.Cost{Input: 2.0, Output: 4.0}
	in, sess := captureAfterLLMCall(t, mockModelStoreWithCost{cost: rate})

	require.NotNil(t, in.Usage, "Usage must be populated on after_llm_call")
	assert.Equal(t, int64(10), in.Usage.InputTokens)
	assert.Equal(t, int64(5), in.Usage.OutputTokens)

	// Same arithmetic as computeMessageCost; inputs chosen for exact
	// float64 representation so equality is reliable.
	expected := (float64(10)*rate.Input + float64(5)*rate.Output) / 1e6
	require.NotNil(t, in.Cost, "Cost must be non-nil for a priced model")
	assert.InDelta(t, expected, *in.Cost, 1e-9,
		"hook Cost must equal computeMessageCost(usage, model)")

	// The headline guarantee: the cost the hook reports is the same
	// cost the session bills for the turn. OwnCost sums the recorded
	// assistant message's Cost, set from the same computeMessageCost
	// value threaded into recordAssistantMessage.
	assert.InDelta(t, *in.Cost, sess.OwnCost(), 1e-9,
		"hook Cost must equal the cost the session recorded for the turn")
}

// TestAfterLLMCallHook_CostNilWhenUnpriced pins the unpriced contract:
// when the model has no pricing data (the zero mockModelStore returns a
// nil model), Usage is still populated but Cost is nil — the signal a
// sidecar reads as "this model is unpriced", distinct from a priced
// free call (a non-nil pointer to 0).
func TestAfterLLMCallHook_CostNilWhenUnpriced(t *testing.T) {
	t.Parallel()

	in, _ := captureAfterLLMCall(t, mockModelStore{})

	require.NotNil(t, in.Usage,
		"Usage must still be populated even when the model is unpriced")
	assert.Equal(t, int64(10), in.Usage.InputTokens)
	assert.Nil(t, in.Cost,
		"Cost must be nil for an unpriced model so handlers can "+
			"distinguish it from a priced free call (pointer to 0)")
}

// TestAfterLLMCallInput_CostJSONContract pins the wire format sidecar
// scripts depend on. With Cost as a *float64 + omitempty:
//   - nil   → the "cost" key is absent (unpriced),
//   - &0    → "cost": 0 is present, NOT elided (priced free call —
//     omitempty drops only nil pointers, never a pointer to 0),
//   - &N    → "cost": N.
//
// The same nil-omitted rule applies to Usage, keeping every non-
// after_llm_call event's payload free of spurious cost/usage keys.
func TestAfterLLMCallInput_CostJSONContract(t *testing.T) {
	t.Parallel()

	marshalKeys := func(in *hooks.Input) map[string]any {
		b, err := json.Marshal(in)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		return m
	}

	t.Run("unpriced omits cost and usage", func(t *testing.T) {
		t.Parallel()
		m := marshalKeys(&hooks.Input{HookEventName: hooks.EventAfterLLMCall})
		_, hasCost := m["cost"]
		_, hasUsage := m["usage"]
		assert.False(t, hasCost, "nil Cost must be omitted, not emitted as null")
		assert.False(t, hasUsage, "nil Usage must be omitted")
	})

	t.Run("priced free call emits explicit zero", func(t *testing.T) {
		t.Parallel()
		zero := 0.0
		m := marshalKeys(&hooks.Input{
			HookEventName: hooks.EventAfterLLMCall,
			Usage:         &chat.Usage{InputTokens: 1, OutputTokens: 1},
			Cost:          &zero,
		})
		raw, hasCost := m["cost"]
		require.True(t, hasCost,
			"a non-nil pointer to 0 must emit \"cost\": 0, not be elided — "+
				"this is what distinguishes a free priced call from an unpriced model")
		assert.InDelta(t, float64(0), raw, 1e-9)
		_, hasUsage := m["usage"]
		assert.True(t, hasUsage, "Usage must be present when set")
	})

	t.Run("priced call emits the value", func(t *testing.T) {
		t.Parallel()
		v := 0.0125
		m := marshalKeys(&hooks.Input{HookEventName: hooks.EventAfterLLMCall, Cost: &v})
		assert.InDelta(t, 0.0125, m["cost"], 1e-9)
	})
}

// TestAfterLLMCallHook_HarnessUsageWithoutCostIsUnpriced pins the
// harness cost gate. The codex harness reports token counts via
// turn.completed but never a cost, so the harness library's
// TotalCostUSD defaults to 0. That 0 must be treated as unpriced (nil
// cost on the hook), NOT as a free priced call (cost 0) — otherwise a
// cost ledger would record a real, billed harness turn as $0.
func TestAfterLLMCallHook_HarnessUsageWithoutCostIsUnpriced(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	const hookName = "test-after-llm-harness-cost"

	useHarnessShim(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"harness done"}}
{"type":"turn.completed","usage":{"input_tokens":120,"output_tokens":30}}
`)

	var captured atomic.Pointer[hooks.Input]

	root := agent.New("root", "You are an external coder.",
		agent.WithHarness(&latest.HarnessConfig{Type: "codex"}),
		agent.WithHooks(&latest.HooksConfig{
			AfterLLMCall: []latest.HookDefinition{{Type: "builtin", Command: hookName}},
		}),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		hookName,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			snap := *in
			captured.Store(&snap)
			return nil, nil
		},
	))

	sess := session.New(session.WithUserMessage("do the task"))
	sess.Title = "Harness Unit Test"
	for range rt.RunStream(t.Context(), sess) {
	}

	in := captured.Load()
	require.NotNil(t, in, "after_llm_call must fire for a harness turn")
	require.NotNil(t, in.Usage, "harness usage must be forwarded to the hook")
	assert.Equal(t, int64(120), in.Usage.InputTokens)
	assert.Equal(t, int64(30), in.Usage.OutputTokens)
	assert.Nil(t, in.Cost,
		"a harness that reports no cost must yield nil cost (unpriced), not 0 (free)")
}

// TestSanitizeToolCallName unit-tests the name sanitization function that
// prevents malformed model-emitted tool call names from poisoning session
// history and causing non-retriable ValidationExceptions on strict providers
// (e.g. AWS Bedrock Converse).
func TestSanitizeToolCallName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"read_file", "read_file"},
		{"my-tool_v2", "my-tool_v2"},
		// Exact incident: attribute syntax leaked into name field
		{`view_file" path="install/step4.php" startLine="27" endLine="60`, "view_file"},
		// Colon and slash — common in MCP server-qualified names
		{"server:tool", "server"},
		{"server/tool", "server"},
		// Dot — e.g. "server.tool"
		{"server.tool", "server"},
		// Space
		{"my tool", "my"},
		// Name starting with invalid char falls back to unknown_tool
		{`" path="foo"`, "unknown_tool"},
		{"", "unknown_tool"},
		// Name longer than 64 chars is capped
		{string(make([]byte, 80)), "unknown_tool"}, // all zero bytes are invalid
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, sanitizeToolCallName(tt.input))
		})
	}
}

// TestMalformedToolCallNameSanitizedBeforeStore is a regression test for the
// incident where a model emitted `view_file" path="..."` as a tool call name.
// The assistant message written to the session must carry the sanitized name
// so that replay to strict providers (AWS Bedrock) does not fail.
func TestMalformedToolCallNameSanitizedBeforeStore(t *testing.T) {
	t.Parallel()

	malformedName := `view_file" path="install/step4.php" startLine="27" endLine="60`

	stream := newStreamBuilder().
		AddToolCallName("tc-1", malformedName).
		AddToolCallArguments("tc-1", `{}`).
		AddToolCallStopWithUsage(10, 5).
		Build()

	sess := session.New(session.WithUserMessage("show me step4.php"))
	events := runSession(t, sess, stream)

	var assistantMsg *session.Message
	for _, ev := range events {
		if e, ok := ev.(*MessageAddedEvent); ok && e.Message != nil && e.Message.Message.Role == chat.MessageRoleAssistant {
			assistantMsg = e.Message
		}
	}

	require.NotNil(t, assistantMsg, "expected an assistant message to be persisted")
	require.Len(t, assistantMsg.Message.ToolCalls, 1)
	assert.Equal(t, "view_file", assistantMsg.Message.ToolCalls[0].Function.Name,
		"malformed name must be truncated at the first invalid character before storage")
}

// TestComputeMessageCost unit-tests the single cost-arithmetic source
// shared by the persisted message and the after_llm_call payload,
// including every branch that yields nil (unpriced).
func TestComputeMessageCost(t *testing.T) {
	t.Parallel()

	rate := &modelsdev.Cost{Input: 2.0, Output: 4.0, CacheRead: 1.0, CacheWrite: 5.0}

	t.Run("nil usage is unpriced", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, computeMessageCost(nil, &modelsdev.Model{Cost: rate}))
	})
	t.Run("nil model is unpriced", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, computeMessageCost(&chat.Usage{InputTokens: 1}, nil))
	})
	t.Run("model without pricing table is unpriced", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, computeMessageCost(&chat.Usage{InputTokens: 1}, &modelsdev.Model{}))
	})
	t.Run("priced computes from all token classes", func(t *testing.T) {
		t.Parallel()
		usage := &chat.Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 4, CacheWriteTokens: 2}
		got := computeMessageCost(usage, &modelsdev.Model{Cost: rate})
		require.NotNil(t, got)
		expected := (10*rate.Input + 5*rate.Output + 4*rate.CacheRead + 2*rate.CacheWrite) / 1e6
		assert.InDelta(t, expected, *got, 1e-9)
	})
}
