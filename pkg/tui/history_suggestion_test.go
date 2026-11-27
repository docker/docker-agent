package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestHistorySuggestionKeepsStatusBarVisible(t *testing.T) {
	const width, height = 48, 24
	rows := []string{"preview-one", "preview-two", "preview-three", "preview-four", "preview-five"}
	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{name: "multiline", prompt: "A " + strings.Join(rows, "\n")},
		{name: "soft wrapped", prompt: "A " + strings.Join([]string{
			"preview-one shows the first line",
			"preview-two shows the second line",
			"preview-three shows the third line",
			"preview-four shows the fourth line",
			"preview-five shows the last line",
		}, " ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, width, height)
			require.Equal(t, composerMinRows, root.composer.rows, "an empty draft starts at the composer floor")
			require.NoError(t, root.history.Add(tc.prompt))
			baseline := strings.Split(ansi.Strip(root.View().Content), "\n")
			require.Len(t, baseline, height)
			status := strings.TrimSpace(ansi.Strip(root.statusBar.View()))
			require.NotEmpty(t, status)
			require.Equal(t, status, strings.TrimSpace(baseline[height-1]))

			_, _ = root.Update(tea.KeyPressMsg{Code: 'A', Text: "A"})
			assert.Equal(t, "A", root.activeTab.editor.Value(), "a preview must not change the draft")
			frame := ansi.Strip(root.View().Content)
			lines := strings.Split(frame, "\n")
			assert.Len(t, lines, height)
			require.GreaterOrEqual(t, len(lines), height)
			statusRow := -1
			for i, line := range lines {
				if strings.TrimSpace(line) == status {
					statusRow = i + 1
					break
				}
			}
			t.Logf("preview renders %d rows; status bar is on row %d", len(lines), statusRow)
			assert.Equal(t, height, statusRow, "status bar must stay on the last terminal row")
			firstRow := root.composerDock().editorTop + styles.EditorStyle.GetPaddingTop()
			for i, want := range rows[:composerMinRows] {
				assert.Contains(t, lines[firstRow+i], want)
			}
			for _, hidden := range rows[composerMinRows:] {
				assert.NotContains(t, frame, hidden)
			}

			_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			assert.Equal(t, tc.prompt, root.activeTab.editor.Value(), "Tab accepts the full history entry")
			assert.Equal(t, PanelEditor, root.focusedPanel)
			assert.Len(t, strings.Split(root.View().Content, "\n"), height)
		})
	}
}
