package leantui

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

func TestResize(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		width, height int
	}{
		{name: "width only", width: 40, height: 24},
		{name: "height only", width: 80, height: 8},
		{name: "grow", width: 120, height: 40},
		{name: "shrink", width: 30, height: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := bareModel(24)
			var output bytes.Buffer
			m.r = ui.NewRenderer(bufio.NewWriter(&output), m.width, m.height)
			text := strings.Repeat("resize this paragraph without losing content ", 12)
			m.screen.Transcript.AppendAssistant(text)
			m.screen.Editor.SetText("keep this draft")
			m.render()
			output.Reset()

			m.resize(m.width, m.height)
			assert.Empty(t, output.String(), "unchanged polling must not render")
			m.render()
			assert.NotContains(t, output.String(), "\x1b[2J", "unchanged size must not invalidate the next frame")
			output.Reset()

			m.resize(tc.width, tc.height)
			assert.Equal(t, tc.width, m.width)
			assert.Equal(t, tc.height, m.height)
			assert.Contains(t, output.String(), "\x1b[2J", "changed dimensions must repaint")
			lines, _, _ := m.buildLines()
			assert.Equal(t, max(0, len(lines)-tc.height), m.r.ViewportTop())
			require.NotEmpty(t, lines)
			for _, line := range lines {
				assert.LessOrEqual(t, ui.DisplayWidth(line), tc.width)
			}

			m.resize(80, 24)
			assert.Equal(t, "keep this draft", m.screen.Editor.Text())
			assert.Contains(t, strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n"), "resize this paragraph")
			output.Reset()
			m.resize(80, 24)
			assert.Empty(t, output.String(), "repeated size must not render")
		})
	}
}
