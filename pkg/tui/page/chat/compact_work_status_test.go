package chat

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	tooldefaults "github.com/docker/docker-agent/pkg/tui/components/tool/defaults"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Work headers come from the chat turn state: "Working" while a turn runs and
// the settled duration of each finished exchange afterwards, never a stale
// placeholder.
func TestCompactWorkStatusFollowsTurnStateForEachExchange(t *testing.T) {
	t.Parallel()

	ar := animation.NewRuntime()
	sessionState := &service.SessionState{}
	registry := tooldefaults.NewRegistry()
	p := &chatPage{
		toolRenderers: registry,
		ar:            ar,
		sessionState:  sessionState,
		messages:      messages.New(ar, sessionState, messages.WithToolRenderers(registry)),
	}
	p.messages.SetSize(120, 40)
	p.SetCompactWorkLog(true)

	runTurn := func(id string, elapsed time.Duration) {
		p.messages.AddUserMessage("question " + id)
		p.messages.AddOrUpdateToolCall("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "read_file"}}, tools.Tool{}, types.ToolStatusPending)
		p.messages.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: id, Response: "ok"}, types.ToolStatusCompleted)

		_ = p.setWorking(true)
		assert.Contains(t, ansi.Strip(p.messages.View()), "Working", "a running turn shows the live header")

		p.workStartedAt = time.Now().Add(-elapsed)
		_ = p.setWorking(false)
		view := ansi.Strip(p.messages.View())
		assert.NotContains(t, view, "Working", "a finished turn must not keep the live header")
	}

	runTurn("first", 3*time.Second)
	require.Contains(t, ansi.Strip(p.messages.View()), "· 3s")

	runTurn("second", 7*time.Second)
	view := ansi.Strip(p.messages.View())
	assert.Contains(t, view, "· 3s", "the earlier exchange keeps its own duration")
	assert.Contains(t, view, "· 7s", "the latest exchange shows its settled duration")
}
