package teamloader

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

// choiceServer answers every assessment with the given choice and counts requests.
func choiceServer(t *testing.T, choice string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		other := "yes"
		if choice == "yes" {
			other = "no"
		}
		_, err := fmt.Fprintf(w, `{"model":"jev","answers":{"evaluation":{"type":"choice","choice":%q,"probabilities":{%q:0.95,%q:0.05}}},"usage":{"input_tokens":1,"output_tokens":0}}`, choice, choice, other)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// routedConfig defines a routing evaluator named task_route that talks to url.
func routedConfig(url string) string {
	return fmt.Sprintf(`
evaluators:
  task_route:
    provider: typesafe
    base_url: %s
    token_key: ROUTE_KEY
    model: jev
    type: choice
    instructions: Pick a route.
    choices:
      yes: Route onward
      no: Stay
agents:
  root:
    model: openai/gpt-4o
    description: entry
    instruction: test
    routing:
      allowed_agents: [helper]
      default_agent: helper
    hooks:
      before_agent_run:
        - type: evaluator
          evaluator: task_route
          routing_policy:
            routes: {yes: helper, no: helper}
            min_probability: 0.5
  helper:
    model: openai/gpt-4o
    description: helper
    instruction: test
`, url)
}

// An imported agent keeps the evaluator bindings of its own source
// configuration, even when the importing configuration defines the same name.
func TestLoadRoutedImportKeepsItsOwnEvaluatorBindings(t *testing.T) {
	t.Parallel()

	parentServer, parentRequests := choiceServer(t, "yes")
	childServer, childRequests := choiceServer(t, "no")

	parentYAML := routedConfig(parentServer.URL) + "    sub_agents: [child:example/imported]\n"
	// Routed agents cannot be imported, so the child only declares the evaluator.
	childYAML := fmt.Sprintf(`
evaluators:
  task_route:
    provider: typesafe
    base_url: %s
    token_key: ROUTE_KEY
    model: jev
    type: choice
    instructions: Pick a route.
    choices:
      yes: Route onward
      no: Stay
agents:
  root:
    model: openai/gpt-4o
    description: imported
    instruction: test
`, childServer.URL)

	loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("parent.yaml", []byte(parentYAML)), &config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{
			"OPENAI_API_KEY": "fake-chat-key", "ROUTE_KEY": "secret",
		}),
	}, withTestProviderRegistry(WithSourceResolver(func(ref string, _ environment.Provider) (config.Source, error) {
		assert.Equal(t, "example/imported", ref)
		return config.NewBytesSource("child.yaml", []byte(childYAML)), nil
	}))...)
	require.NoError(t, err)
	assert.Zero(t, parentRequests.Load()+childRequests.Load(), "loading must not call any evaluator")

	root, err := loaded.Team.Agent("root")
	require.NoError(t, err)
	child, err := loaded.Team.Agent("child")
	require.NoError(t, err)
	require.True(t, root.HasEvaluatorScope())
	require.True(t, child.HasEvaluatorScope(), "an imported agent must keep a bound scope")

	rootEval, ok := root.Evaluator("task_route")
	require.True(t, ok)
	childEval, ok := child.Evaluator("task_route")
	require.True(t, ok)
	require.NotSame(t, rootEval, childEval, "each source configuration owns its own client")

	rootResult, err := rootEval.Evaluate(t.Context(), map[string]any{"input": "q"})
	require.NoError(t, err)
	childResult, err := childEval.Evaluate(t.Context(), map[string]any{"input": "q"})
	require.NoError(t, err)

	assert.Equal(t, "yes", rootResult.Choice, "the parent's evaluator answers for the parent")
	assert.Equal(t, "no", childResult.Choice, "the imported agent's evaluator answers for the import")
	assert.EqualValues(t, 1, parentRequests.Load(), "the parent evaluator hit only the parent endpoint")
	assert.EqualValues(t, 1, childRequests.Load(), "the imported evaluator hit only its own endpoint")
}

// Routing may only name agents declared in the same configuration, so an
// import can never become a route target.
func TestLoadRoutingRejectsExternalTargets(t *testing.T) {
	t.Parallel()

	server, _ := choiceServer(t, "yes")
	yaml := routedConfig(server.URL)
	yaml = strings.Replace(yaml, "allowed_agents: [helper]", "allowed_agents: [child:example/imported]", 1)

	_, err := LoadWithConfig(t.Context(), config.NewBytesSource("parent.yaml", []byte(yaml)), &config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fake-chat-key", "ROUTE_KEY": "secret"}),
	}, withTestProviderRegistry(WithSourceResolver(func(string, environment.Provider) (config.Source, error) {
		t.Error("an external routing target must be rejected before it is resolved")
		return nil, assert.AnError
	}))...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown local agent")
}

// An imported agent's route targets belong to its own configuration, so a
// routed import is rejected instead of resolving them against the parent team.
func TestLoadRejectsRoutedImport(t *testing.T) {
	t.Parallel()

	server, _ := choiceServer(t, "yes")
	// The parent declares its own "helper", which the import must never reach.
	parentYAML := routedConfig(server.URL) + "    sub_agents: [child:example/imported]\n"

	_, err := LoadWithConfig(t.Context(), config.NewBytesSource("parent.yaml", []byte(parentYAML)), &config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fake-chat-key", "ROUTE_KEY": "secret"}),
	}, withTestProviderRegistry(WithSourceResolver(func(string, environment.Provider) (config.Source, error) {
		return config.NewBytesSource("child.yaml", []byte(routedConfig(server.URL))), nil
	}))...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routed agents cannot be imported")
}
