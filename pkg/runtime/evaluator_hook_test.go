package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	evaluatorprovider "github.com/docker/docker-agent/pkg/evaluator/provider"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestEvaluatorHookApprovalIntegration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		decision    string
		yolo        bool
		denyRule    bool
		httpError   bool
		wantRun     bool
		wantBlock   bool
		wantMessage string
	}{
		{
			name: "allow preserves yolo approval", decision: "allow", yolo: true, wantRun: true,
		},
		{
			name: "allow cannot approve a strict session", decision: "allow",
			wantMessage: "requires user confirmation but the session is non-interactive",
		},
		{
			name: "allow cannot bypass a deny rule", decision: "allow", yolo: true, denyRule: true,
			wantMessage: "denied by permissions configuration",
		},
		{
			name: "ask overrides yolo without a human", decision: "ask", yolo: true,
			wantMessage: "requires user confirmation but the session is non-interactive",
		},
		{
			name: "deny overrides yolo", decision: "deny", yolo: true, wantBlock: true,
			wantMessage: "tool_guard hook",
		},
		{
			name: "HTTP failure closes the guard", decision: "allow", yolo: true, httpError: true, wantBlock: true,
			wantMessage: "evaluator returned HTTP status 503",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v1/systemone", r.URL.Path)
				assert.Equal(t, "Bearer evaluator-secret", r.Header.Get("Authorization"))
				var payload struct {
					State json.RawMessage `json:"state"`
				}
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload)) {
					return
				}
				assert.JSONEq(t, `{"tool_name":"the_tool","tool_input":{"cmd":"git status"},"tool_category":"test"}`, string(payload.State))
				if tt.httpError {
					http.Error(w, "private provider error: evaluator-secret", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"model":"jev-resolved","answers":{"evaluation":{"type":"noul","noul":0.98}}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)

			client, err := evaluatorprovider.New(t.Context(), latest.EvaluatorConfig{
				Provider: "typesafe", Model: "jev", BaseURL: server.URL,
				Type: "boolean", Instructions: "Is the proposed tool call safe?",
			}, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "evaluator-secret"}))
			require.NoError(t, err)

			var executed bool
			agentTools := recordingTool("the_tool", &executed)
			agentTools[0].Category = "test"
			root := agent.New("root", "private agent instructions",
				agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}),
				agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
				agent.WithHooks(&latest.HooksConfig{
					ToolGuard: []latest.HookMatcherConfig{{
						Matcher: "^the_tool$",
						Hooks: []latest.HookDefinition{{
							Type: hooks.HookTypeEvaluator, Evaluator: "safety",
							EvaluatorPolicy: &latest.EvaluatorPolicy{
								Decisions:      map[string]string{"true": tt.decision, "false": "deny"},
								MinProbability: 0.9, Fallback: "ask",
							},
						}},
					}},
				}),
			)
			var checker *permissions.Checker
			if tt.denyRule {
				checker = permissions.NewChecker(&latest.PermissionsConfig{Deny: []string{"the_tool"}})
			}
			tm := team.New(team.WithAgents(root), team.WithPermissions(checker),
				team.WithEvaluators(map[string]evaluator.Evaluator{"safety": client}))
			rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Close()) })
			factory, ok := rt.hooksRegistry.Lookup(hooks.HookTypeEvaluator)
			require.True(t, ok)
			require.NotNil(t, factory)
			require.True(t, rt.hooksExec(root).Has(hooks.EventToolGuard))
			assert.Zero(t, requests.Load(), "constructing a runtime must not evaluate tools")

			policy := session.SafetyPolicyStrict
			if tt.yolo {
				policy = session.SafetyPolicyAutonomous
			}
			sess := session.New(session.WithUserMessage("private conversation"),
				session.WithSafetyPolicy(policy), session.WithNonInteractive(true))
			require.Equal(t, tt.yolo, sess.ToolsApproved)
			calls := []tools.ToolCall{{
				ID: "call_1", Type: "function",
				Function: tools.FunctionCall{Name: "the_tool", Arguments: `{"cmd":"git status"}`},
			}}
			eventCh := make(chan Event, 32)
			rt.processToolCalls(t.Context(), sess, root, calls, agentTools, NewChannelSink(eventCh))
			close(eventCh)
			events := collectClosedEvents(eventCh)

			assert.Equal(t, tt.wantRun, executed)
			assert.EqualValues(t, 1, requests.Load(), "each tool call must evaluate exactly once")
			assert.False(t, hasEventOfType[*ToolCallConfirmationEvent](events))
			assert.Equal(t, tt.wantBlock, hasEventOfType[*HookBlockedEvent](events))
			var responses []*ToolCallResponseEvent
			for _, event := range events {
				if response, ok := event.(*ToolCallResponseEvent); ok {
					responses = append(responses, response)
				}
			}
			require.Len(t, responses, 1)
			require.NotNil(t, responses[0].Result)
			assert.Equal(t, !tt.wantRun, responses[0].Result.IsError)
			if tt.wantMessage != "" {
				assert.Contains(t, responses[0].Response, tt.wantMessage)
			}
			assert.NotContains(t, responses[0].Response, "evaluator-secret")
			assert.NotContains(t, responses[0].Response, "private provider error")
		})
	}
}

func TestEvaluatorHookImportedAgentScope(t *testing.T) {
	t.Parallel()

	for _, childEvaluator := range []string{"safety", "child_only"} {
		t.Run(childEvaluator, func(t *testing.T) {
			t.Parallel()

			var parentCalls, childCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probability := 1.0
				switch r.URL.Path {
				case "/parent/v1/systemone":
					parentCalls.Add(1)
					assert.Equal(t, "Bearer parent-secret", r.Header.Get("Authorization"))
				case "/child/v1/systemone":
					childCalls.Add(1)
					assert.Equal(t, "Bearer child-secret", r.Header.Get("Authorization"))
					probability = 0
				default:
					t.Errorf("unexpected evaluator URL: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprintf(w, `{"model":"jev","answers":{"evaluation":{"type":"noul","noul":%g}}}`, probability)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)

			const sourceYAML = `
evaluators:
  %[1]s:
    provider: typesafe
    model: jev
    base_url: %[2]s
    token_key: %[3]s
    type: boolean
    instructions: Is this tool call safe?
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    %[4]s
    hooks:
      tool_guard:
        - hooks:
            - type: evaluator
              evaluator: %[1]s
              evaluator_policy:
                decisions: {"true": allow, "false": deny}
                min_probability: 0.9
                fallback: ask
`
			parentYAML := fmt.Sprintf(sourceYAML, "safety", server.URL+"/parent", "PARENT_KEY", "sub_agents: [child:example/helper]")
			childYAML := fmt.Sprintf(sourceYAML, childEvaluator, server.URL+"/child", "CHILD_KEY", "")
			tm, err := teamloader.Load(t.Context(), config.NewBytesSource("parent.yaml", []byte(parentYAML)), &config.RuntimeConfig{
				EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{
					"OPENAI_API_KEY": "fake-chat-key", "PARENT_KEY": "parent-secret", "CHILD_KEY": "child-secret",
				}),
			}, teamloader.WithProviderRegistry(testProviderRegistry()),
				teamloader.WithStrict(config.FeatureHooks, config.FeatureEvaluators, config.FeatureExternalAgents),
				teamloader.WithSourceResolver(func(ref string, _ environment.Provider) (config.Source, error) {
					assert.Equal(t, "example/helper", ref)
					return config.NewBytesSource("child.yaml", []byte(childYAML)), nil
				}))
			require.NoError(t, err)
			root, err := tm.Agent("root")
			require.NoError(t, err)
			child, err := tm.Agent("child")
			require.NoError(t, err)
			require.Equal(t, []*agent.Agent{child}, root.SubAgents())
			parentClient, ok := tm.Evaluator("safety")
			require.True(t, ok)
			childClient, ok := child.Evaluator(childEvaluator)
			require.True(t, ok)
			assert.NotSame(t, parentClient, childClient)
			if childEvaluator == "child_only" {
				_, ok := tm.Evaluator(childEvaluator)
				assert.False(t, ok, "imported definitions must not be merged into the team scope")
			}

			var executed bool
			agentTools := recordingTool("scoped_tool", &executed)
			for _, a := range []*agent.Agent{root, child} {
				agent.WithTools(agentTools...)(a)
			}
			rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}), WithSessionCompaction(false))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Close()) })
			assert.Zero(t, parentCalls.Load())
			assert.Zero(t, childCalls.Load())

			for _, a := range []*agent.Agent{root, child} {
				executed = false
				sess := session.New(session.WithAgentName(a.Name()), session.WithSafetyPolicy(session.SafetyPolicyAutonomous), session.WithNonInteractive(true))
				calls := []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "scoped_tool", Arguments: `{}`}}}
				events := make(chan Event, 32)
				rt.processToolCalls(t.Context(), sess, a, calls, agentTools, NewChannelSink(events))
				close(events)
				assert.Equal(t, a == root, executed, "child guard must use its own deny response")
				collected := collectClosedEvents(events)
				assert.Equal(t, a == child, hasEventOfType[*HookBlockedEvent](collected))
				if a == child {
					for _, event := range collected {
						if blocked, ok := event.(*HookBlockedEvent); ok {
							assert.Contains(t, blocked.Message, "Evaluator policy matched the selected outcome.")
						}
					}
				}
			}
			assert.EqualValues(t, 1, parentCalls.Load())
			assert.EqualValues(t, 1, childCalls.Load(), "child-only definitions must remain reachable after import")
		})
	}
}

type runtimeEvaluatorFunc func(context.Context, any) (*evaluator.Result, error)

func (f runtimeEvaluatorFunc) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return f(ctx, state)
}

func TestEvaluatorHookTeamFallbackOnlyForUnboundAgents(t *testing.T) {
	t.Parallel()

	for _, scope := range []string{"unbound", "nil", "empty", "other evaluator", "scoped"} {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()

			var teamCalls, scopedCalls atomic.Int64
			teamClient := runtimeEvaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
				teamCalls.Add(1)
				return &evaluator.Result{Type: "boolean", Probability: new(1.0)}, nil
			})
			scopedClient := runtimeEvaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
				scopedCalls.Add(1)
				return &evaluator.Result{Type: "boolean", Probability: new(0.0)}, nil
			})
			root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/model", stream: &mockStream{}}),
				agent.WithHooks(&latest.HooksConfig{ToolGuard: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{
					Type: hooks.HookTypeEvaluator, Evaluator: "safety", OnError: "ignore",
					EvaluatorPolicy: &latest.EvaluatorPolicy{
						Decisions: map[string]string{"true": "allow", "false": "deny"}, MinProbability: 0.9, Fallback: "ask",
					},
				}}}}}))
			switch scope {
			case "nil":
				agent.WithEvaluators(nil)(root)
			case "empty":
				agent.WithEvaluators(map[string]evaluator.Evaluator{})(root)
			case "other evaluator":
				agent.WithEvaluators(map[string]evaluator.Evaluator{"other": scopedClient})(root)
			case "scoped":
				bindings := map[string]evaluator.Evaluator{"safety": scopedClient}
				agent.WithEvaluators(bindings)(root)
				bindings["safety"] = teamClient
			}
			assert.Equal(t, scope != "unbound", root.HasEvaluatorScope())
			tm := team.New(team.WithAgents(root), team.WithEvaluators(map[string]evaluator.Evaluator{"safety": teamClient}))
			rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}), WithSessionCompaction(false))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Close()) })

			res := rt.dispatchHook(t.Context(), root, hooks.EventToolGuard, &hooks.Input{ToolName: "the_tool"}, nil)
			require.NotNil(t, res)
			switch scope {
			case "unbound":
				assert.True(t, res.Allowed)
				assert.Equal(t, hooks.DecisionAllow, res.Decision)
				assert.EqualValues(t, 1, teamCalls.Load())
				assert.Zero(t, scopedCalls.Load())
			case "scoped":
				assert.False(t, res.Allowed)
				assert.Equal(t, hooks.DecisionDeny, res.Decision)
				assert.Zero(t, teamCalls.Load())
				assert.EqualValues(t, 1, scopedCalls.Load(), "agent bindings must be snapshotted")
			default:
				assert.False(t, res.Allowed)
				assert.Equal(t, -1, res.ExitCode)
				assert.Contains(t, res.Message, `unknown evaluator "safety" for agent "root"`)
				assert.Zero(t, teamCalls.Load())
				assert.Zero(t, scopedCalls.Load())
			}
		})
	}
}
