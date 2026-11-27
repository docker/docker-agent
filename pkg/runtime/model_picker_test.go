package runtime

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
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
		rt.processToolCalls(t.Context(), sess, rt.resolveSessionAgent(sess), []tools.ToolCall{{ID: name, Type: "function", Function: tools.FunctionCall{Name: name, Arguments: args}}}, ts, NewChannelSink(make(chan Event, 128)), nil)
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

// A model switched by the agent must survive a resume, like one the user picked.
func TestModelPickerRecordsOverrideOnSession(t *testing.T) {
	t.Parallel()
	picker := modelpicker.New([]string{"chosen"})
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/root"}), agent.WithToolSets(picker))
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		if c, ok := store.(io.Closer); ok {
			require.NoError(t, c.Close())
		}
	})
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionStore(store), WithModelStore(mockModelStore{}), WithModelSwitcherConfig(&modelcatalog.Config{
		Models: map[string]latest.ModelConfig{"chosen": {Provider: "openai", Model: "gpt-4o-mini"}}, ProviderRegistry: testProviderRegistry(),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithToolsApproved(true))
	require.NoError(t, store.AddSession(t.Context(), sess))
	ts, err := picker.Tools(t.Context())
	require.NoError(t, err)
	invoke := func(name, args string) {
		rt.processToolCalls(t.Context(), sess, root, []tools.ToolCall{{ID: name, Type: "function", Function: tools.FunctionCall{Name: name, Arguments: args}}}, ts, NewChannelSink(make(chan Event, 128)), nil)
	}
	stored := func() map[string]string {
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		return loaded.AgentModelOverrides
	}

	invoke("change_model", `{"model":"chosen"}`)
	assert.Equal(t, map[string]string{"root": "chosen"}, stored())
	invoke("revert_model", `{}`)
	assert.Empty(t, stored())

	// A sub-agent's switch is recorded on the root session, which is the one resumed.
	child := session.New(session.WithParentID(sess.ID), session.WithToolsApproved(true))
	ctx := context.WithValue(t.Context(), rootSessionKey{}, sess)
	rt.processToolCalls(ctx, child, root, []tools.ToolCall{{ID: "sub", Type: "function", Function: tools.FunctionCall{Name: "change_model", Arguments: `{"model":"chosen"}`}}}, ts, NewChannelSink(make(chan Event, 128)), nil)
	assert.Equal(t, map[string]string{"root": "chosen"}, stored())
}
