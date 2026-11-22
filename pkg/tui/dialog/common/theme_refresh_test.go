package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestMultiChoiceCustomInputRefreshesOnThemeChange(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(styles.DefaultTheme())
	before := styles.DialogInputStyle

	d := NewMultiChoiceDialog(MultiChoiceConfig{
		Title:       "Why?",
		Options:     []MultiChoiceOption{{ID: "a", Label: "A"}},
		AllowCustom: true,
	}).(*multiChoiceDialog)
	require.Equal(t, before, d.customInput.Styles())

	theme := styles.DefaultTheme()
	theme.Colors.TextPrimary = "#123456"
	theme.Colors.Accent = "#316497"
	styles.ApplyTheme(theme)
	require.NotEqual(t, before, styles.DialogInputStyle)

	updated, _ := d.Update(messages.ThemeChangedMsg{})
	in := updated.(*multiChoiceDialog).customInput
	assert.Equal(t, styles.DialogInputStyle, in.Styles())
}
