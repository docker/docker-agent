// Steering end-to-end scenarios (issue #3547): messages sent while the agent
// is streaming attach to the ongoing stream by default, and the /settings
// Behavior tab switches busy sends to end-of-turn queueing instead.
//
// Both tests replay their cassette through the proxy's simulated-stream mode:
// the first answer streams one character per chunk with a real delay, so the
// test has a wide, deterministic window to interact with the TUI mid-stream.

package tui_test

import (
	"os"
	"runtime"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/fake"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// steeringProxyOptions slows the SSE replay enough that submitting a
// follow-up while the first answer is still streaming is deterministic even
// on slow CI and under -race (the first answer streams one character per
// chunk, so the window is about 4 seconds wide).
func steeringProxyOptions() *fake.ProxyOptions {
	return &fake.ProxyOptions{
		SimulateStream:   true,
		StreamChunkDelay: 250 * time.Millisecond,
	}
}

// newStreamingTUI builds the steering harness and, on Windows only, raises
// the WaitFor deadline to 30s: the simulated stream replays slower there
// (issue #3983). Applied after construction because newTUIWithProxyOptions
// does not forward tuitest options; it is safe before any interaction.
func newStreamingTUI(t *testing.T) *tuitest.Driver {
	t.Helper()
	d := newTUIWithProxyOptions(t, "testdata/basic.yaml", 120, 40, steeringProxyOptions())
	if runtime.GOOS == "windows" {
		tuitest.WithTimeout(30 * time.Second)(d)
	}
	return d
}

// TestChat_SteerWhileStreaming submits a second message while the agent is
// still streaming its first answer. The message must be steered into the
// ongoing stream: the steering toast appears, no queue toast is shown, and
// once the runtime drains the message the transcript shows the injected user
// bubble followed by the agent's answer to it.
func TestChat_SteerWhileStreaming(t *testing.T) {
	d := newStreamingTUI(t)

	// Draft the follow-up as a single paste so it costs one Update instead of
	// one per keystroke (keystrokes are expensive under -race and would eat
	// into the streaming window). Submission waits until chunks are visibly
	// streaming.
	d.Type("What's 2+2?").
		Enter().
		Send(tea.PasteMsg{Content: "Also, what's 3+3?"}).
		WaitFor(tuitest.Contains("What's 2+2?")).
		// First chunks visible: the model call is in flight, the stream is live.
		WaitFor(tuitest.Contains("2 +"))

	// Plain Enter while the agent is working steers into the ongoing stream.
	d.Enter().
		WaitFor(tuitest.Contains("Message sent to the working agent")).
		Assert(tuitest.Absent("Message queued"))

	// The runtime drains the steered message at the end of the model call,
	// emits it as a user message, and answers it in the same stream.
	d.WaitFor(tuitest.Contains("2 + 2 equals 4.")).
		WaitFor(tuitest.Contains("Also, what's 3+3?")).
		WaitFor(tuitest.Contains("3 + 3 equals 6."))
}

// TestChat_QueueSendModeWhileStreaming switches the send mode to Queue via
// the /settings dialog, then submits a second message while the agent is
// still streaming. The message must go to the local queue (queue toast, no
// steering toast) and only be dispatched once the first stream stops,
// producing a second turn with its own answer. The switch must also be
// persisted to the user config.
func TestChat_QueueSendModeWhileStreaming(t *testing.T) {
	d := newStreamingTUI(t)

	// Flip the send mode on the Behavior tab of /settings: open the dialog,
	// switch tab, cycle Steer → Queue, apply.
	d.Send(tea.PasteMsg{Content: "/settings"}).
		Enter().
		WaitFor(tuitest.Contains("Sidebar position")).
		Press(tea.KeyTab).
		WaitFor(tuitest.Contains("While agent is working")).
		Press(tea.KeyRight).
		WaitFor(tuitest.Contains("● Queue")).
		Enter().
		WaitFor(tuitest.Contains("Settings updated"))

	// The choice is persisted for future sessions.
	cfg, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	assert.Contains(t, string(cfg), "busy_send_mode: queue")

	d.Type("What's 2+2?").
		Enter().
		Send(tea.PasteMsg{Content: "Also, what's 3+3?"}).
		WaitFor(tuitest.Contains("What's 2+2?")).
		WaitFor(tuitest.Contains("2 +"))

	// With the queue send mode, a plain Enter queues instead of steering.
	d.Enter().
		WaitFor(tuitest.Contains("Message queued (1 waiting)")).
		Assert(tuitest.Absent("Message sent to the working agent"))

	// The queued message is processed only after the first stream stops.
	d.WaitFor(tuitest.Contains("2 + 2 equals 4.")).
		WaitFor(tuitest.Contains("Also, what's 3+3?")).
		WaitFor(tuitest.Contains("3 + 3 equals 6."))
}
