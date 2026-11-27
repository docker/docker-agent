package dialog

import (
	"testing"

	"charm.land/bubbles/v2/textinput"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func useDefaultTheme(t *testing.T) {
	t.Helper()
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(styles.DefaultTheme())
}

func switchToContrastingTheme(t *testing.T) textinput.Styles {
	t.Helper()
	before := styles.DialogInputStyle
	theme := styles.DefaultTheme()
	theme.Colors.TextPrimary = "#123456"
	theme.Colors.Accent = "#316497"
	styles.ApplyTheme(theme)
	require.NotEqual(t, before, styles.DialogInputStyle, "test theme must change dialog input styles")
	return before
}

func TestDialogTextInputsRefreshOnThemeChange(t *testing.T) {
	cases := map[string]struct {
		dialog func() Dialog
		input  func(Dialog) *input.Text
	}{
		"model picker": {
			dialog: func() Dialog { return NewModelPickerDialog(nil) },
			input:  func(d Dialog) *input.Text { return &d.(*modelPickerDialog).textInput },
		},
		"command palette": {
			dialog: func() Dialog { return NewCommandPaletteDialog(nil) },
			input:  func(d Dialog) *input.Text { return &d.(*commandPaletteDialog).textInput },
		},
		"help search": {
			dialog: func() Dialog { return NewHelpDialog(nil) },
			input:  func(d Dialog) *input.Text { return &d.(*helpDialog).input },
		},
		"file picker": {
			dialog: func() Dialog { return NewFilePickerDialog(t.TempDir()) },
			input:  func(d Dialog) *input.Text { return &d.(*filePickerDialog).textInput },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			useDefaultTheme(t)
			d := tc.dialog()
			before := switchToContrastingTheme(t)
			in := tc.input(d)
			require.Equal(t, before, in.Styles(), "input should start with the old theme")

			updated, _ := d.Update(messages.ThemeChangedMsg{})
			assert.Equal(t, styles.DialogInputStyle, tc.input(updated.(Dialog)).Styles())
		})
	}
}

func TestElicitationInputsRefreshOnThemeChange(t *testing.T) {
	useDefaultTheme(t)
	d := NewElicitationDialog("Enter a value", nil, nil)
	el := d.(*ElicitationDialog)
	before := switchToContrastingTheme(t)
	require.Equal(t, before, el.responseInput.Styles())

	updated, _ := d.Update(messages.ThemeChangedMsg{})
	assert.Equal(t, styles.DialogInputStyle, updated.(*ElicitationDialog).responseInput.Styles())
}

func TestMCPPromptInputsRefreshOnThemeChange(t *testing.T) {
	useDefaultTheme(t)
	d := NewMCPPromptInputDialog("greet", tools.PromptInfo{
		Name:      "greet",
		Arguments: []tools.PromptArgument{{Name: "who", Required: true}, {Name: "tone"}},
	}).(*MCPPromptInputDialog)
	before := switchToContrastingTheme(t)
	require.Len(t, d.inputs, 2)
	require.Equal(t, before, d.inputs[0].Styles())

	updated, _ := d.Update(messages.ThemeChangedMsg{})
	for i, in := range updated.(*MCPPromptInputDialog).inputs {
		assert.Equal(t, styles.DialogInputStyle, in.Styles(), "input %d", i)
	}
}

func TestThemeChangeReachesStackedDialogs(t *testing.T) {
	useDefaultTheme(t)
	mgr := New().(*manager)
	mgr.handleOpen(OpenDialogMsg{Model: NewCommandPaletteDialog(nil)})
	mgr.handleOpen(OpenDialogMsg{Model: NewElicitationDialog("Enter a value", nil, nil)})
	before := switchToContrastingTheme(t)
	require.Len(t, mgr.stack, 2)
	require.Equal(t, before, mgr.stack[0].dialog.(*commandPaletteDialog).textInput.Styles())

	_, _ = mgr.Update(messages.ThemeChangedMsg{})
	assert.Equal(t, styles.DialogInputStyle, mgr.stack[0].dialog.(*commandPaletteDialog).textInput.Styles(), "bottom dialog")
	assert.Equal(t, styles.DialogInputStyle, mgr.stack[1].dialog.(*ElicitationDialog).responseInput.Styles(), "top dialog")
}
