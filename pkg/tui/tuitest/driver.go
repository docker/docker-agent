// Package tuitest is a black-box end-to-end harness for the docker-agent TUI.
//
// It drives the real top-level Bubble Tea model through a real tea.Program so
// that the full event path — keyboard input, the chat page, the runtime event
// stream, and the session supervisor that routes events back via
// program.Send — is exercised exactly as in production. The only thing it
// fakes is the terminal: the renderer is disabled and each rendered frame is
// captured inside the model's Update (which runs on the program's single
// message-loop goroutine), so frame capture is race-free and independent of
// renderer timing.
//
// The result is a fluent, Selenium-like DSL without Selenium's flakiness:
//
//	tuitest.New(t, model, 120, 40).
//	    Type("What's 2+2?").
//	    Enter().
//	    WaitFor(tuitest.Contains("4")).
//	    AssertGolden("basic_math")
//
// Synchronization is expressed through WaitFor: it polls the latest captured
// frame until a Matcher passes or a deadline elapses. Because runtime events
// arrive asynchronously (the agent streams its answer), tests wait for visible
// outcomes rather than sleeping.
package tuitest

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// defaultWaitTimeout bounds WaitFor polling. Generous because cassette replay
// still streams chunk-by-chunk through the runtime.
const defaultWaitTimeout = 10 * time.Second

// pollInterval is how often WaitFor re-checks the latest frame.
const pollInterval = 5 * time.Millisecond

// defaultStopTimeout bounds how long stop waits for the program's run loop to
// exit after Quit before declaring the shutdown failed.
const defaultStopTimeout = 5 * time.Second

// programStarter is the subset of *tea.Program the driver relies on. It lets
// tests inject a fake in the harness's own unit tests.
type programStarter interface {
	Run() (tea.Model, error)
	Send(msg tea.Msg)
	Quit()
	Wait()
}

// Driver drives a TUI model and exposes a fluent assertion API. Every method
// that advances state returns the Driver so calls can be chained. Assertion
// failures are reported through testing.TB and stop the test.
type Driver struct {
	tb        testing.TB
	program   programStarter
	frames    *frameStore
	sink      frameSink
	clipboard *clipboardStore

	// cleanupModel releases model-managed resources after the program has
	// stopped; nil when the model doesn't implement resourceCleaner.
	cleanupModel func()

	waitTimeout time.Duration
	// stopTimeout bounds stop's wait for the run loop to exit. Unit tests of
	// the harness shrink it to exercise the timeout path quickly.
	stopTimeout time.Duration
	runDone     chan struct{}
}

// frameStore accumulates ANSI-stripped frames captured on the program
// goroutine. It is read from the test goroutine, hence the mutex.
type frameStore struct {
	mu     sync.Mutex
	frames []string
}

func (s *frameStore) push(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame)
}

// latest returns the most recently captured frame, or "" if none yet.
func (s *frameStore) latest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return ""
	}
	return s.frames[len(s.frames)-1]
}

func (s *frameStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// Option configures a Driver.
type Option func(*Driver)

// WithTimeout overrides the default WaitFor deadline.
func WithTimeout(d time.Duration) Option {
	return func(dr *Driver) { dr.waitTimeout = d }
}

// programReady is the interface the inner TUI model implements so the
// supervisor can route events back through the running program. tui.New's
// model satisfies it via SetProgram.
type programReady interface {
	SetProgram(p *tea.Program)
}

// resourceCleaner is the optional interface a model can implement to release
// resources it manages outside the Bubble Tea loop (state stores, watchers,
// supervisors). The driver invokes it once the program has fully stopped, so
// files those resources hold open (e.g. a SQLite store under t.TempDir) are
// closed before the test's temp dirs are removed — on Windows an open handle
// makes that removal fail. tui.New's model satisfies it via CleanupForTesting,
// which must be idempotent.
type resourceCleaner interface {
	CleanupForTesting()
}

// New starts model in a real, renderer-less tea.Program and returns a Driver.
// width and height seed the initial window size. The program is stopped and
// awaited automatically via t.Cleanup.
//
// model is typically the value returned by tui.New. If it implements
// SetProgram (as the docker-agent TUI does) the running program is wired into
// it so the session supervisor can deliver routed runtime events.
func New(tb testing.TB, model tea.Model, width, height int, opts ...Option) *Driver {
	tb.Helper()

	sink := newDebugSink(tb)
	clipboard, restoreClipboard := newClipboardStore()
	store := &frameStore{}
	capture := &captureModel{inner: model, frames: store, sink: sink}

	// WithoutRenderer: we never want the framework to touch a terminal. Frames
	// are captured in captureModel.Update instead, which is deterministic.
	// WithoutSignals/WithoutCatchPanics keep test output clean and let panics
	// surface to the test runner.
	p := tea.NewProgram(capture,
		tea.WithoutRenderer(),
		tea.WithoutSignals(),
		tea.WithoutCatchPanics(),
		tea.WithInput(nil),
		tea.WithWindowSize(width, height),
	)

	if pr, ok := model.(programReady); ok {
		pr.SetProgram(p)
	}

	d := &Driver{
		tb:          tb,
		program:     p,
		frames:      store,
		sink:        sink,
		clipboard:   clipboard,
		waitTimeout: defaultWaitTimeout,
		stopTimeout: defaultStopTimeout,
		runDone:     make(chan struct{}),
	}
	if rc, ok := model.(resourceCleaner); ok {
		d.cleanupModel = rc.CleanupForTesting
	}
	for _, opt := range opts {
		opt(d)
	}

	// tuitest.New intentionally returns a running driver and registers cleanup with testing.TB.
	go func() { //rubocop:disable Lint/ConstructorPurity
		defer close(d.runDone)
		_, _ = p.Run()
	}()

	// A blocking Send guarantees Run() has started before we return, so later
	// sends are never dropped. The window size also flips the model to its
	// ready state.
	p.Send(tea.WindowSizeMsg{Width: width, Height: height})

	tb.Cleanup(func() {
		d.stop()
		restoreClipboard()
	})
	return d
}

// stop quits the program and waits for the run loop to return so background
// goroutines (supervisor subscriptions, streaming) are torn down before the
// test ends. Model-managed resources are released only once that wait
// confirms the run loop exited: they must not be closed under a still-running
// program, so on timeout the cleanup hook is skipped and the resources leak
// into the failed test.
func (d *Driver) stop() {
	d.program.Quit()
	select {
	case <-d.runDone:
		if d.cleanupModel != nil {
			d.cleanupModel()
		}
	case <-time.After(d.stopTimeout):
		d.tb.Errorf("tuitest: program did not shut down within %s", d.stopTimeout)
	}
	if d.sink != nil {
		if err := d.sink.err(); err != nil {
			d.tb.Errorf("tuitest: debug frame output failed: %v", err)
		}
	}
}

// sendSync delivers a message and blocks until the program has processed it,
// so a following Assert observes the post-message frame rather than racing
// the program's goroutine. It relies on the message loop being FIFO: a
// barrier sentinel sent right after msg is only handled once msg's Update —
// and thus its frame capture — has completed. Counting frames instead would
// race with frames produced by startup or internal messages. The sentinel is
// sent separately rather than wrapping msg so the program still applies its
// internal handling of special messages (window size, quit, ...). Commands
// returned by msg still complete asynchronously; use WaitFor for their effects.
func (d *Driver) sendSync(msg tea.Msg) {
	d.tb.Helper()

	d.program.Send(msg)
	done := make(chan struct{})
	d.program.Send(syncMsg{done: done})
	select {
	case <-done:
	case <-d.runDone:
		// The program terminated before the barrier was processed; there is
		// no later frame to wait for.
	case <-time.After(d.waitTimeout):
		d.tb.Fatalf("tuitest: timed out after %s waiting for %T to be processed", d.waitTimeout, msg)
	}
}

// Resize sends a new terminal size.
func (d *Driver) Resize(width, height int) *Driver {
	d.sendSync(tea.WindowSizeMsg{Width: width, Height: height})
	return d
}

// Type sends s to the model one key press at a time, mirroring real typing.
func (d *Driver) Type(s string) *Driver {
	for _, r := range s {
		d.sendSync(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return d
}

// Press sends a single key by its code (e.g. tea.KeyEnter, tea.KeyEsc) with
// optional modifiers.
func (d *Driver) Press(code rune, mods ...tea.KeyMod) *Driver {
	var mod tea.KeyMod
	for _, m := range mods {
		mod |= m
	}
	d.sendSync(tea.KeyPressMsg{Code: code, Mod: mod})
	return d
}

// Enter is shorthand for Press(tea.KeyEnter).
func (d *Driver) Enter() *Driver { return d.Press(tea.KeyEnter) }

// Send delivers an arbitrary message to the model. Useful for injecting
// runtime events or custom messages a scenario can't reach through the
// keyboard alone.
func (d *Driver) Send(msg tea.Msg) *Driver {
	d.sendSync(msg)
	return d
}

// Frame returns the most recently captured, ANSI-stripped frame.
func (d *Driver) Frame() string {
	return d.frames.latest()
}

// WaitFor polls the latest frame until m passes or the timeout elapses. On
// timeout it fails the test with the last frame for context.
func (d *Driver) WaitFor(m Matcher) *Driver {
	d.tb.Helper()

	deadline := time.Now().Add(d.waitTimeout)
	for time.Now().Before(deadline) {
		if m.match(d.frames.latest()) {
			return d
		}
		time.Sleep(pollInterval)
	}

	d.tb.Fatalf("tuitest: timed out after %s waiting for %s\n--- last frame ---\n%s",
		d.waitTimeout, m.describe(), d.frames.latest())
	return d
}

// WaitForStable blocks until no new frame has been captured for quiet, then
// returns. Use it before AssertGolden so the snapshot is taken once streaming
// has settled. It still respects the overall wait timeout.
func (d *Driver) WaitForStable(quiet time.Duration) *Driver {
	d.tb.Helper()

	deadline := time.Now().Add(d.waitTimeout)
	last := d.frames.count()
	stableSince := time.Now()

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		if n := d.frames.count(); n != last {
			last = n
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= quiet {
			return d
		}
	}
	return d
}

// Assert immediately checks m against the latest frame without waiting.
func (d *Driver) Assert(m Matcher) *Driver {
	d.tb.Helper()
	if !m.match(d.frames.latest()) {
		d.tb.Fatalf("tuitest: assertion failed: %s\n--- frame ---\n%s",
			m.describe(), d.frames.latest())
	}
	return d
}

// captureModel wraps the real model and records each frame after Update. It
// runs entirely on the program's message-loop goroutine, so reading inner and
// appending frames needs no locking here (frameStore guards cross-goroutine
// reads).
type captureModel struct {
	inner  tea.Model
	frames *frameStore
	sink   frameSink
}

func (c *captureModel) recordFrame() {
	frame := stripFrame(c.inner.View())
	// Feed the sink before the store: the store push is what sendSync/WaitFor
	// synchronize on, so any sink side effect (dump file, live output) must be
	// complete by the time a test observes the frame and inspects that output.
	if c.sink != nil {
		c.sink.frame(frame)
	}
	c.frames.push(frame)
}

func (c *captureModel) Init() tea.Cmd {
	cmd := c.inner.Init()
	c.recordFrame()
	return cmd
}

// syncMsg is the barrier sentinel used by sendSync. captureModel acks it
// without touching the inner model or recording a frame.
type syncMsg struct{ done chan struct{} }

func (c *captureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if s, ok := msg.(syncMsg); ok {
		close(s.done)
		return c, nil
	}
	inner, cmd := c.inner.Update(msg)
	c.inner = inner
	c.recordFrame()
	return c, cmd
}

func (c *captureModel) View() tea.View {
	return c.inner.View()
}

// stripFrame renders a tea.View to plain text with ANSI escape codes removed
// and trailing whitespace on each line trimmed, so assertions and goldens are
// stable across color profiles and padding.
func stripFrame(v tea.View) string {
	plain := ansi.Strip(v.Content)
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}
