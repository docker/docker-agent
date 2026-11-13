package acp

import (
	"encoding/json"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestRunAgentRedactedTodosEmitPlanUpdates(t *testing.T) {
	t.Parallel()
	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	registry := hooks.NewRegistry()
	require.NoError(t, builtins.Register(registry))
	executor := hooks.NewExecutorWithRegistry(&hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.RedactSecrets}}}}}, t.TempDir(), nil, registry)
	transformed, err := executor.Dispatch(t.Context(), hooks.EventToolResponseTransform, &hooks.Input{ToolResponse: "updated", ToolResultMeta: []todo.Todo{
		{ID: "one", Description: "check " + secret, Status: "completed"},
		{ID: "two", Description: "remaining task", Status: "in-progress"},
	}})
	require.NoError(t, err)
	require.NotNil(t, transformed.UpdatedToolResultMeta)
	var decoded any
	require.NoError(t, json.Unmarshal(transformed.UpdatedToolResultMeta, &decoded))
	for _, tt := range []struct {
		name string
		meta any
	}{
		{"rewritten JSON", transformed.UpdatedToolResultMeta},
		{"remote JSON", decoded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := &fakeRuntime{events: []runtime.Event{
				runtime.ToolCallResponse("initial", tools.Tool{Name: todo.ToolNameCreateTodos}, &tools.ToolCallResult{Meta: []todo.Todo{{ID: "one", Description: "previous task", Status: "pending"}}}, "initial", "root"),
				runtime.ToolCallResponse("updated", tools.Tool{Name: todo.ToolNameUpdateTodos}, &tools.ToolCallResult{Meta: tt.meta}, "updated", "root"),
			}}
			fixture := newRunAgentFixture(t, rt, &captureWriter{})
			require.NoError(t, fixture.runAgent(t.Context(), fixture.sess))
			var plans []*acpsdk.SessionUpdatePlan
			for _, update := range fixture.sessionUpdates(t) {
				if update.Plan != nil {
					plans = append(plans, update.Plan)
				}
			}
			require.Len(t, plans, 2)
			assert.Equal(t, acpsdk.PlanEntryStatusPending, plans[0].Entries[0].Status)
			require.Len(t, plans[1].Entries, 2)
			assert.Equal(t, acpsdk.PlanEntryStatusCompleted, plans[1].Entries[0].Status)
			assert.Equal(t, acpsdk.PlanEntryStatusInProgress, plans[1].Entries[1].Status)
			assert.Contains(t, plans[1].Entries[0].Content, portcullis.Marker)
			assert.NotContains(t, plans[1].Entries[0].Content, secret)
		})
	}
}
