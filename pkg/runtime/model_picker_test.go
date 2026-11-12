package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
)

func TestModelPickerUsesSessionHandoffCaller(t *testing.T) {
	t.Parallel()
	picker := modelpicker.New([]string{"chosen"})
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/root"}), agent.WithToolSets(modelpicker.New([]string{"parent-only"})))
	target := agent.New("target", "", agent.WithModel(&mockProvider{id: "test/target"}), agent.WithToolSets(picker))
	root.SetModelOverride(&mockProvider{id: "test/root-override"})
	target.SetModelOverride(&mockProvider{id: "test/target-override"})
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, target)), WithModelStore(mockModelStore{}), WithModelSwitcherConfig(&modelcatalog.Config{
		Models: map[string]latest.ModelConfig{"chosen": {Provider: "openai", Model: "gpt-4o-mini"}}, ProviderRegistry: testProviderRegistry(),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithAgentName("root"), session.WithAgentHandoffs(), session.WithToolsApproved(true))
	require.True(t, sess.TryAgentHandoff("target"))
	ts, err := picker.Tools(t.Context())
	require.NoError(t, err)
	invoke := func(name, args string) {
		rt.processToolCalls(t.Context(), sess, rt.resolveSessionAgent(sess), []tools.ToolCall{{ID: name, Type: "function", Function: tools.FunctionCall{Name: name, Arguments: args}}}, ts, NewChannelSink(make(chan Event, 128)))
	}
	invoke("change_model", `{"model":"chosen"}`)
	assert.Equal(t, "openai/gpt-4o-mini", target.Model(t.Context()).ID().String())
	assert.Equal(t, "test/root-override", root.Model(t.Context()).ID().String())
	invoke("change_model", `{"model":"parent-only"}`)
	assert.Equal(t, "openai/gpt-4o-mini", target.Model(t.Context()).ID().String())
	invoke("revert_model", `{}`)
	assert.False(t, target.HasModelOverride())
	assert.True(t, root.HasModelOverride())
	assert.Equal(t, "root", rt.CurrentAgent().Name())
}
