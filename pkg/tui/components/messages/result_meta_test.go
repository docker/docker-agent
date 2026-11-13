package messages

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestLoadFromSessionRestoresToolResultMeta(t *testing.T) {
	t.Parallel()
	for _, reasoning := range []string{"", "thinking"} {
		t.Run("reasoning="+reasoning, func(t *testing.T) {
			t.Parallel()
			meta, err := filesystem.NewEditFileMeta("gone.txt", "before\n", "after\n")
			require.NoError(t, err)
			sess := &session.Session{Messages: []session.Item{
				session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: reasoning,
					ToolCalls:        []tools.ToolCall{{ID: "edit", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path":"gone.txt"}`}}},
					ToolDefinitions:  []tools.Tool{{Name: "edit_file"}},
				}}),
				session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{
					Role: chat.MessageRoleTool, ToolCallID: "edit", Content: "post-edit failed", IsError: true, ToolResultMeta: meta,
				}}),
			}}
			data, err := json.Marshal(sess)
			require.NoError(t, err)
			var restored session.Session
			require.NoError(t, json.Unmarshal(data, &restored))
			registry := tool.NewRegistry()
			var captured *types.Message
			registry.Register("edit_file", func(_ *animation.Runtime, msg *types.Message, _ service.SessionStateReader) layout.Model {
				captured = msg
				return editfile.New(animation.NewRuntime(), msg, service.StaticSessionState{})
			})
			m := NewScrollableView(animation.NewRuntime(), 120, 30, &service.SessionState{}, WithToolRenderers(registry)).(*model)
			m.LoadFromSession(&restored, nil)
			if reasoning != "" {
				// Restored reasoning blocks create tool views lazily.
				block := m.views[0].(*reasoningblock.Model)
				block.View()
			}
			require.NotNil(t, captured)
			require.NotNil(t, captured.ToolResult)
			decoded, ok := filesystem.DecodeEditFileMeta(captured.ToolResult.Meta)
			require.True(t, ok)
			assert.Equal(t, meta, decoded)
			assert.Equal(t, types.ToolStatusError, captured.ToolStatus)
		})
	}
}
