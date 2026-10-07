package teamloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

const evaluatorTeamYAML = `
evaluators:
  safety:
    provider: risk_api
    model: jev
    type: choice
    instructions: Classify the proposed tool call.
    choices:
      safe: Safe to proceed
      unsafe: Do not proceed
  unused:
    provider: risk_api
    model: jev
    type: boolean
    instructions: Is the state safe?
    token_key: UNUSED_EVALUATOR_KEY
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    hooks:
      tool_guard:
        - matcher: shell
          hooks:
            - type: evaluator
              evaluator: safety
              evaluator_policy:
                decisions:
                  safe: allow
                  unsafe: deny
                min_probability: 0.9
                fallback: ask
`

func TestLoadEvaluatorsIntegration(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"agent provider", "global provider", "agent provider overrides global"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/custom/v1/systemone", r.URL.Path)
				assert.Equal(t, "Bearer custom-evaluator-secret", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				var payload json.RawMessage
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload)) {
					return
				}
				assert.JSONEq(t, `{
  "model":"jev",
  "state":{"tool_name":"shell","tool_input":{"cmd":"git status"}},
  "questions":{"evaluation":{
    "type":"choice",
    "instructions":"Classify the proposed tool call.",
    "criteria":{"safe":"Safe to proceed","unsafe":"Do not proceed"}
  }}
}`, string(payload))
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"model":"jev-resolved","answers":{"evaluation":{"type":"choice","choice":"safe","probabilities":{"safe":0.98,"unsafe":0.02}}},"usage":{"input_tokens":12,"output_tokens":3}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)

			provider := latest.ProviderConfig{
				Provider: "typesafe", BaseURL: server.URL + "/custom", TokenKey: "CUSTOM_EVALUATOR_KEY",
			}
			data := evaluatorTeamYAML
			runConfig := &config.RuntimeConfig{
				EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{
					"OPENAI_API_KEY":       "fake-chat-key",
					"CUSTOM_EVALUATOR_KEY": "custom-evaluator-secret",
					"TYPESAFE_API_KEY":     "wrong-default-key",
				}),
			}
			if source == "global provider" {
				runConfig.Providers = map[string]latest.ProviderConfig{"risk_api": provider}
			} else {
				data = fmt.Sprintf("providers:\n  risk_api:\n    provider: typesafe\n    base_url: %s\n    token_key: CUSTOM_EVALUATOR_KEY\n", provider.BaseURL) + data
				if source == "agent provider overrides global" {
					runConfig.Providers = map[string]latest.ProviderConfig{"risk_api": {
						Provider: "typesafe", BaseURL: server.URL + "/wrong", TokenKey: "WRONG_GLOBAL_KEY",
					}}
				}
			}

			loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("evaluators.yaml", []byte(data)), runConfig,
				withTestProviderRegistry(WithStrict(config.FeatureHooks, config.FeatureEvaluators))...)
			require.NoError(t, err)
			assert.Zero(t, requests.Load(), "loading evaluator definitions must not call the API")
			assert.Equal(t, provider, loaded.Providers["risk_api"])

			client, ok := loaded.Team.Evaluator("safety")
			require.True(t, ok)
			require.NotNil(t, client)
			unused, ok := loaded.Team.Evaluator("unused")
			require.True(t, ok, "all definitions must be built, including unreferenced evaluators")
			require.NotNil(t, unused)
			assert.NotSame(t, client, unused)
			missing, ok := loaded.Team.Evaluator("missing")
			assert.False(t, ok)
			assert.Nil(t, missing)

			root, err := loaded.Team.Agent("root")
			require.NoError(t, err)
			assert.True(t, root.HasEvaluatorScope())
			scoped, ok := root.Evaluator("safety")
			require.True(t, ok)
			assert.Same(t, client, scoped)
			require.NotNil(t, root.Hooks())
			require.Len(t, root.Hooks().ToolGuard, 1)
			matcher := root.Hooks().ToolGuard[0]
			assert.Equal(t, "shell", matcher.Matcher)
			require.Len(t, matcher.Hooks, 1)
			hook := matcher.Hooks[0]
			assert.Equal(t, "evaluator", hook.Type)
			assert.Equal(t, "safety", hook.Evaluator)
			require.NotNil(t, hook.EvaluatorPolicy)
			assert.Equal(t, map[string]string{"safe": "allow", "unsafe": "deny"}, hook.EvaluatorPolicy.Decisions)
			assert.InDelta(t, 0.9, hook.EvaluatorPolicy.MinProbability, 1e-9)
			assert.Equal(t, "ask", hook.EvaluatorPolicy.Fallback)

			for range 2 {
				reused, ok := loaded.Team.Evaluator(hook.Evaluator)
				require.True(t, ok)
				assert.Same(t, client, reused)
				result, err := reused.Evaluate(t.Context(), map[string]any{
					"tool_name": "shell", "tool_input": map[string]any{"cmd": "git status"},
				})
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, "choice", result.Type)
				assert.Equal(t, "safe", result.Choice)
				assert.Equal(t, "jev-resolved", result.Model)
				assert.Equal(t, map[string]float64{"safe": 0.98, "unsafe": 0.02}, result.Probabilities)
				assert.EqualValues(t, 12, result.Usage.InputTokens)
				assert.EqualValues(t, 3, result.Usage.OutputTokens)
			}
			assert.EqualValues(t, 2, requests.Load())
		})
	}
}

type evaluatorCredentialProbe struct {
	calls atomic.Int64
}

func (p *evaluatorCredentialProbe) Get(context.Context, string) (string, bool) {
	p.calls.Add(1)
	return "", false
}

func TestLoadEvaluatorsStrictBeforeCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		features []config.Feature
		missing  string
	}{
		{name: "hooks alone do not enable evaluators", features: []config.Feature{config.FeatureHooks}, missing: `feature "evaluators" at evaluators.safety`},
		{name: "evaluators alone do not enable hooks", features: []config.Feature{config.FeatureEvaluators}, missing: `feature "hooks" at agents.root.hooks`},
		{name: "both capabilities reach credential preflight", features: []config.Feature{config.FeatureHooks, config.FeatureEvaluators}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := &evaluatorCredentialProbe{}
			runConfig := &config.RuntimeConfig{
				Config: config.Config{Providers: map[string]latest.ProviderConfig{
					"risk_api": {Provider: "typesafe", TokenKey: "CUSTOM_EVALUATOR_KEY"},
				}},
				EnvProviderForTests: env,
			}
			_, err := Load(t.Context(), config.NewBytesSource("evaluators.yaml", []byte(evaluatorTeamYAML)), runConfig,
				withTestProviderRegistry(WithStrict(tt.features...))...)
			require.Error(t, err)
			if tt.missing != "" {
				var unsupported *config.UnsupportedError
				require.ErrorAs(t, err, &unsupported)
				assert.Contains(t, err.Error(), tt.missing)
				assert.Zero(t, env.calls.Load(), "unsupported capabilities must fail before any credential access")
			} else {
				assert.Contains(t, err.Error(), "CUSTOM_EVALUATOR_KEY")
				assert.Contains(t, err.Error(), "OPENAI_API_KEY")
				assert.NotContains(t, err.Error(), "UNUSED_EVALUATOR_KEY")
				assert.Positive(t, env.calls.Load())
			}
		})
	}
}

func TestLoadBindsEmptyEvaluatorScope(t *testing.T) {
	t.Parallel()

	const agentYAML = `agents:
  root:
    model: openai/gpt-4o
    instruction: test
`
	parentYAML := agentYAML + "    sub_agents: [child:example/helper]\n"
	tm, err := Load(t.Context(), config.NewBytesSource("parent.yaml", []byte(parentYAML)), &config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fake-chat-key"}),
	}, withTestProviderRegistry(WithSourceResolver(func(ref string, _ environment.Provider) (config.Source, error) {
		assert.Equal(t, "example/helper", ref)
		return config.NewBytesSource("child.yaml", []byte(agentYAML)), nil
	}))...)
	require.NoError(t, err)
	for _, name := range []string{"root", "child"} {
		a, err := tm.Agent(name)
		require.NoError(t, err)
		assert.True(t, a.HasEvaluatorScope(), "loaded agents must not inherit team-level evaluators")
		client, ok := a.Evaluator("safety")
		assert.False(t, ok)
		assert.Nil(t, client)
	}
}

func TestLoadEvaluatorsInheritedHooksPreflight(t *testing.T) {
	t.Parallel()
	const manifest = `
evaluators:
  risk:
    provider: typesafe
    model: jev-latest
    type: boolean
    instructions: Assess risk.
agents:
  root:
    model: openai/gpt-4o
`
	for _, tc := range []struct{ name, reference, want string }{
		{"missing credentials", "risk", "TYPESAFE_API_KEY"},
		{"unknown inherited reference", "missing", "unknown evaluator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})}
			rc.GlobalHooks = &latest.HooksConfig{ToolGuard: latest.HookMatcherConfigs{{Hooks: latest.HookDefinitions{{
				Type: "evaluator", Evaluator: tc.reference, EvaluatorPolicy: &latest.EvaluatorPolicy{
					Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "ask",
				},
			}}}}}
			_, err := Load(t.Context(), config.NewBytesSource("test.yaml", []byte(manifest)), rc, withTestProviderRegistry()...)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadIgnoresUnusedGlobalProviderAuth(t *testing.T) {
	t.Parallel()
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})}
	rc.Providers = map[string]latest.ProviderConfig{
		"unused": {Provider: "anthropic", Auth: &latest.AuthConfig{Type: "typo"}},
	}
	loaded, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(`
agents:
  root:
    model: openai/gpt-4o
`)), rc, withTestProviderRegistry()...)
	require.NoError(t, err)
	require.NotNil(t, loaded)
}

func TestLoadValidatesInheritedHooks(t *testing.T) {
	t.Parallel()
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()}
	rc.GlobalHooks = &latest.HooksConfig{ToolGuard: latest.HookMatcherConfigs{{Matcher: "[", Hooks: latest.HookDefinitions{{
		Type: "command", Command: "echo test",
	}}}}}
	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(`
agents:
  root:
    model: openai/gpt-4o
`)), rc, withTestProviderRegistry()...)
	require.ErrorContains(t, err, "invalid matcher")
}

func TestLoadLayaEvaluatorExample(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/development/predict", r.RequestURI)
		assert.Equal(t, "Bearer test-baseten-key", r.Header.Get("Authorization"))
		var payload struct {
			Model     string         `json:"model"`
			State     map[string]any `json:"state"`
			Questions map[string]any `json:"questions"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload)) {
			return
		}
		assert.Equal(t, "english", payload.Model)
		assert.Equal(t, "shell", payload.State["tool_name"])
		assert.Contains(t, payload.Questions, "evaluation")
		_, err := io.WriteString(w, `{"model":"laya-rl-agent","answers":{"evaluation":{"type":"choice","choice":"read_only","probabilities":{"read_only":0.98,"risky":0.01,"unknown":0.01}}},"usage":{"input_tokens":45,"output_tokens":0},"routing":{"model":"english"}}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	data, err := os.ReadFile("../../examples/evaluators-laya.yaml")
	require.NoError(t, err)
	manifest := strings.ReplaceAll(string(data), "https://model-YOUR_MODEL_ID.api.baseten.co", server.URL)
	rc := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{
			"OPENAI_API_KEY": "fake-chat-key", "BASETEN_API_KEY": "test-baseten-key",
		}),
	}
	registry := NewToolsetRegistry(map[string]ToolsetCreator{"shell": shell.Creator, "filesystem": filesystem.Creator})
	loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("laya.yaml", []byte(manifest)), rc,
		withTestProviderRegistry(WithStrict(config.FeatureHooks, config.FeatureEvaluators), WithToolsetRegistry(registry))...)
	require.NoError(t, err)
	assert.Zero(t, requests.Load())
	client, ok := loaded.Team.Evaluator("tool_risk")
	require.True(t, ok)
	result, err := client.Evaluate(t.Context(), map[string]any{
		"tool_name": "shell", "tool_input": map[string]any{"cmd": "git status"}, "tool_category": "shell",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "read_only", result.Choice)
	assert.Equal(t, "laya-rl-agent", result.Model)
	assert.Nil(t, result.Cost)
	assert.EqualValues(t, 1, requests.Load())
}

const openAIEvaluatorYAML = `
models:
  decision_model:
    provider: openai
    model: gpt-6-luna
    base_url: %[1]s/direct/v1
    token_key: DECISION_KEY
evaluators:
  inline:
    model: openai/gpt-6-luna
    type: boolean
    instructions: Is this safe?
  named:
    model: decision_model
    type: boolean
    instructions: Is this safe?
  bypassed:
    provider: openai
    model: gpt-6-luna
    bypass_models_gateway: true
    base_url: %[1]s/bypass/v1
    type: boolean
    instructions: Is this safe?
agents:
  root:
    model: openai/gpt-4o
    instruction: test
`

func TestLoadOpenAIEvaluators(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	paths := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = io.WriteString(w, `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"evaluation","probability":0.25}]}`)
	}))
	t.Cleanup(server.Close)

	runConfig := &config.RuntimeConfig{
		Config: config.Config{ModelsGateway: server.URL + "/gw"},
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{
			"DECISION_KEY": "decision-secret", "OPENAI_API_KEY": "upstream-secret", "DOCKER_TOKEN": "docker-token",
		}),
	}
	loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("openai.yaml", fmt.Appendf(nil, openAIEvaluatorYAML, server.URL)), runConfig,
		withTestProviderRegistry()...)
	require.NoError(t, err)

	for _, name := range []string{"inline", "named", "bypassed"} {
		client, ok := loaded.Team.Evaluator(name)
		require.True(t, ok, name)
		result, err := client.Evaluate(t.Context(), "state")
		require.NoError(t, err, name)
		require.NotNil(t, result.Probability)
		assert.InDelta(t, 0.25, *result.Probability, 1e-9)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer docker-token", paths["/gw/v1/decisions"], "inline goes through the gateway without the upstream key")
	assert.Equal(t, "Bearer decision-secret", paths["/direct/v1/decisions"], "a custom base URL stays direct")
	assert.Equal(t, "Bearer upstream-secret", paths["/bypass/v1/decisions"], "bypass stays direct")
	assert.Len(t, paths, 3)
}

func TestLoadEvaluatorsRejectsBadModelReferences(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"missing", "openai/", "anthropic/claude"} {
		data := "evaluators:\n  e:\n    model: " + model + "\n    type: boolean\n    instructions: x\nagents:\n  root:\n    model: openai/gpt-4o\n"
		_, err := LoadWithConfig(t.Context(), config.NewBytesSource("bad.yaml", []byte(data)), &config.RuntimeConfig{
			EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "k"}),
		}, withTestProviderRegistry()...)
		require.ErrorContains(t, err, `evaluator "e"`, model)
	}
}
