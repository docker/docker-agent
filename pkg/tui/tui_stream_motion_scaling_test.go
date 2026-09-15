package tui

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type (
	streamingMotionAck   struct{ done chan struct{} }
	streamingMotionRead  struct{ content chan string }
	streamingMotionModel struct {
		root                *appModel
		ready               chan struct{}
		once                sync.Once
		mu                  sync.Mutex
		chunks, motions     atomic.Uint64
		views, compositions atomic.Uint64
	}
)

func (m *streamingMotionModel) Init() tea.Cmd { return nil }
func (m *streamingMotionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch msg := msg.(type) {
	case *agentruntime.AgentChoiceEvent:
		m.chunks.Add(1)
	case tea.MouseMotionMsg:
		m.motions.Add(1)
	case streamingMotionAck:
		close(msg.done)
		return m, nil
	case streamingMotionRead:
		msg.content <- m.root.View().Content
		return m, nil
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *streamingMotionModel) View() tea.View {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.views.Add(1)
	if !m.root.viewCacheValid {
		m.compositions.Add(1)
	}
	v := m.root.View()
	m.once.Do(func() { close(m.ready) })
	return v
}

// waitForProgramQuiescence blocks until the program's view/composition/write
// counters have been unchanged for a full settle window. The settle window
// must comfortably exceed the renderer's tick period (tea's default FPS is
// 60, i.e. ~16.7ms): under -race with the full suite's package-level
// parallelism, the render ticker's goroutine can be scheduled several tick
// periods late, so a composition that already happened may not yet have been
// flushed to the writer when a short window would call it stable. A caller
// that samples counters right after that premature "stable" reading (as a
// baseline to diff against later) then attributes the delayed flush to
// whatever happens next, inflating its write count. 300ms is ~18 tick
// periods, giving the ticker goroutine ample room to catch up before we
// trust a reading.
func waitForProgramQuiescence(t *testing.T, model *streamingMotionModel, writer *wallClockCountingWriter) {
	t.Helper()
	const settleWindow = 300 * time.Millisecond
	lastViews, lastCompositions, lastWrites := model.views.Load(), model.compositions.Load(), writer.writes.Load()
	stableSince := time.Now()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			views, compositions, writes := model.views.Load(), model.compositions.Load(), writer.writes.Load()
			if views != lastViews || compositions != lastCompositions || writes != lastWrites {
				lastViews, lastCompositions, lastWrites = views, compositions, writes
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= settleWindow {
				return
			}
		case <-timeout.C:
			t.Fatal("Bubble Tea program did not quiesce")
		}
	}
}

func TestActualProgramLongStreamMotionWorkIsViewportBounded(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	sess, _, _ := mixedHistorySession(1000)
	root.application.Session().Messages = sess.Messages
	_ = root.activeTab.chatPage.Init()
	root.handleWindowResize(120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	_ = root.View()
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	program := startStreamingMotionProgram(t, model, tea.WithOutput(writer))
	waitForProgramQuiescence(t, model, writer)

	chunk := "Paragraph with **markdown**, `code`, Unicode λ界, and a [link](https://example.com).\n\n"
	var earlyViews, earlyCompositions uint64
	for i := range 240 {
		program.Send(agentruntime.AgentChoice("root", "profile", chunk))
		program.Send(tea.MouseMotionMsg{X: 50 + i%2, Y: 20})
		if i == 39 || i == 199 {
			ack := make(chan struct{})
			program.Send(streamingMotionAck{done: ack})
			<-ack
			if i == 39 {
				earlyViews, earlyCompositions = model.views.Load(), model.compositions.Load()
			}
		}
	}
	ack := make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	require.Equal(t, uint64(240), model.chunks.Load())
	require.Equal(t, uint64(240), model.motions.Load())
	lateViews := model.views.Load() - earlyViews
	lateCompositions := model.compositions.Load() - earlyCompositions
	require.LessOrEqual(t, lateViews, uint64(420), "views remain bounded by 400 late input events")
	require.LessOrEqual(t, lateCompositions, uint64(420), "root compositions remain bounded by late input events")
	require.Positive(t, writer.writes.Load(), "stream remains writer-visible")
	views := model.views.Load()
	waitForProgramQuiescence(t, model, writer)
	require.Equal(t, views, model.views.Load(), "program quiesces after stream input")
}
