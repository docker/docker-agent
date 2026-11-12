package common

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog/internal/testutil"
)

func TestMultiChoiceResultSequence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		inputs []tea.Msg
		result MultiChoiceResult
	}{
		{"skip", []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "skip", IsSkipped: true}},
		{"option", []tea.Msg{tea.KeyPressMsg{Code: '1', Text: "1"}, tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "one", Value: "first"}},
		{"paste", []tea.Msg{tea.PasteMsg{Content: "  custom  "}, tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "custom", Value: "custom", IsCustom: true}},
		{"cancel", []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEscape}}, MultiChoiceResult{IsCancelled: true}},
		{"cancel custom", []tea.Msg{tea.PasteMsg{Content: "discard"}, tea.KeyPressMsg{Code: tea.KeyEscape}}, MultiChoiceResult{IsCancelled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewMultiChoiceDialog(MultiChoiceConfig{
				DialogID: "sequence", Title: "Choose",
				Options:     []MultiChoiceOption{{ID: "one", Label: "One", Value: "first"}},
				AllowCustom: true, AllowSecondary: true,
			})
			var cmd tea.Cmd
			for _, input := range tc.inputs {
				_, cmd = d.Update(input)
			}
			steps := testutil.SequenceCommands(t, cmd)
			require.Len(t, steps, 2)
			assert.Equal(t, CloseDialogMsg{}, steps[0]())
			assert.Equal(t, MultiChoiceResultMsg{DialogID: "sequence", Result: tc.result}, steps[1]())
		})
	}
}

func TestMultiChoiceQuitDoesNotSendResult(t *testing.T) {
	t.Parallel()

	d := NewMultiChoiceDialog(MultiChoiceConfig{AllowCustom: true})
	_, _ = d.Update(tea.PasteMsg{Content: "unfinished"})
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	assert.Equal(t, tea.QuitMsg{}, cmd())
}
