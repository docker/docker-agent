package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestRunStreamAddContext(t *testing.T) {
	t.Parallel()

	for _, event := range []hooks.EventType{hooks.EventSessionStart, hooks.EventTurnStart, hooks.EventUserPromptSubmit} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()

			hook := hooks.Hook{
				Type:    hooks.HookTypeBuiltin,
				Command: builtins.AddContext,
				Args:    []string{"Current session ID: {{ .SessionID }}", "Agent: {{ .AgentName }}"},
			}
			cfg := &hooks.Config{}
			switch event {
			case hooks.EventSessionStart:
				cfg.SessionStart = []hooks.Hook{hook}
			case hooks.EventTurnStart:
				cfg.TurnStart = []hooks.Hook{hook}
			case hooks.EventUserPromptSubmit:
				cfg.UserPromptSubmit = []hooks.Hook{hook}
			}
			prov := &messageRecordingProvider{id: "test/mock-model"}
			root := agent.New("root", "You are a helpful assistant.", agent.WithModel(prov), agent.WithHooks(cfg))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			sess := session.New(session.WithUserMessage("hello"))
			sess.Title = "add_context test"
			for ev := range rt.RunStream(t.Context(), sess) {
				if userEvent, ok := ev.(*UserMessageEvent); ok {
					assert.Equal(t, "hello", userEvent.Message)
				}
			}

			require.Len(t, prov.recordedMessages, 1)
			var contents []string
			for _, msg := range prov.recordedMessages[0] {
				contents = append(contents, msg.Content)
			}
			assert.Contains(t, strings.Join(contents, "\n"), "Current session ID: "+sess.ID+"\nAgent: root")
			for _, item := range sess.Messages {
				if item.IsMessage() {
					assert.NotEqual(t, chat.MessageRoleSystem, item.Message.Message.Role,
						"hook context must not be appended to the visible transcript")
				}
			}
		})
	}
}
