package tuitest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// echoModel is a tiny model used to unit-test the harness without pulling in
// the real TUI. It echoes typed runes into a buffer, clears on Enter, and
// renders the buffer prefixed with "echo: ". A delayed command lets us prove
// WaitFor polls asynchronously delivered frames.
type echoModel struct {
	buf     string
	delayed string
}

func (m *echoModel) Init() tea.Cmd { return nil }

type delayedMsg struct{ text string }

func (m *echoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.Code {
		case tea.KeyEnter:
			m.buf = ""
		case 'x':
			// Pressing x schedules a message that lands a bit later, so a
			// WaitFor must poll rather than read a single synchronous frame.
			return m, tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg {
				return delayedMsg{text: "later"}
			})
		default:
			m.buf += msg.Text
		}
	case delayedMsg:
		m.delayed = msg.text
	case tea.MouseClickMsg:
		m.buf = "clicked"
	case tea.MouseMotionMsg:
		m.delayed = "moved"
	}
	return m, nil
}

func (m *echoModel) View() tea.View {
	return tea.NewView("echo: " + m.buf + " " + m.delayed)
}

func TestDriver_TypeAndAssert(t *testing.T) {
	d := New(t, &echoModel{}, 80, 24)
	d.Type("hi").Assert(Contains("echo: hi"))
}

func TestDriver_EnterClearsBuffer(t *testing.T) {
	d := New(t, &echoModel{}, 80, 24)
	d.Type("hello").
		Assert(Contains("hello")).
		Enter().
		Assert(Absent("hello"))
}

func TestDriver_WaitForPollsAsyncFrames(t *testing.T) {
	d := New(t, &echoModel{}, 80, 24, WithTimeout(2*time.Second))
	// The delayed message only arrives via tea.Tick, so a synchronous Assert
	// would miss it — WaitFor must poll until the frame updates.
	d.Press('x').WaitFor(Contains("later"))
}

// quitModel quits the program on 'q'. It guards against sendSync deadlocking
// when the sent message terminates the program before the barrier sentinel
// is processed.
type quitModel struct{ echoModel }

func (m *quitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.Code == 'q' {
		return m, tea.Quit
	}
	// echoModel.Update mutates the embedded value in place.
	_, cmd := m.echoModel.Update(msg)
	return m, cmd
}

func TestDriver_SendSyncReturnsWhenMessageQuitsProgram(t *testing.T) {
	d := New(t, &quitModel{}, 80, 24, WithTimeout(2*time.Second))
	d.Press('q') // must not hang or fail even though no frame follows
}

// cleanupModel is a minimal model whose CleanupForTesting records when the
// driver released model-managed resources and whether the program's run loop
// had already exited at that point. The real TUI model owns OS resources
// (the tui_state.db SQLite store) that must be closed after the program
// stopped but before t.TempDir removal, which fails on Windows while the
// file is still open.
type cleanupModel struct {
	runDone <-chan struct{}

	// Written during the subtest's cleanup and read by the parent test after
	// t.Run returned, so plain fields are safe.
	calls        int
	afterRunExit bool
}

func (m *cleanupModel) Init() tea.Cmd                       { return nil }
func (m *cleanupModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m *cleanupModel) View() tea.View                      { return tea.NewView("cleanup") }

func (m *cleanupModel) CleanupForTesting() {
	m.calls++
	select {
	case <-m.runDone:
		m.afterRunExit = true
	default:
	}
}

func TestDriver_StopReleasesModelResources(t *testing.T) {
	m := &cleanupModel{}
	t.Run("session", func(t *testing.T) {
		d := New(t, m, 80, 24)
		m.runDone = d.runDone
	})
	// The subtest's cleanups have run, so stop() must have invoked the hook
	// exactly once — and only once the run loop had exited, because resources
	// must not be closed under a still-running program.
	if m.calls != 1 {
		t.Fatalf("CleanupForTesting calls = %d, want 1", m.calls)
	}
	if !m.afterRunExit {
		t.Fatal("CleanupForTesting ran before the program's run loop exited")
	}
}

// wedgedProgram is a programStarter whose Quit has no effect, standing in for
// a Bubble Tea program whose run loop never exits during Driver.stop.
type wedgedProgram struct{}

func (wedgedProgram) Run() (tea.Model, error) { return nil, nil }
func (wedgedProgram) Send(tea.Msg)            {}
func (wedgedProgram) Quit()                   {}
func (wedgedProgram) Wait()                   {}

// recordingTB captures Error/Errorf calls so a test can assert on stop's
// failure reporting without failing itself.
type recordingTB struct {
	testing.TB

	errors []string
}

func (r *recordingTB) Error(args ...any) { r.errors = append(r.errors, fmt.Sprint(args...)) }
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestDriver_StopSkipsCleanupWhenRunLoopHangs(t *testing.T) {
	tb := &recordingTB{TB: t}
	cleaned := false
	d := &Driver{
		tb:           tb,
		program:      wedgedProgram{},
		cleanupModel: func() { cleaned = true },
		stopTimeout:  10 * time.Millisecond,
		runDone:      make(chan struct{}), // never closed: the run loop hangs
	}

	d.stop()

	// Releasing model resources under a possibly still-running program races
	// the Bubble Tea loop, so a timed-out stop must skip the cleanup hook.
	if cleaned {
		t.Fatal("stop released model resources although the run loop never exited")
	}
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "did not shut down") {
		t.Fatalf("stop errors = %q, want a single shutdown-timeout report", tb.errors)
	}
}

func TestDriver_MouseHelpers(t *testing.T) {
	d := New(t, &echoModel{}, 80, 24)
	d.Type("target")

	x, y := d.MustFindText("target")
	if x != len("echo: ") || y != 0 {
		t.Fatalf("MustFindText returned (%d,%d), want (%d,0)", x, y, len("echo: "))
	}

	d.MoveMouseToText("target").WaitFor(Contains("moved"))
	d.ClickText("target").WaitFor(Contains("clicked"))
}

func TestDriver_ClipboardAssertions(t *testing.T) {
	d := New(t, &echoModel{}, 80, 24)
	d.clipboard.mu.Lock()
	d.clipboard.values = append(d.clipboard.values, "hello clipboard")
	d.clipboard.mu.Unlock()

	d.WaitForClipboard("clipboard").AssertClipboard(Contains("hello"))
}

func TestMatchers(t *testing.T) {
	t.Parallel()

	const frame = "the quick brown fox"
	cases := []struct {
		name string
		m    Matcher
		want bool
	}{
		{"contains hit", Contains("quick"), true},
		{"contains miss", Contains("slow"), false},
		{"containsAll hit", ContainsAll("quick", "fox"), true},
		{"containsAll miss", ContainsAll("quick", "cat"), false},
		{"absent hit", Absent("cat"), true},
		{"absent miss", Absent("fox"), false},
		{"matches hit", Matches("br[ow]+n"), true},
		{"matches miss", Matches("^fox"), false},
		{"not inverts", Not(Contains("cat")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.m.match(frame); got != tc.want {
				t.Errorf("%s.match(%q) = %v, want %v", tc.m.describe(), frame, got, tc.want)
			}
		})
	}
}

func TestStripFrame(t *testing.T) {
	t.Parallel()

	// ANSI bold red around "hi", with trailing padding that must be trimmed.
	v := tea.NewView("\x1b[1;31mhi\x1b[0m   \nworld   ")
	got := stripFrame(v)
	want := "hi\nworld"
	if got != want {
		t.Errorf("stripFrame = %q, want %q", got, want)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestDebugFramesDump(t *testing.T) {
	origLive := *liveFrames
	origDump := *dumpFrames
	t.Cleanup(func() {
		*liveFrames = origLive
		*dumpFrames = origDump
	})
	*liveFrames = false
	*dumpFrames = true

	dir := filepath.Join(goldenDir, "frames", safeTestName(t.Name()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	d := New(t, &echoModel{}, 80, 24)
	d.Type("x").WaitFor(Contains("later"))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading frame dump dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("frame dump dir is empty")
	}

	last, err := os.ReadFile(filepath.Join(dir, entries[len(entries)-1].Name()))
	if err != nil {
		t.Fatalf("reading last frame: %v", err)
	}
	if !bytes.Contains(last, []byte("later")) {
		t.Fatalf("last dumped frame = %q, want to contain delayed text", last)
	}
}

func TestDebugLiveFrames(t *testing.T) {
	origLive := *liveFrames
	origDump := *dumpFrames
	liveMu.Lock()
	origWriter := liveWriter
	var out lockedBuffer
	liveWriter = &out
	liveMu.Unlock()
	t.Cleanup(func() {
		*liveFrames = origLive
		*dumpFrames = origDump
		liveMu.Lock()
		defer liveMu.Unlock()
		liveWriter = origWriter
	})
	*liveFrames = true
	*dumpFrames = false

	d := New(t, &echoModel{}, 80, 24)
	d.Type("hi")

	got := out.String()
	if !strings.Contains(got, "--- tuitest frame") {
		t.Fatalf("live output = %q, want frame header", got)
	}
	if !strings.Contains(got, "echo: hi") {
		t.Fatalf("live output = %q, want latest frame content", got)
	}
}

func TestSafeTestName(t *testing.T) {
	t.Parallel()

	got := safeTestName(`TestFoo/bar baz:*`)
	want := "TestFoo_bar_baz__"
	if got != want {
		t.Errorf("safeTestName = %q, want %q", got, want)
	}
	if got := safeTestName(".."); got != "__" {
		t.Errorf("safeTestName for dots = %q, want __", got)
	}
}

func TestAssertGolden_UpdateThenCompare(t *testing.T) {
	// Point the golden at this package's testdata dir (created on demand).
	orig := *updateGolden
	t.Cleanup(func() { *updateGolden = orig })

	name := "unit_golden"
	path := filepath.Join(goldenDir, name+".golden")
	t.Cleanup(func() { _ = os.Remove(path) })

	*updateGolden = true
	assertGolden(t, name, "frame-A")

	*updateGolden = false
	assertGolden(t, name, "frame-A") // matches → no failure
}
