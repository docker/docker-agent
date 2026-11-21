package tui

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
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
			m.Update(notification.ShowMsg{Text: strings.Repeat("Notice text ", 30), Type: notification.TypeInfo})
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
			_ = m.View() // the press changes the grip
			m.Update(tea.MouseMotionMsg{X: insideX, Y: 1, Button: tea.MouseLeft})
			require.Equal(t, insideX, requireSidebarGuide(t, m).X)
			require.Nil(t, sidebarGuideTestTick(t, m, cmd))
			require.True(t, m.viewCacheValid)
			screen := func(frame string) uv.ScreenBuffer {
				buf := uv.NewScreenBuffer(lipgloss.Width(frame), lipgloss.Height(frame))
				uv.NewStyledString(frame).Draw(&buf, buf.Bounds())
				return buf
			}
			base, full := screen(m.viewCache.Content), screen(m.View().Content)
			for y := insideY; y < min(insideY+layer.Height(), m.contentHeight); y++ {
				assert.Equal(t, base.CellAt(insideX, y), full.CellAt(insideX, y), "foreground cell at row %d", y)
			}
			assert.Equal(t, "│", full.CellAt(insideX, 0).String(), "guide still paints uncovered rows")

			m.Update(tea.MouseMotionMsg{X: insideX + 1, Y: 1, Button: tea.MouseLeft})
			moved := screen(m.View().Content)
			assert.Equal(t, base.CellAt(insideX, 0), moved.CellAt(insideX, 0), "moving restores the old column")
			assert.Equal(t, "│", moved.CellAt(insideX+1, 0).String())

			_, cmd = m.Update(tea.MouseReleaseMsg{X: insideX + 1, Y: -1, Button: tea.MouseLeft})
			_ = m.View()
			clock.step = animation.ShortDuration / 100
			sidebarGuideTestTick(t, m, cmd)
			guide := requireSidebarGuide(t, m)
			require.LessOrEqual(t, guide.Start, insideY)
			require.Greater(t, guide.End, insideY)
			require.GreaterOrEqual(t, guide.X, layer.GetX())
			require.Less(t, guide.X, layer.GetX()+layer.Width())
			base, full = screen(m.viewCache.Content), screen(m.View().Content)
			assert.Equal(t, base.CellAt(guide.X, insideY), full.CellAt(guide.X, insideY), "retraction also stays under foreground")
		})
	}
}

func TestSidebarResizeGuideClipsKittyPlacements(t *testing.T) {
	m, x, clock := sidebarResizeTestRoot(t, messages.SidebarLeft)
	var pngData, out bytes.Buffer
	require.NoError(t, png.Encode(&pngData, image.NewGray(image.Rect(0, 0, 120, 32))))
	m.imageWriter = tuiimage.NewWriter(&out)
	tool := tools.Tool{Name: "image"}
	call := tools.ToolCall{ID: "sidebar-image", Function: tools.FunctionCall{Name: tool.Name}}
	result := &tools.ToolCallResult{Images: []tools.MediaContent{{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(pngData.Bytes())}}}
	m.Update(agentruntime.ToolCall(call, tool, "root"))
	m.Update(agentruntime.ToolCallResponse(call.ID, tool, result, "", "root"))
	m.Update(agentruntime.StreamStopped("profile", "root", "normal"))

	writeFrame := func() string {
		out.Reset()
		_, err := m.imageWriter.Write([]byte(m.View().Content))
		require.NoError(t, err)
		_, commands, _ := strings.Cut(out.String(), "\x1b7")
		return commands
	}
	checkPlacements := func(commands string, crops ...string) {
		require.Equal(t, len(crops), strings.Count(commands, "\x1b_Ga=p,"))
		for _, crop := range crops {
			assert.Contains(t, commands, ","+crop+"\x1b\\")
		}
	}
	checkPlacements(writeFrame(), "c=60,r=8")

	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x + 7, Y: 1, Button: tea.MouseLeft})
	require.Nil(t, sidebarGuideTestTick(t, m, cmd))
	commands := writeFrame()
	assert.Contains(t, commands, "\x1b_Ga=d,d=a,q=2", "remove the old uncropped placement")
	assert.NotContains(t, commands, "\x1b_Ga=t,", "cropping must reuse the existing image upload")
	checkPlacements(commands, "c=4,r=8,x=0,y=0,w=8,h=32", "c=55,r=8,x=10,y=0,w=110,h=32")
	assert.Contains(t, commands, "\x1b[3;54H\x1b_Ga=p,", "right-hand placement starts after the guide")

	m.Update(tea.MouseMotionMsg{X: x + 9, Y: 1, Button: tea.MouseLeft})
	checkPlacements(writeFrame(), "c=6,r=8,x=0,y=0,w=12,h=32", "c=53,r=8,x=14,y=0,w=106,h=32")

	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	clock.step = animation.ShortDuration / 10
	cmd = sidebarGuideTestTick(t, m, cmd)
	checkPlacements(writeFrame(), "c=60,r=2,x=0,y=0,w=120,h=8", "c=4,r=6,x=0,y=8,w=8,h=24", "c=55,r=6,x=10,y=8,w=110,h=24")
	clock.step = animation.ShortDuration
	require.Nil(t, sidebarGuideTestTick(t, m, cmd))
	commands = writeFrame()
	assert.Contains(t, commands, "\x1b_Ga=d,d=a,q=2", "remove temporary crops when restoring the full placement")
	checkPlacements(commands, "c=60,r=8")

	// The tour card covers the right side of the image, leaving the rest visible.
	m.tour.SetSize(120, m.height, 10)
	m.tour.Start()
	m.viewCacheValid = false
	checkPlacements(writeFrame(), "c=25,r=8,x=0,y=0,w=50,h=32")
}
