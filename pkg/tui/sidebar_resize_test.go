package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSidebarResizeGuideStaysBelowForeground(t *testing.T) {
	for _, tc := range []struct {
		name string
		dock messages.SidebarPosition
		open func(*appModel) *lipgloss.Layer
	}{
		{"completion", messages.SidebarLeft, func(m *appModel) *lipgloss.Layer {
			m.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: strings.Repeat("popup ", 11), Value: "test"}}})
			return m.completions.GetLayers()[0]
		}},
		{"tour", messages.SidebarRight, func(m *appModel) *lipgloss.Layer {
			m.tour.Start()
			return m.tour.Layer()
		}},
		{"notification", messages.SidebarRight, func(m *appModel) *lipgloss.Layer {
			m.Update(notification.ShowMsg{Text: strings.Repeat("Notice text ", 18), Type: notification.TypeInfo})
			return m.notification.GetLayer()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, x, clock := sidebarResizeTestRoot(t, tc.dock)
			layer := tc.open(m)
			require.NotNil(t, layer)
			m.viewCacheValid = false
			_ = m.View()

			insideX := max(x+4, layer.GetX()+1)
			insideX = min(insideX, layer.GetX()+layer.Width()-2)
			insideY := layer.GetY()
			require.Positive(t, insideY, "the guide must also have uncovered rows")
			require.Less(t, insideY, m.contentHeight, "the foreground must intersect the guide's vertical range")
			_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			_ = m.View() // the press changes the grip and neighboring scrollbar paint
			move := func(target int) {
				t.Helper()
				m.Update(tea.MouseMotionMsg{X: target, Y: 1, Button: tea.MouseLeft})
				require.True(t, m.viewCacheValid, "moving the guide does not render components again")
				require.Equal(t, target, requireSidebarGuide(t, m).X)
			}
			move(insideX)
			cmd = sidebarGuideTestTick(t, m, cmd)
			require.Nil(t, cmd)
			require.True(t, m.viewCacheValid)
			base := m.viewCache.Content

			cell := func(frame string, x, y int) *uv.Cell {
				t.Helper()
				buffer := uv.NewScreenBuffer(lipgloss.Width(frame), lipgloss.Height(frame))
				uv.NewStyledString(frame).Draw(&buffer, buffer.Bounds())
				c := buffer.CellAt(x, y)
				require.NotNil(t, c)
				return c
			}
			assertCovered := func(frame string, x int) {
				t.Helper()
				for y := insideY; y < min(insideY+layer.Height(), m.contentHeight); y++ {
					assert.Equal(t, cell(base, x, y), cell(frame, x, y), "foreground glyph, style and link at row %d", y)
				}
			}
			full := m.View().Content
			assertCovered(full, insideX)
			assert.Equal(t, "│", cell(full, insideX, 0).String(), "guide remains visible above the foreground")
			assert.Zero(t, testing.AllocsPerRun(20, func() { _ = m.View() }), "steady overlay reuses the exact composed frame")

			move(insideX + 1)
			moved := m.View().Content
			assertCovered(moved, insideX+1)
			assert.Equal(t, cell(base, insideX, 0), cell(moved, insideX, 0), "moving the guide restores the old column")
			assert.Equal(t, "│", cell(moved, insideX+1, 0).String())

			_, cmd = m.Update(tea.MouseReleaseMsg{X: insideX + 1, Y: -1, Button: tea.MouseLeft})
			_ = m.View()
			base = m.viewCache.Content
			clock.step = animation.ShortDuration / 100
			cmd = sidebarGuideTestTick(t, m, cmd)
			guide := requireSidebarGuide(t, m)
			require.LessOrEqual(t, guide.Start, insideY)
			require.Greater(t, guide.End, insideY, "retraction still crosses the foreground")
			require.True(t, m.viewCacheValid, "settling also reuses the base frame")
			require.GreaterOrEqual(t, guide.X, layer.GetX())
			require.Less(t, guide.X, layer.GetX()+layer.Width())
			assert.Equal(t, cell(base, guide.X, insideY), cell(m.View().Content, guide.X, insideY))
			clock.step = animation.ShortDuration
			require.Nil(t, sidebarGuideTestTick(t, m, cmd))
			assert.Equal(t, base, m.View().Content, "completion of retraction restores the full cached frame")
		})
	}
}
