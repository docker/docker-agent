package main

import (
	"path/filepath"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
)

func TestTUIAnimationDuration(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, TUIAnimationDuration, coptest.ProgramFiles{
		"go.mod": `module github.com/docker/docker-agent

go 1.27

require charm.land/bubbletea/v2 v2.0.0
replace charm.land/bubbletea/v2 => ./bubbletea
`,
		"bubbletea/go.mod": "module charm.land/bubbletea/v2\n\ngo 1.27\n",
		"bubbletea/tea.go": `package tea
import "time"
type Msg any
type Cmd func() Msg
type TickMsg struct{ Frame int }
func Tick(time.Duration, func(time.Time) Msg) Cmd { return nil }
func Every(time.Duration, func(time.Time) Msg) Cmd { return nil }
`,
		"pkg/tui/animation/animation.go": `package animation
import "time"
type TickMsg struct{ Frame int }
func (m TickMsg) MarkDirty() {}
func (m TickMsg) ElapsedBounds() (time.Duration, time.Duration) { return 0, 0 }
func TimedStep(a, b time.Duration) int { return int(a/b) }
func FrameAt(a time.Duration) string { return "" }
func ProgressAt(a time.Duration) float64 { return 0 }
func Legacy(m TickMsg) int { m.Frame++; return m.Frame }
`,
		"pkg/tui/components/effects.go": `package components
import (
	"strconv"
	"strings"
	"time"
	bt "charm.land/bubbletea/v2"
	anim "github.com/docker/docker-agent/pkg/tui/animation"
)
type sample struct{ counter, railFrame, bottomSlack, position, voicePhase, selection int }
var frames = []string{"-", "|"}
func legacy(tick *anim.TickMsg) string { return frames[tick.Frame%len(frames)] }
func (m *sample) update(msg bt.Msg) {
	switch msg.(type) {
	case anim.TickMsg:
		m.counter++
	}
	if tick, ok := msg.(anim.TickMsg); ok && m.counter > 0 {
		m.railFrame++
		tick.MarkDirty()
	}
}
func (m *sample) slack(tick anim.TickMsg) {
	if m.bottomSlack > 0 {
		m.bottomSlack--
		tick.MarkDirty()
	}
}
func (m *sample) guardedSlack(tick anim.TickMsg) {
	if m.bottomSlack > 0 {
		m.bottomSlack = m.bottomSlack - 1
		m.selection++ // Not used in rendering, despite MarkDirty.
	}
	tick.MarkDirty()
}
func (m *sample) View() string {
	return frames[m.counter%len(frames)] + strconv.Itoa(1+m.bottomSlack)
}
type voiceTick struct{}
func (m *sample) voice(msg bt.Msg) bt.Cmd {
	switch msg.(type) {
	case voiceTick:
		m.voicePhase = (1+m.voicePhase)%len(frames)
		_ = strings.Repeat("·", m.voicePhase)
		return bt.Every(time.Second, func(time.Time) bt.Msg { return voiceTick{} })
	}
	return nil
}
func (m *sample) ticker() {
	t := time.NewTicker(time.Second)
	for range t.C {
		m.position += 2
		_ = frames[m.position%len(frames)]
	}
}
`,
		"pkg/tui/components/sibling.go": `package components
import anim "github.com/docker/docker-agent/pkg/tui/animation"
func (m *sample) siblingSlack(tick anim.TickMsg, redraw bool) {
	if redraw {
		tick.MarkDirty()
	} else {
		m.bottomSlack-- // MarkDirty is in the opposite branch.
	}
	if m.bottomSlack > 0 {
		m.bottomSlack-- // MarkDirty is in a different conditional.
	}
}
`,
		"pkg/board/tui/update.go": `package board
import (
	"time"
	tea "charm.land/bubbletea/v2"
)
type model struct{ frame int }
type tickMsg struct{}
func tick() tea.Cmd { return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }
func (m *model) Update(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case tickMsg:
		m.frame++
		return tick()
	}
	return nil
}
`,
		"pkg/board/tui/view.go": `package board
var spinnerFrames = []string{"-", "|"}
func (m *model) View() string {
	index := m.frame % len(spinnerFrames)
	return spinnerFrames[index]
}
`,
		"pkg/leantui/lean.go": `package lean
import (
	"time"
	"github.com/docker/docker-agent/pkg/leantui/ui"
)
type model struct{ screen ui.Screen; frame, width int }
func (m *model) render() { _ = m.screen.Frame(1, 2, m.frame) }
func (m *model) run() {
	t, resize := time.NewTicker(time.Second), time.NewTicker(2*time.Second)
	for {
		select {
		case <-t.C:
			m.frame++
			m.render()
		case <-resize.C:
			m.width++
			m.render()
		}
	}
}
`,
		"pkg/leantui/ui/screen.go": `package ui
type Screen struct{}
func (Screen) Frame(_, _, phase int) string { return []string{"-", "|"}[phase%2] }
`,
		"pkg/tui/safe.go": `package tui
import (
	"time"
	tea "charm.land/bubbletea/v2"
	"github.com/docker/docker-agent/pkg/tui/animation"
)
var frames = []string{"-", "|"}
var stateLabels = []string{"idle", "working"}
type copyFlashExpired struct{}
type notificationDismissed struct{}
type tourDelay struct{}
type autoScroll struct{ direction int }
type tmuxVisibility struct{}
type statusProbe struct{}
type model struct{ frame, scrollOffset, state, pollCount int }
func (m *model) View() string { return frames[m.frame%len(frames)] + stateLabels[m.state] }
func (m *model) Update(msg tea.Msg) {
	switch tick := msg.(type) {
	case animation.TickMsg:
		before, elapsed := tick.ElapsedBounds()
		m.frame = int(elapsed / time.Second)
		m.frame = animation.TimedStep(elapsed, time.Second)
		_ = animation.FrameAt(elapsed)
		_ = animation.ProgressAt(elapsed)
		if elapsed != before { tick.MarkDirty() }
	case copyFlashExpired:
		m.state = 0
	case notificationDismissed:
		m.state = 0
	case tourDelay:
		m.state = 1
	case autoScroll:
		m.scrollOffset++
	case tmuxVisibility:
		m.pollCount++
	case statusProbe:
		m.pollCount++
	case tea.TickMsg:
		m.frame++ // Dependency-owned ticking is outside this cop.
		_ = tick.Frame
	}
}
func (m *model) wake() {
	start := time.Now()
	for n := range time.NewTicker(time.Second).C {
		m.frame = int(n.Sub(start) / time.Second)
		_ = m.View()
	}
}
func schedule() []tea.Cmd {
	return []tea.Cmd{
		tea.Tick(time.Second, func(time.Time) tea.Msg { return copyFlashExpired{} }),
		tea.Tick(time.Second, func(time.Time) tea.Msg { return notificationDismissed{} }),
		tea.Tick(time.Second, func(time.Time) tea.Msg { return tourDelay{} }),
		tea.Tick(time.Second, func(time.Time) tea.Msg { return autoScroll{1} }),
		tea.Every(time.Second, func(time.Time) tea.Msg { return tmuxVisibility{} }),
		tea.Tick(time.Second, func(time.Time) tea.Msg { return statusProbe{} }),
	}
}
func enumState(m *model, tick animation.TickMsg) string {
	m.state = 1
	return stateLabels[m.state]
}
type unrelated struct { Frame int }
func fake(tick unrelated) string { return frames[tick.Frame%len(frames)] }
`,
		"pkg/evaluation/progress.go": `package evaluation
import "github.com/docker/docker-agent/pkg/tui/animation"
func outOfScope(t animation.TickMsg) int { return t.Frame }
`,
	})

	var got []string
	for _, offense := range offenses {
		assert.Equal(t, "Lint/TUIAnimationDuration", offense.CopName)
		got = append(got, filepath.Base(offense.Pos.Filename)+": "+offense.Message)
	}
	const advance = " advances by a fixed amount per tick; derive its phase or progress from elapsed animation time"
	assert.ElementsMatch(t, []string{
		"effects.go: animation.TickMsg.Frame counts ticks; use ElapsedBounds and elapsed-time animation helpers",
		"effects.go: visual counter counter" + advance,
		"effects.go: visual counter railFrame" + advance,
		"effects.go: visual counter bottomSlack" + advance,
		"effects.go: visual counter bottomSlack" + advance,
		"effects.go: visual counter voicePhase" + advance,
		"effects.go: visual counter position" + advance,
		"update.go: visual counter frame" + advance,
		"lean.go: visual counter frame" + advance,
	}, got)
}
