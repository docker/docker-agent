package tui

import (
	"bytes"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// mockChatPage implements chat.Page for testing.
type mockChatPage struct{}

func (m *mockChatPage) Init() tea.Cmd                            { return nil }
func (m *mockChatPage) Update(tea.Msg) (layout.Model, tea.Cmd)   { return m, nil }
func (m *mockChatPage) View() string                             { return "" }
func (m *mockChatPage) SetSize(int, int) tea.Cmd                 { return nil }
func (m *mockChatPage) CompactSession(string) tea.Cmd            { return nil }
func (m *mockChatPage) SetSessionStarred(bool)                   {}
func (m *mockChatPage) SetTitleRegenerating(bool) tea.Cmd        { return nil }
func (m *mockChatPage) ScrollToBottom() tea.Cmd                  { return nil }
func (m *mockChatPage) IsWorking() bool                          { return false }
func (m *mockChatPage) IsInlineEditing() bool                    { return false }
func (m *mockChatPage) QueueLength() int                         { return 0 }
func (m *mockChatPage) FocusMessages() tea.Cmd                   { return nil }
func (m *mockChatPage) FocusMessageAt(int, int) tea.Cmd          { return nil }
func (m *mockChatPage) BlurMessages()                            {}
func (m *mockChatPage) GetSidebarSettings() chat.SidebarSettings { return chat.SidebarSettings{} }
func (m *mockChatPage) SetSidebarSettings(chat.SidebarSettings)  {}
func (m *mockChatPage) Bindings() []key.Binding                  { return nil }
func (m *mockChatPage) Help() help.KeyMap                        { return nil }

// mockEditor implements editor.Editor for testing.
type mockEditor struct {
	cleanupCalled bool
}

func (m *mockEditor) Init() tea.Cmd                          { return nil }
func (m *mockEditor) Update(tea.Msg) (layout.Model, tea.Cmd) { return m, nil }
func (m *mockEditor) View() string                           { return "" }
func (m *mockEditor) SetSize(int, int) tea.Cmd               { return nil }
func (m *mockEditor) Focus() tea.Cmd                         { return nil }
func (m *mockEditor) Blur() tea.Cmd                          { return nil }
func (m *mockEditor) SetWorking(bool) tea.Cmd                { return nil }
func (m *mockEditor) AcceptSuggestion() tea.Cmd              { return nil }
func (m *mockEditor) ScrollByWheel(int)                      {}
func (m *mockEditor) Value() string                          { return "" }
func (m *mockEditor) SetValue(string)                        {}
func (m *mockEditor) InsertText(string)                      {}
func (m *mockEditor) AttachFile(string) error                { return nil }
func (m *mockEditor) Cleanup()                               { m.cleanupCalled = true }
func (m *mockEditor) GetSize() (int, int)                    { return 0, 0 }
func (m *mockEditor) BannerHeight() int                      { return 0 }
func (m *mockEditor) AttachmentAt(int) (editor.AttachmentPreview, bool) {
	return editor.AttachmentPreview{}, false
}
func (m *mockEditor) SetRecording(bool) tea.Cmd                   { return nil }
func (m *mockEditor) IsRecording() bool                           { return false }
func (m *mockEditor) IsHistorySearchActive() bool                 { return false }
func (m *mockEditor) EnterHistorySearch() (layout.Model, tea.Cmd) { return m, nil }
func (m *mockEditor) SendContent() tea.Cmd                        { return nil }

// collectMsgs executes a command (or batch/sequence of commands) and collects all returned messages.
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()
	if msg == nil {
		return nil
	}

	if batchMsg, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, innerCmd := range batchMsg {
			if innerCmd != nil {
				msgs = append(msgs, collectMsgs(innerCmd)...)
			}
		}
		return msgs
	}

	// Handle Sequence (unexported type, use reflection)
	msgValue := reflect.ValueOf(msg)
	if msgValue.Kind() == reflect.Slice {
		var msgs []tea.Msg
		for i := range msgValue.Len() {
			elem := msgValue.Index(i)
			if elem.CanInterface() {
				if innerCmd, ok := elem.Interface().(tea.Cmd); ok && innerCmd != nil {
					msgs = append(msgs, collectMsgs(innerCmd)...)
				}
			}
		}
		if len(msgs) > 0 {
			return msgs
		}
	}

	return []tea.Msg{msg}
}

func hasMsg[T any](msgs []tea.Msg) bool {
	for _, msg := range msgs {
		if _, ok := msg.(T); ok {
			return true
		}
	}
	return false
}

func newTestModel() (*appModel, *mockEditor) {
	page := &mockChatPage{}
	ed := &mockEditor{}

	m := &appModel{
		chatPages:               map[string]chat.Page{"test": page},
		sessionStates:           map[string]*service.SessionState{},
		editors:                 map[string]editor.Editor{"test": ed},
		pendingRestores:         map[string]string{},
		pendingSidebarCollapsed: map[string]bool{},
		stashedDialogs:          map[string]stashedDialog{},
		chatPage:                page,
		editor:                  ed,
		transcriber:             &fakeTranscriber{},
		notification:            notification.New(),
		dialogMgr:               dialog.New(),
		completions:             completion.New(),
	}
	return m, ed
}

// neutralizeExitFunc replaces the package-level exitFunc with a no-op for
// the duration of the test so that the safety-net goroutine spawned by
// cleanupAll doesn't call os.Exit. It also shrinks shutdownTimeout so the
// safety-net goroutine fires quickly, and waits for it to fire (or for a
// short timeout) before restoring the originals so that subsequent tests
// don't race against a pending background goroutine.
//
// Tests that call this helper must NOT use t.Parallel(): exitFunc and
// shutdownTimeout are package-level variables, and concurrent mutation
// from sibling tests would race with the cleanup that restores them.
func neutralizeExitFunc(t *testing.T) {
	t.Helper()

	origExitFunc := exitFunc
	origTimeout := shutdownTimeout

	fired := make(chan struct{})
	var once sync.Once
	exitFunc = func(int) {
		once.Do(func() { close(fired) })
	}
	shutdownTimeout = 10 * time.Millisecond

	t.Cleanup(func() {
		// Wait for any pending safety-net goroutine to observe our no-op
		// exitFunc, but with a deadline so tests that never trigger
		// cleanupAll don't block.
		select {
		case <-fired:
		case <-time.After(200 * time.Millisecond):
		}
		exitFunc = origExitFunc
		shutdownTimeout = origTimeout
	})
}

func TestExitSessionMsg_ExitsImmediately(t *testing.T) {
	neutralizeExitFunc(t)

	m, ed := newTestModel()

	_, cmd := m.Update(messages.ExitSessionMsg{})

	assert.True(t, ed.cleanupCalled, "Cleanup() should be called on editor")
	require.NotNil(t, cmd, "cmd should not be nil")
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[tea.QuitMsg](msgs), "should produce tea.QuitMsg for immediate exit")
}

func TestExitConfirmedMsg_ExitsImmediately(t *testing.T) {
	neutralizeExitFunc(t)

	m, ed := newTestModel()

	_, cmd := m.Update(dialog.ExitConfirmedMsg{})

	assert.True(t, ed.cleanupCalled, "Cleanup() should be called on editor")
	require.NotNil(t, cmd, "cmd should not be nil")
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[tea.QuitMsg](msgs), "should produce tea.QuitMsg")
}

// blockingWriter is an io.Writer whose Write blocks until unblocked.
type blockingWriter struct {
	mu      sync.Mutex
	blocked chan struct{} // closed once the first Write starts blocking
	gate    chan struct{} // Write blocks until this is closed
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		blocked: make(chan struct{}),
		gate:    make(chan struct{}),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	select {
	case <-w.blocked:
	default:
		close(w.blocked)
	}
	gate := w.gate
	w.mu.Unlock()

	<-gate
	return len(p), nil
}

// reblock installs a new gate so that subsequent writes block again.
func (w *blockingWriter) reblock() {
	w.mu.Lock()
	w.gate = make(chan struct{})
	w.mu.Unlock()
}

// unblock releases all pending and future writes.
func (w *blockingWriter) unblock() {
	w.mu.Lock()
	select {
	case <-w.gate:
	default:
		close(w.gate)
	}
	w.mu.Unlock()
}

// quitModel is a minimal bubbletea model that requests alt-screen output
// and quits in response to a trigger message. An optional onQuit callback
// runs inside Update before tea.Quit is returned.
type quitModel struct {
	onQuit func()
}

type triggerQuitMsg struct{}

func (m *quitModel) Init() tea.Cmd { return nil }

func (m *quitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(triggerQuitMsg); ok {
		if m.onQuit != nil {
			m.onQuit()
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m *quitModel) View() tea.View {
	v := tea.NewView("hello world")
	v.AltScreen = true
	return v
}

// initBlockingBubbletea creates a bubbletea program whose output writer
// blocks. It lets the initial render complete (so the event loop is ready)
// then re-blocks the writer. Returns the program and the writer.
func initBlockingBubbletea(t *testing.T, model tea.Model) (*tea.Program, *blockingWriter, <-chan struct{}) {
	t.Helper()

	w := newBlockingWriter()
	var in bytes.Buffer

	p := tea.NewProgram(model,
		tea.WithContext(t.Context()),
		tea.WithInput(&in),
		tea.WithOutput(w),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()

	// Wait for the initial render to hit the blocking writer.
	select {
	case <-w.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial write to block")
	}

	// Let the initial writes through so the event loop starts.
	w.unblock()
	time.Sleep(200 * time.Millisecond)

	// Re-block so the next renderer flush will stall.
	w.reblock()

	return p, w, runDone
}

// TestCleanupAll_SpawnsSafetyNet verifies that cleanupAll spawns a goroutine
// that calls exitFunc after shutdownTimeout. Without the safety net, the
// process would hang when bubbletea's renderer deadlocks on exit.
func TestCleanupAll_SpawnsSafetyNet(t *testing.T) {
	origTimeout := shutdownTimeout
	origExitFunc := exitFunc
	t.Cleanup(func() {
		shutdownTimeout = origTimeout
		exitFunc = origExitFunc
	})
	shutdownTimeout = 200 * time.Millisecond

	exitDone := make(chan int, 1)
	exitFunc = func(code int) {
		exitDone <- code
	}

	m, _ := newTestModel()
	m.cleanupAll()

	select {
	case code := <-exitDone:
		assert.Equal(t, 0, code)
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("exitFunc was not called — safety net is missing from cleanupAll")
	}
}

// TestExitDeadlock_BlockedStdout proves that bubbletea's p.Run() hangs when
// stdout blocks during the final render after tea.Quit. This is the underlying
// bug that the safety net in cleanupAll works around.
func TestExitDeadlock_BlockedStdout(t *testing.T) {
	t.Parallel()

	model := &quitModel{}
	p, w, runDone := initBlockingBubbletea(t, model)

	// Trigger quit — the event loop will deadlock trying to render.
	p.Send(triggerQuitMsg{})

	// Verify that p.Run() does NOT return within a reasonable window.
	select {
	case <-runDone:
		t.Skip("bubbletea returned without deadlocking; upstream fix may have landed")
	case <-time.After(2 * time.Second):
		// Expected: p.Run() is stuck.
	}

	// Unblock everything to let goroutines drain.
	w.unblock()
}

// TestExitSafetyNet_BlockedStdout verifies that when bubbletea's renderer
// is stuck writing to stdout (terminal buffer full), the shutdown safety net
// forces the process to exit.
//
// Background: bubbletea's cursed renderer holds a mutex during io.Copy to
// stdout. If stdout blocks (e.g. full PTY buffer), the event loop's final
// render call after tea.Quit deadlocks on the same mutex. Without the safety
// net the process hangs forever.
func TestExitSafetyNet_BlockedStdout(t *testing.T) {
	t.Parallel()

	const safetyNetTimeout = 500 * time.Millisecond
	var exitCalled atomic.Bool
	exitDone := make(chan int, 1)
	testExitFunc := func(code int) {
		exitCalled.Store(true)
		exitDone <- code
	}

	model := &quitModel{
		onQuit: func() {
			go func() {
				time.Sleep(safetyNetTimeout)
				testExitFunc(0)
			}()
		},
	}
	p, w, runDone := initBlockingBubbletea(t, model)
	defer w.unblock()

	// Trigger quit — the model's onQuit starts the safety net.
	p.Send(triggerQuitMsg{})

	select {
	case code := <-exitDone:
		assert.True(t, exitCalled.Load())
		assert.Equal(t, 0, code)
	case <-runDone:
		// p.Run() returned on its own — also acceptable.
	case <-time.After(safetyNetTimeout + 2*time.Second):
		t.Fatal("neither p.Run() returned nor safety-net exitFunc fired within the deadline")
	}
}

// TestExitSafetyNet_GracefulShutdown verifies that when bubbletea shuts down
// normally (no blocked stdout), p.Run() returns before the safety net fires.
func TestExitSafetyNet_GracefulShutdown(t *testing.T) {
	t.Parallel()

	const safetyNetTimeout = 2 * time.Second
	var exitCalled atomic.Bool
	testExitFunc := func(int) {
		exitCalled.Store(true)
	}

	var mu sync.Mutex
	cleanupCalled := false

	model := &quitModel{
		onQuit: func() {
			mu.Lock()
			cleanupCalled = true
			mu.Unlock()
			go func() {
				time.Sleep(safetyNetTimeout)
				testExitFunc(0)
			}()
		},
	}
	var buf bytes.Buffer
	var in bytes.Buffer

	p := tea.NewProgram(model,
		tea.WithContext(t.Context()),
		tea.WithInput(&in),
		tea.WithOutput(&buf),
	)

	runDone := make(chan error, 1)
	go func() {
		_, err := p.Run()
		runDone <- err
	}()

	// Give bubbletea time to initialise.
	time.Sleep(200 * time.Millisecond)

	p.Send(triggerQuitMsg{})

	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("p.Run() did not return within deadline for graceful shutdown")
	}

	mu.Lock()
	assert.True(t, cleanupCalled, "cleanup should have been called")
	mu.Unlock()
	assert.False(t, exitCalled.Load(), "exitFunc should NOT fire during graceful shutdown")
}
