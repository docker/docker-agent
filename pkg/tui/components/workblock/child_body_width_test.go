package workblock

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
)

// widthProbe renders one column per cell so any overflow past the body is measurable.
type widthProbe struct {
	width int
	views int
}

func (p *widthProbe) Init() tea.Cmd                          { return nil }
func (p *widthProbe) Update(tea.Msg) (layout.Model, tea.Cmd) { return p, nil }
func (p *widthProbe) View() string                           { p.views++; return strings.Repeat("#", p.width) }
func (p *widthProbe) SetSize(width, _ int) tea.Cmd           { p.width = width; return nil }

func TestWorkChildReRenderKeepsBodyInsetAfterExternalFullWidthReset(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	child := &widthProbe{}
	model := New(ar)
	model.SetSize(40, 20)
	model.SetItems([]Item{{SourceIndex: 1, View: child}})
	model.SetExpandedImmediately(true)
	t.Cleanup(model.StopAnimation)
	require.Equal(t, SourceContentWidth(40), child.width)

	// A transcript-wide resize pass sizes every view, this child included, to full width.
	child.SetSize(40, 0)
	model.InvalidateSource(1)

	rows := 0
	for line := range strings.SplitSeq(ansi.Strip(model.View()), "\n") {
		if !strings.Contains(line, "#") {
			continue
		}
		rows++
		require.Equal(t, 2, len(line)-len(strings.TrimLeft(line, " ")), "child keeps the Work inset: %q", line)
		require.LessOrEqual(t, ansi.StringWidth(strings.TrimRight(line, " ")), 38, "child stays inside the Work body: %q", line)
	}
	require.Positive(t, rows, "the probe child is visible inside the expanded Work body")
}

// Rebuilding the body at an unchanged width must reuse the child's output.
func TestWorkChildSameWidthRebuildDoesNotRerenderChild(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	child := &widthProbe{}
	model := New(ar)
	model.SetSize(40, 20)
	items := []Item{{SourceIndex: 1, View: child}}
	model.SetItems(items)
	model.SetExpandedImmediately(true)
	t.Cleanup(model.StopAnimation)
	_ = model.View()
	rendered := child.views

	model.SetSize(40, 20)
	model.SetItems(items)
	_ = model.View()
	require.Equal(t, rendered, child.views, "same-width body rebuild reuses the child render")
	require.Equal(t, SourceContentWidth(40), child.width)
}
