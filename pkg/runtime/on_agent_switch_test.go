package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// recordingBuiltin captures the [hooks.Input] passed on every dispatch
// so tests can assert exactly what the runtime forwards into the hook
// protocol. Concurrency-safe because hook dispatch can run from
// arbitrary goroutines.
type recordingBuiltin struct {
	mu     sync.Mutex
	inputs []*hooks.Input
}

func (rb *recordingBuiltin) hook(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if in != nil {
		// Defensive copy: don't depend on whether the executor mutates
		// the pointer after the call returns.
		c := *in
		rb.inputs = append(rb.inputs, &c)
	}
	return nil, nil
}

func (rb *recordingBuiltin) snapshot() []*hooks.Input {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	out := make([]*hooks.Input, len(rb.inputs))
	copy(out, rb.inputs)
	return out
}

// runtimeWithRecordedAgentSwitch wires a recording builtin onto the
// runtime's private hook registry, then registers an on_agent_switch
// entry on the agent that points at it. This is the most direct way
// to assert on dispatched input from a runtime test: the builtin
// system already does the type validation we'd otherwise duplicate.
func runtimeWithRecordedAgentSwitch(t *testing.T, agentName string, opts ...agent.Opt) (*LocalRuntime, *recordingBuiltin) {
	t.Helper()

	rb := &recordingBuiltin{}
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	allOpts := append([]agent.Opt{
		agent.WithModel(prov),
		agent.WithHooks(&hooks.Config{
			OnAgentSwitch: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "test_record_agent_switch",
			}},
		}),
	}, opts...)
	a := agent.New(agentName, "instructions", allOpts...)
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Register our recording builtin on the runtime's private registry
	// after construction, then rebuild the per-agent executors so they
	// pick up the new builtin. This is the smallest test seam that
	// avoids exporting a WithHooksRegistry option on LocalRuntime.
	require.NoError(t, r.hooksRegistry.RegisterBuiltin("test_record_agent_switch", rb.hook))
	r.buildHooksExecutors()

	return r, rb
}

// TestExecuteOnAgentSwitchHooks_ForwardsTransitionFields pins the
// contract: the runtime puts the FromAgent / ToAgent / AgentSwitchKind
// triple onto the hook input verbatim, plus the SessionID. This is the
// data downstream audit pipelines rely on.
func TestExecuteOnAgentSwitchHooks_ForwardsTransitionFields(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedAgentSwitch(t, "root")
	a := r.CurrentAgent()
	require.NotNil(t, a)

	r.executeOnAgentSwitchHooks(t.Context(), a, "session-x", "root", "planner", agentSwitchKindTransferTask)

	got := rb.snapshot()
	require.Len(t, got, 1, "exactly one dispatch must have happened")
	in := got[0]
	assert.Equal(t, "session-x", in.SessionID)
	assert.Equal(t, "root", in.FromAgent)
	assert.Equal(t, "planner", in.ToAgent)
	assert.Equal(t, agentSwitchKindTransferTask, in.AgentSwitchKind)
}

// TestExecuteOnAgentSwitchHooks_NoopWhenNoHookRegistered documents
// the cheap-when-unused property: an agent without an on_agent_switch
// hook produces no dispatch at all (the runtime short-circuits via
// the executor lookup), so audit-free deployments pay nothing.
func TestExecuteOnAgentSwitchHooks_NoopWhenNoHookRegistered(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Should be a successful no-op rather than a panic or error.
	r.executeOnAgentSwitchHooks(t.Context(), a, "s", "root", "next", agentSwitchKindHandoff)
}

// endpointProvider is a minimal [provider.Provider] test double whose
// BaseConfig returns a non-zero base.Config, so we can drive the
// FromAgentModels-population branch of executeOnAgentSwitchHooks.
type endpointProvider struct {
	cfg base.Config
}

func (p *endpointProvider) ID() modelsdev.ID { return p.cfg.ID() }

func (p *endpointProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &mockStream{}, nil
}

func (p *endpointProvider) BaseConfig() base.Config { return p.cfg }

// TestExecuteOnAgentSwitchHooks_PopulatesFromAgentModels pins the
// runtime→hook handoff for the new pure-unload contract: when the
// previous agent has configured models, the runtime must ship a
// snapshot of {Provider, Model, BaseURL, UnloadAPI} for each one on
// Input.FromAgentModels so a runtime-free hook can act on them.
func TestExecuteOnAgentSwitchHooks_PopulatesFromAgentModels(t *testing.T) {
	t.Parallel()

	rb := &recordingBuiltin{}
	prev := &endpointProvider{cfg: base.Config{
		ModelConfig: latest.ModelConfig{
			Provider:     "dmr",
			Model:        "ai/qwen3",
			ProviderOpts: map[string]any{"unload_api": "/engines/_unload"},
		},
		BaseURL: "http://127.0.0.1:12434/engines/v1",
	}}
	next := &mockProvider{id: "test/next", stream: &mockStream{}}

	from := agent.New("root", "instructions",
		agent.WithModel(prev),
		agent.WithHooks(&hooks.Config{
			OnAgentSwitch: []hooks.Hook{{
				Type: hooks.HookTypeBuiltin, Command: "test_record_agent_switch",
			}},
		}))
	to := agent.New("planner", "instructions", agent.WithModel(next))
	tm := team.New(team.WithAgents(from, to))

	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	require.NoError(t, r.hooksRegistry.RegisterBuiltin("test_record_agent_switch", rb.hook))
	r.buildHooksExecutors()

	r.executeOnAgentSwitchHooks(t.Context(), from, "s", "root", "planner", agentSwitchKindTransferTask)

	got := rb.snapshot()
	require.Len(t, got, 1)
	require.Len(t, got[0].FromAgentModels, 1, "runtime must ship one ModelEndpoint per configured model")
	ep := got[0].FromAgentModels[0]
	assert.Equal(t, "dmr", ep.Provider)
	assert.Equal(t, "ai/qwen3", ep.Model)
	assert.Equal(t, "http://127.0.0.1:12434/engines/v1", ep.BaseURL)
	assert.Equal(t, "/engines/_unload", ep.UnloadAPI)
}

// TestExecuteOnAgentSwitchHooks_FromAgentModelsNilWhenFromEmpty pins
// the cheap path on the very first switch into the team's default
// agent: there is no previous agent, so FromAgentModels must stay nil
// (no team lookup, no allocation) and the JSON wire form omits the
// field via `omitempty`.
func TestExecuteOnAgentSwitchHooks_FromAgentModelsNilWhenFromEmpty(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedAgentSwitch(t, "root")
	a := r.CurrentAgent()
	require.NotNil(t, a)

	r.executeOnAgentSwitchHooks(t.Context(), a, "s", "", "root", agentSwitchKindHandoff)

	got := rb.snapshot()
	require.Len(t, got, 1)
	assert.Nil(t, got[0].FromAgentModels)
}
