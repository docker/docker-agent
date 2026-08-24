//nolint:unparam // Shared performance harness is activated by descendant benchmark tests.
package tui

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type wallClockCountingWriter struct{ writes, bytes atomic.Uint64 }

func (w *wallClockCountingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	w.bytes.Add(uint64(len(p)))
	return len(p), nil
}

func mixedHistorySession(count int) (*session.Session, int, int) {
	body := strings.Repeat("word ", 996) + "**bold** `code` λ界 end" // exactly 1,000 words
	items := make([]session.Item, 0, count)
	totalBytes := 0
	for i := range count {
		id := fmt.Sprintf("call-%04d", i)
		var msg *session.Message
		switch i % 5 {
		case 0:
			msg = session.UserMessage(body)
		case 1:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "## Assistant\n\n" + body}}
		case 2:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: body, ReasoningContent: body}}
		case 3:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: body, ReasoningContent: body, ToolCalls: []tools.ToolCall{{ID: id, Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"fixture"}`}}}, ToolDefinitions: []tools.Tool{{Name: "read_file", Description: body}}}}
		default:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: fmt.Sprintf("call-%04d", i-1), Content: body}}
		}
		items = append(items, session.NewMessageItem(msg))
		totalBytes += len(msg.Message.Content) + len(msg.Message.ReasoningContent)
	}
	return &session.Session{ID: "profile", Title: "profile", Messages: items}, count * 1000, totalBytes
}

func wallClockRoot(tb testing.TB, width, height int) (*appModel, time.Duration, runtime.MemStats) {
	tb.Helper()
	if setter, ok := tb.(interface{ Setenv(key, value string) }); ok {
		home := tb.TempDir()
		setter.Setenv("HOME", home)
		setter.Setenv("USERPROFILE", home)
	}
	started := time.Now()
	sess := &session.Session{ID: "profile", Title: "profile"}
	a := app.New(tb.Context(), stubRuntime{}, sess)
	m := New(tb.Context(), nil, a, "", func() {}, WithHideSidebar()).(*appModel)
	if cleaner, ok := tb.(interface{ Cleanup(f func()) }); ok {
		cleaner.Cleanup(m.cleanupManagedResources)
	}
	m.supervisor = supervisor.New(nil)
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	page := chat.New(m.ar, tb.Context(), a, ss, chat.WithHideSidebar())
	_ = page.SetSize(width, height-9)
	m.chatPages = map[string]chat.Page{}
	m.sessionStates = map[string]*service.SessionState{}
	m.supervisor.AddSession(tb.Context(), a, sess, "", nil)
	m.chatPages["profile"], m.sessionStates["profile"] = page, ss
	m.chatPage, m.sessionState, m.application = page, ss, a
	m.workingSpinner = spinner.New(m.ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
	m.handleWindowResize(width, height)
	_ = m.Init() // synchronously loads the session; returned one-shot commands are warm-up only
	_ = m.View()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return m, time.Since(started), memory
}
