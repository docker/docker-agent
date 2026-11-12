package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/dialog/common"
	"github.com/docker/docker-agent/pkg/tui/dialog/internal/testutil"
	"github.com/docker/docker-agent/pkg/tui/dialog/toolconfirmation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSharedConfirmationRejectionTransitions(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name   string
		inputs []tea.Msg
		result MultiChoiceResult
	}
	cases := []testCase{
		{"cancel", []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEscape}}, MultiChoiceResult{IsCancelled: true}},
		{"skip", []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "skip", IsSkipped: true}},
		{"custom", []tea.Msg{tea.PasteMsg{Content: "  use another tool  "}, tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "custom", Value: "use another tool", IsCustom: true}},
	}
	for i, reason := range toolconfirm.RejectionReasons() {
		cases = append(cases, testCase{
			name: reason.ID,
			inputs: []tea.Msg{
				tea.KeyPressMsg{Code: '1' + rune(i), Text: string('1' + rune(i))},
				tea.KeyPressMsg{Code: tea.KeyEnter},
			},
			result: MultiChoiceResult{OptionID: reason.ID, Value: reason.Value},
		})
	}

	for _, tc := range cases {
		for _, rejectKey := range []string{"n", "N"} {
			t.Run(tc.name+"/"+rejectKey, func(t *testing.T) {
				t.Parallel()
				state := &service.EmbeddedSessionState{}
				state.SetYoloMode(true)
				event := &runtime.ToolCallConfirmationEvent{
					ToolCall:       tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}},
					ToolDefinition: tools.Tool{Name: "shell"},
				}
				confirmation := toolconfirmation.NewToolConfirmationDialog(animation.NewRuntime(), event, state)
				mgr := New()
				mgr.SetSize(100, 40)
				mgr.Update(common.OpenDialogMsg{Model: confirmation, OriginatingEvent: event})
				require.Same(t, event, mgr.TopBackgroundEvent())

				_, cmd := mgr.Update(tea.KeyPressMsg{Code: rune(rejectKey[0]), Text: rejectKey})
				require.NotNil(t, cmd)
				require.Same(t, confirmation, mgr.TopDialog(), "reject only schedules opening the reason dialog")
				open, ok := cmd().(OpenDialogMsg)
				require.True(t, ok, "shared open messages retain application type identity")
				mgr.Update(open)
				require.Same(t, open.Model, mgr.TopDialog())
				assert.False(t, mgr.TopIsBackground())

				for _, input := range tc.inputs {
					_, cmd = mgr.Update(input)
				}
				steps := testutil.SequenceCommands(t, cmd)
				require.Len(t, steps, 2)
				closeMsg, ok := steps[0]().(CloseDialogMsg)
				require.True(t, ok)
				mgr.Update(closeMsg)
				require.Same(t, confirmation, mgr.TopDialog(), "reason closes before its result is handled")
				assert.Same(t, event, mgr.TopBackgroundEvent())
				result, ok := steps[1]().(MultiChoiceResultMsg)
				require.True(t, ok)
				assert.Equal(t, ToolRejectionDialogID, result.DialogID)
				assert.Equal(t, tc.result, result.Result)

				resume := HandleToolRejectionResult(result.Result)
				if tc.result.IsCancelled {
					assert.Nil(t, resume)
					assert.Same(t, confirmation, mgr.TopDialog())
				} else {
					require.NotNil(t, resume)
					assert.Equal(t, runtime.ResumeReject(tc.result.Value), resume.Request)
					assert.IsType(t, &toolconfirmation.RuntimeResumeMsg{}, resume)
					mgr.Update(common.CloseDialogMsg{})
					assert.False(t, mgr.Open())
				}
				assert.True(t, state.YoloMode(), "rejection does not alter approval mode")
			})
		}
	}
}

func TestCompatibilityConstructors(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{DialogID: "compat", Title: "Choose", AllowSecondary: true}
	assert.IsType(t, common.NewMultiChoiceDialog(config), NewMultiChoiceDialog(config))
	assert.IsType(t, toolconfirmation.NewToolRejectionReasonDialog(), NewToolRejectionReasonDialog())
	event := &runtime.ToolCallConfirmationEvent{ToolCall: tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}}
	assert.IsType(t,
		toolconfirmation.NewToolConfirmationDialog(animation.NewRuntime(), event, &service.EmbeddedSessionState{}),
		NewToolConfirmationDialog(animation.NewRuntime(), event, &service.EmbeddedSessionState{}))

	mgr := New()
	mgr.Update(common.OpenDialogMsg{Model: NewExitConfirmationDialog()})
	assert.True(t, mgr.TopIsExitConfirmation())
	mgr.Update(common.CloseAllDialogsMsg{})
	assert.False(t, mgr.Open())
}
