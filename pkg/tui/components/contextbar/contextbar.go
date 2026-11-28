// Package contextbar provides a thin progress indicator showing how full the
// LLM context window is as a single row of half-block characters (▄).
package contextbar

import (
	"fmt"
	"image/color"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	Height            = 1
	blockFill         = "▄"
	warningThreshold  = 0.45
	criticalThreshold = 0.65
	animDuration      = 670 * time.Millisecond
)

// Model holds the state for the context-usage bar.
type Model struct {
	width          int
	contextPercent float64
	hasReading     bool

	anim     animation.Transition
	animFrom float64
	animTo   float64
}

// New creates a context bar owned by runtime.
func New(runtime *animation.Runtime) *Model {
	if runtime == nil {
		panic("contextbar: nil animation runtime")
	}
	return &Model{anim: animation.NewTransition(runtime)}
}

// Cancel stops any in-flight animation.
func (m *Model) Cancel() { m.anim.Cancel() }

// SetWidth sets the available rendering width.
func (m *Model) SetWidth(width int) { m.width = width }

func usagePercent(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return max(0, min(1, float64(used)/float64(limit)))
}

// SetContextUsage updates context-window utilisation with an animated transition.
// A zero limit has no reading and eases the fill to an empty track.
func (m *Model) SetContextUsage(used, limit int64) tea.Cmd {
	if limit <= 0 {
		used, limit = 0, 0
	}
	m.hasReading = limit > 0
	newPercent := usagePercent(used, limit)
	if m.anim.Running() && newPercent == m.animTo {
		return nil
	}
	if newPercent == m.contextPercent {
		m.Cancel()
		return nil
	}
	m.animFrom = m.contextPercent
	m.animTo = newPercent
	return m.anim.Start(animDuration, animation.EaseOutCubic)
}

// Hold marks usage as pending while keeping the displayed fill as the next origin.
func (m *Model) Hold() {
	m.Cancel()
	m.hasReading = false
}

// Label is the currently displayed usage reading, tracking the animated fill.
func (m *Model) Label() string {
	if !m.hasReading {
		return ""
	}
	return fmt.Sprintf("%.0f%%", m.contextPercent*100)
}

// Filled reports whether the bar currently renders any fill.
func (m *Model) Filled() bool { return m.contextPercent > 0 }

// SetContextUsageDirect initializes context-window utilisation without animation.
func (m *Model) SetContextUsageDirect(used, limit int64) {
	m.hasReading = limit > 0
	newPercent := usagePercent(used, limit)
	if limit > 0 && m.anim.Running() && newPercent == m.animTo {
		return
	}
	m.Cancel()
	m.contextPercent = newPercent
	m.animFrom = newPercent
	m.animTo = newPercent
}

// Update handles animation tick messages.
func (m *Model) Update(msg tea.Msg) {
	if tick, ok := msg.(animation.TickMsg); ok && m.anim.Running() {
		before := m.View()
		m.anim.Tick()
		m.contextPercent = m.animFrom + (m.animTo-m.animFrom)*m.anim.Value()
		if !m.anim.Running() {
			m.contextPercent = m.animTo
		}
		if before != m.View() {
			tick.MarkDirty()
		}
	}
}

// View renders the bar as a single line of [blockFill] characters followed
// by a "Context xx%" label.
func (m *Model) View() string {
	if m.width <= 0 {
		return ""
	}

	label := " Context —"
	if reading := m.Label(); reading != "" {
		label = " Context " + reading
	}
	labelWidth := lipgloss.Width(label)
	if labelWidth > m.width {
		label, labelWidth = "", 0
	}
	barWidth := m.width - labelWidth
	const widest = len(" Context 100%")
	scale := barWidth
	if label != "" && m.width > widest {
		scale = min(barWidth, m.width-widest)
	}

	fillWidth := 0
	if m.contextPercent > 0 && barWidth > 0 {
		fillWidth = min(max(1, int(math.Round(float64(scale)*m.contextPercent))), barWidth)
	}

	const gradientLen = 5
	gradWidth := min(gradientLen, fillWidth)
	solidWidth := fillWidth - gradWidth
	trackWidth := barWidth - fillWidth

	fillFg := fillColor(m.contextPercent)
	trackFg := styles.Background
	baseFg := styles.BlendColor(trackFg, fillFg, 1.0/float64(gradientLen+1))
	bg := lipgloss.NewStyle().Background(styles.EditorBg)

	var sb strings.Builder
	if solidWidth > 0 {
		sb.WriteString(bg.Foreground(baseFg).Render(strings.Repeat(blockFill, solidWidth)))
	}
	for i := range gradWidth {
		t := float64(i+1) / float64(gradWidth+1)
		sb.WriteString(bg.Foreground(styles.BlendColor(trackFg, fillFg, t)).Render(blockFill))
	}
	if trackWidth > 0 {
		sb.WriteString(bg.Foreground(trackFg).Render(strings.Repeat(blockFill, trackWidth)))
	}
	if label != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(styles.TextMuted).Render(label))
	}
	return sb.String()
}

func fillColor(pct float64) color.Color {
	switch {
	case pct >= criticalThreshold:
		return styles.BlendColor(styles.Warning, styles.Error, (pct-criticalThreshold)/(1.0-criticalThreshold))
	case pct >= warningThreshold:
		return styles.BlendColor(styles.Info, styles.Warning, (pct-warningThreshold)/(criticalThreshold-warningThreshold))
	default:
		return styles.Info
	}
}
