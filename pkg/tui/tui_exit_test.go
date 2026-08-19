package tui

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/tour"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// mockChatPage implements chat.Page for testing.
type mockChatPage struct {
	compactCalls []string // AdditionalPrompt of each CompactSession call

	// interruptMode records the last SetInterruptMode value.
	interruptMode messages.InterruptMode
}

func (m *mockChatPage) Init() tea.Cmd                          { return nil }
func (m *mockChatPage) Update(tea.Msg) (layout.Model, tea.Cmd) { return m, nil }
func (m *mockChatPage) View() string                           { return "" }
func (m *mockChatPage) SetSize(int, int) tea.Cmd               { return nil }
func (m *mockChatPage) CompactSession(prompt string) tea.Cmd {
	m.compactCalls = append(m.compactCalls, prompt)
	return nil
}
func (m *mockChatPage) SetSessionStarred(bool)                   {}
func (m *mockChatPage) SetTitleRegenerating(bool) tea.Cmd        { return nil }
func (m *mockChatPage) ScrollToBottom() tea.Cmd                  { return nil }
func (m *mockChatPage) IsWorking() bool                          { return false }
func (m *mockChatPage) IsInlineEditing() bool                    { return false }
func (m *mockChatPage) IsSelecting() bool                        { return false }
func (m *mockChatPage) QueueLength() int                         { return 0 }
func (m *mockChatPage) FocusMessages() tea.Cmd                   { return nil }
func (m *mockChatPage) FocusMessageAt(int, int) tea.Cmd          { return nil }
func (m *mockChatPage) BlurMessages()                            {}
func (m *mockChatPage) GetSidebarSettings() chat.SidebarSettings { return chat.SidebarSettings{} }
func (m *mockChatPage) SetSidebarSettings(chat.SidebarSettings)  {}
func (m *mockChatPage) SetLayoutSettings(messages.LayoutSettings) tea.Cmd {
	return nil
}
func (m *mockChatPage) SetSendMode(messages.SendMode) {}
func (m *mockChatPage) SetInterruptMode(mode messages.InterruptMode) {
	m.interruptMode = mode
}
func (m *mockChatPage) SetRoutingID(string)       {}
func (m *mockChatPage) TakeRoutedTimers() tea.Cmd { return nil }
func (m *mockChatPage) VisualGeneration() uint64  { return 0 }
func (m *mockChatPage) Bindings() []key.Binding   { return nil }
func (m *mockChatPage) Help() help.KeyMap         { return nil }

type countingChatPage struct {
	mockChatPage

	views     int
	text      string
	dirtyTick bool
}

func (p *countingChatPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if tick, ok := msg.(animation.TickMsg); ok && p.dirtyTick {
		tick.MarkDirty()
	}
	return p, nil
}

func (p *countingChatPage) VisualGeneration() uint64 { return 0 }
func (p *countingChatPage) View() string {
	p.views++
	return p.text
}

func TestRootViewCachePreservesExactViewOnCleanTick(t *testing.T) {
	m, _ := newTestModel(t)
	page := &countingChatPage{text: "stable"}
	m.chatPage = page
	m.ready = true
	m.leanMode = true
	m.ar = animation.NewRuntime()
	m.appName = "test"
	m.sessionState = &service.SessionState{}

	first := m.View()
	require.Equal(t, 1, page.views)
	second := m.View()
	assert.Equal(t, first, second)
	assert.Equal(t, fmt.Sprintf("%p", first.OnMouse), fmt.Sprintf("%p", second.OnMouse))
	assert.Equal(t, first.Cursor, second.Cursor)
	assert.Equal(t, first.ForegroundColor, second.ForegroundColor)
	assert.Equal(t, first.BackgroundColor, second.BackgroundColor)
	assert.Equal(t, first.WindowTitle, second.WindowTitle)
	assert.Equal(t, first.ProgressBar, second.ProgressBar)
	assert.Equal(t, first.AltScreen, second.AltScreen)
	assert.Equal(t, first.MouseMode, second.MouseMode)
	assert.Equal(t, 1, page.views, "cached root view must skip composition")

	m.update(struct{}{})
	_ = m.View()
	assert.Equal(t, 2, page.views, "every non-animation message invalidates")
}

func TestRootViewCacheInvalidatesOnlyForDirtyAcceptedTick(t *testing.T) {
	m, _ := newTestModel(t)
	page := &countingChatPage{text: "stable"}
	m.chatPage = page
	m.ready = true
	m.leanMode = true
	m.ar = animation.NewRuntime()
	m.appName = "test"
	m.sessionState = &service.SessionState{}
	_ = m.View()

	sub := m.ar.Subscribe()
	cleanCmd := sub.Start()
	clean := cleanCmd().(animation.TickMsg)
	_, _ = m.update(clean)
	_ = m.View()
	assert.Equal(t, 1, page.views)

	page.dirtyTick = true
	dirtyCmd := m.ar.EnsureRunning()
	dirty := dirtyCmd().(animation.TickMsg)
	_, _ = m.update(dirty)
	_ = m.View()
	assert.Equal(t, 2, page.views)
	sub.Stop()
}

// mockEditor implements editor.Editor for testing.
type mockEditor struct {
	cleanupCalled bool
	// argumentCompletionCmd is returned by TryStartArgumentCompletion, letting
	// tests simulate an open (or not-applicable) argument-completion popup.
	argumentCompletionCmd tea.Cmd
	// acceptSuggestionCmd is returned by AcceptSuggestion, letting tests
	// simulate a pending ghost suggestion (e.g. a history match) that would
	// otherwise compete with argument completion for the same Tab press.
	acceptSuggestionCmd tea.Cmd
}

func (m *mockEditor) Init() tea.Cmd                          { return nil }
func (m *mockEditor) Update(tea.Msg) (layout.Model, tea.Cmd) { return m, nil }
func (m *mockEditor) View() string                           { return "" }
func (m *mockEditor) SetSize(int, int) tea.Cmd               { return nil }
func (m *mockEditor) Focus() tea.Cmd                         { return nil }
func (m *mockEditor) Blur() tea.Cmd                          { return nil }
func (m *mockEditor) SetWorking(bool) tea.Cmd                { return nil }
func (m *mockEditor) AcceptSuggestion() tea.Cmd              { return m.acceptSuggestionCmd }
func (m *mockEditor) TryStartArgumentCompletion() tea.Cmd    { return m.argumentCompletionCmd }
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

func newTestModel(tb testing.TB) (*appModel, *mockEditor) {
	tb.Helper()
	page := &mockChatPage{}
	ed := &mockEditor{}

	m := &appModel{
		ctx:                     tb.Context,
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
		tour:                    tour.New(),
	}
	return m, ed
}

// neutralizeExitFunc replaces the model's exit function with a no-op for the
// duration of the test and waits for the safety-net goroutine to fire (or time
// out). It sets per-model fields rather than package globals, so tests using
// it may run in parallel.
func neutralizeExitFunc(t *testing.T, m *appModel) {
	t.Helper()

	fired := make(chan struct{})
	var once sync.Once
	m.exitFunc = func(int) {
		once.Do(func() { close(fired) })
	}
	m.shutdownTimeout = 10 * time.Millisecond

	t.Cleanup(func() {
		select {
		case <-fired:
		case <-time.After(200 * time.Millisecond):
		}
	})
}

func TestExitSessionMsg_ExitsImmediately(t *testing.T) {
	t.Parallel()

	m, ed := newTestModel(t)
	neutralizeExitFunc(t, m)

	_, cmd := m.Update(messages.ExitSessionMsg{})

	assert.True(t, ed.cleanupCalled, "Cleanup() should be called on editor")
	require.NotNil(t, cmd, "cmd should not be nil")
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[tea.QuitMsg](msgs), "should produce tea.QuitMsg for immediate exit")
}

func TestExitConfirmedMsg_ExitsImmediately(t *testing.T) {
	t.Parallel()

	m, ed := newTestModel(t)
	neutralizeExitFunc(t, m)

	_, cmd := m.Update(dialog.ExitConfirmedMsg{})

	assert.True(t, ed.cleanupCalled, "Cleanup() should be called on editor")
	require.NotNil(t, cmd, "cmd should not be nil")
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[tea.QuitMsg](msgs), "should produce tea.QuitMsg")
}

// blockingWriter is an io.Writer whose Write blocks until unblocked. It
// records everything written so tests can sync on rendered content.
type blockingWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
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
	// Capture the gate before publishing the bytes: once a test observes
	// this write's content, the write is guaranteed to complete even if
	// reblock() swaps the gate right after.
	gate := w.gate
	w.buf.Write(p)
	select {
	case <-w.blocked:
	default:
		close(w.blocked)
	}
	w.mu.Unlock()

	<-gate
	return len(p), nil
}

// contains reports whether the accumulated output includes s.
func (w *blockingWriter) contains(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Contains(w.buf.String(), s)
}

// reblock installs a new gate so that subsequent writes block again.
func (w *blockingWriter) reblock() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gate = make(chan struct{})
}

// unblock releases all pending and future writes.
func (w *blockingWriter) unblock() {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.gate:
	default:
		close(w.gate)
	}
}

// quitModel is a minimal bubbletea model that requests alt-screen and quits
// on triggerQuitMsg. onQuit, if set, runs before tea.Quit.
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

// initBlockingBubbletea starts a bubbletea program whose stdout will block
// the renderer on its next flush. Used to reproduce the wedged-renderer
// shutdown deadlock.
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

	select {
	case <-w.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial write to block")
	}

	// Let the initial writes through so the event loop starts. The setup
	// burst ends with clear-screen (ESC[2J); once it lands the renderer is
	// idle (no frame renders without a window size), so re-block to make
	// the next flush stall.
	w.unblock()
	require.Eventually(t, func() bool { return w.contains("\x1b[2J") },
		5*time.Second, time.Millisecond, "terminal setup was not flushed")
	w.reblock()

	return p, w, runDone
}

// TestCleanupAll_SpawnsSafetyNet: an unstarted Program has a nil finished
// channel, so Wait() blocks forever — same shape as a real renderer
// deadlock. exitFunc must fire after shutdownTimeout.
func TestCleanupAll_SpawnsSafetyNet(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 200 * time.Millisecond

	exitDone := make(chan int, 1)
	m.exitFunc = func(code int) {
		exitDone <- code
	}

	m.program = tea.NewProgram(&quitModel{})
	m.cleanupAll()

	select {
	case code := <-exitDone:
		assert.Equal(t, 0, code)
	case <-time.After(m.shutdownTimeout + time.Second):
		t.Fatal("exitFunc was not called — safety net is missing from cleanupAll")
	}
}

// TestCleanupAll_GracefulShutdownSkipsExit: when Wait() returns promptly,
// the safety net must not call exitFunc.
func TestCleanupAll_GracefulShutdownSkipsExit(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 2 * time.Second

	exitFired := make(chan struct{}, 1)
	m.exitFunc = func(int) { exitFired <- struct{}{} }

	var in, out bytes.Buffer
	p := tea.NewProgram(&quitModel{},
		tea.WithContext(t.Context()),
		tea.WithInput(&in),
		tea.WithOutput(&out),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()

	// Send blocks until the program is running, which guarantees Run() has
	// initialized p.finished — otherwise Wait() races the assignment.
	p.Send(syncMsg{})

	m.program = p
	m.cleanupAll()

	p.Send(triggerQuitMsg{})

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("p.Run() did not return within deadline")
	}

	// Give the safety-net goroutine a window to (wrongly) fire after
	// Wait() returned.
	select {
	case <-exitFired:
		t.Fatal("exitFunc must not fire on prompt shutdown")
	case <-time.After(100 * time.Millisecond):
	}
}

// syncMsg pings the program's event loop to confirm Run() has started.
type syncMsg struct{}

// TestCleanupAll_RepeatCallsDoNotStackOnWedgedCleanup: once the first
// cleanupAll has kicked off a (possibly wedged) resource cleanup, later calls
// must be complete no-ops — they must not spawn more goroutines that pile up
// behind the wedged sync.Once inside cleanupManagedResources.
func TestCleanupAll_RepeatCallsDoNotStackOnWedgedCleanup(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 100 * time.Millisecond
	m.exitFunc = func(int) {}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var cleanupCalls atomic.Int32
	sv := supervisor.New(nil)
	sv.AddSession(t.Context(), nil, session.New(), t.TempDir(), func() {
		cleanupCalls.Add(1)
		<-release
	})
	m.supervisor = sv

	// All calls must return promptly, and only the first may start a cleanup.
	for range 3 {
		returned := make(chan struct{})
		go func() {
			m.cleanupAll()
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("cleanupAll blocked on the wedged resource cleanup")
		}
	}

	assert.LessOrEqual(t, cleanupCalls.Load(), int32(1),
		"repeat cleanupAll calls must not start additional resource cleanups")
}

// TestCleanupAll_WedgedResourceCleanupForcesExit: supervisor shutdown can
// block indefinitely (e.g. a toolset Stop wedged behind a broken Docker
// daemon). cleanupAll must not run it synchronously — that would freeze the
// Update loop before tea.Quit is ever processed, so confirming the exit
// dialog would appear to do nothing — and the safety net must force the exit
// when the cleanup never completes.
func TestCleanupAll_WedgedResourceCleanupForcesExit(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 200 * time.Millisecond

	exitDone := make(chan struct{})
	m.exitFunc = func(int) { close(exitDone) }

	// A session whose cleanup blocks forever, as a wedged toolset Stop would.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sv := supervisor.New(nil)
	sv.AddSession(t.Context(), nil, session.New(), t.TempDir(), func() { <-release })
	m.supervisor = sv

	var in, out bytes.Buffer
	p := tea.NewProgram(&quitModel{},
		tea.WithContext(t.Context()),
		tea.WithInput(&in),
		tea.WithOutput(&out),
	)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()
	p.Send(syncMsg{})

	m.program = p

	// cleanupAll must return promptly even though the supervisor cleanup is
	// wedged — it runs from the Update loop, right before tea.Quit.
	returned := make(chan struct{})
	go func() {
		m.cleanupAll()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanupAll blocked on the wedged resource cleanup")
	}

	p.Send(triggerQuitMsg{})

	select {
	case <-exitDone:
	case <-time.After(m.shutdownTimeout + 2*time.Second):
		t.Fatal("exitFunc was not called — a wedged resource cleanup must force exit")
	}
}

// TestCleanupAll_NilProgramIsSafe: with no program wired, cleanupAll is a
// no-op and exitFunc is never called.
func TestCleanupAll_NilProgramIsSafe(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 20 * time.Millisecond

	exitFired := make(chan struct{}, 1)
	m.exitFunc = func(int) { exitFired <- struct{}{} }

	m.program = nil
	assert.NotPanics(t, func() { m.cleanupAll() })

	select {
	case <-exitFired:
		t.Fatal("exitFunc must not fire without a program")
	case <-time.After(m.shutdownTimeout + 50*time.Millisecond):
	}
}

// TestCleanupAll_WedgedStdoutFiresExit: the realistic case. The renderer is
// stuck on a wedged stdout write, and once tea.Quit fires the final flush
// would itself re-acquire the same mutex — a hard deadlock. Wait() never
// returns and ReleaseTerminal would block too; exitFunc must still fire.
func TestCleanupAll_WedgedStdoutFiresExit(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 300 * time.Millisecond

	exitDone := make(chan struct{})
	m.exitFunc = func(int) { close(exitDone) }

	p, w, _ := initBlockingBubbletea(t, &quitModel{})
	defer w.unblock()

	m.program = p
	m.cleanupAll()

	// Drive the program into the deadlock path: tea.Quit triggers the final
	// render flush against the wedged writer, which is the actual upstream
	// bug the safety net guards against.
	p.Send(triggerQuitMsg{})

	select {
	case <-exitDone:
	case <-time.After(m.shutdownTimeout + 2*time.Second):
		t.Fatal("exitFunc was not called — safety net is blocked by ReleaseTerminal")
	}
}

// TestCleanupAll_MultipleCallsFireExitOnce: cleanupAll is invoked from
// several message handlers (ExitSessionMsg, ExitConfirmedMsg, …) and may
// run more than once on the same model. Each safety-net goroutine snapshots
// exitFunc, so without a guard each one would call exit(0) on timeout —
// fine in production where exit is os.Exit, fatal in tests where it's a
// channel close.
func TestCleanupAll_MultipleCallsFireExitOnce(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.shutdownTimeout = 100 * time.Millisecond

	exitFired := make(chan struct{}, 3)
	m.exitFunc = func(int) { exitFired <- struct{}{} }

	m.program = tea.NewProgram(&quitModel{})

	m.cleanupAll()
	m.cleanupAll()
	m.cleanupAll()

	// Exactly one safety net must fire: wait for it, then make sure no
	// second one follows.
	select {
	case <-exitFired:
	case <-time.After(m.shutdownTimeout + 2*time.Second):
		t.Fatal("no safety net fired")
	}
	select {
	case <-exitFired:
		t.Fatal("only the first cleanupAll should arm a safety net")
	case <-time.After(m.shutdownTimeout + 200*time.Millisecond):
	}
}

// TestExitDeadlock_BlockedStdout proves the underlying bubbletea bug: Run()
// hangs when stdout blocks during the final render after tea.Quit.
func TestExitDeadlock_BlockedStdout(t *testing.T) {
	t.Parallel()

	model := &quitModel{}
	p, w, runDone := initBlockingBubbletea(t, model)

	p.Send(triggerQuitMsg{})

	select {
	case <-runDone:
		t.Skip("bubbletea returned without deadlocking; upstream fix may have landed")
	case <-time.After(2 * time.Second):
	}

	w.unblock()
}

// TestExitSafetyNet_BlockedStdout: with a wedged renderer, an external
// safety-net (simulated here in onQuit) must force the process to exit.
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
			time.AfterFunc(safetyNetTimeout, func() { testExitFunc(0) })
		},
	}
	p, w, runDone := initBlockingBubbletea(t, model)
	defer w.unblock()

	p.Send(triggerQuitMsg{})

	select {
	case code := <-exitDone:
		assert.True(t, exitCalled.Load())
		assert.Equal(t, 0, code)
	case <-runDone:
		// Run() returned on its own — also acceptable.
	case <-time.After(safetyNetTimeout + 2*time.Second):
		t.Fatal("neither Run() returned nor safety-net exitFunc fired")
	}
}

// TestExitSafetyNet_GracefulShutdown: on a clean shutdown, Run() must return
// before the safety net fires.
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
			time.AfterFunc(safetyNetTimeout, func() { testExitFunc(0) })
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

	// Send blocks until the program is running, so the quit message below
	// is processed by a fully started event loop.
	p.Send(syncMsg{})

	p.Send(triggerQuitMsg{})

	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("p.Run() did not return")
	}

	mu.Lock()
	assert.True(t, cleanupCalled)
	mu.Unlock()
	assert.False(t, exitCalled.Load(), "exitFunc must not fire on graceful shutdown")
}
