package toolconfirmation

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog/common"
	"github.com/docker/docker-agent/pkg/tui/dialog/internal/testutil"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type trackedSessionState struct {
	service.EmbeddedSessionState

	changes []bool
}

func (s *trackedSessionState) SetYoloMode(enabled bool) {
	s.changes = append(s.changes, enabled)
	s.EmbeddedSessionState.SetYoloMode(enabled)
}

func TestApprovalSequenceAndSessionTiming(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key     string
		request runtime.ResumeRequest
		changes []bool
	}{
		{"Y", runtime.ResumeApprove(), nil},
		{"T", runtime.ResumeApproveTool("shell:cmd=ls*"), nil},
		{"B", runtime.ResumeApproveBalanced(), []bool{false}},
		{"A", runtime.ResumeApproveAutonomous(), []bool{true}},
	} {
		for _, key := range []string{tc.key, strings.ToLower(tc.key)} {
			for _, initialYolo := range []bool{false, true} {
				t.Run(key+"/yolo="+strconv.FormatBool(initialYolo), func(t *testing.T) {
					t.Parallel()
					state := &trackedSessionState{}
					state.EmbeddedSessionState.SetYoloMode(initialYolo)
					event := newConfirmationEvent(nil)
					event.ToolCall.Function.Arguments = `{"cmd":"ls -la /tmp"}`
					d := NewToolConfirmationDialog(animation.NewRuntime(), event, state)
					require.Nil(t, state.changes)

					model, cmd := d.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
					require.Same(t, d, model)
					require.Equal(t, tc.changes, state.changes, "session changes happen in Update, before commands run")
					wantYolo := initialYolo
					if len(tc.changes) > 0 {
						wantYolo = tc.changes[0]
					}
					require.Equal(t, wantYolo, state.YoloMode())

					steps := testutil.SequenceCommands(t, cmd)
					require.Len(t, steps, 2)
					assert.Equal(t, common.CloseDialogMsg{}, steps[0]())
					assert.Equal(t, RuntimeResumeMsg{Request: tc.request}, steps[1]())
					assert.Equal(t, tc.changes, state.changes, "commands must not repeat session mutations")
				})
			}
		}
	}
}

func TestRejectionResult(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		result common.MultiChoiceResult
		want   *RuntimeResumeMsg
	}{
		{"cancel wins", common.MultiChoiceResult{IsCancelled: true, IsSkipped: true, Value: "ignored"}, nil},
		{"skip drops reason", common.MultiChoiceResult{IsSkipped: true, Value: "ignored"}, &RuntimeResumeMsg{Request: runtime.ResumeReject("")}},
		{"value unchanged", common.MultiChoiceResult{Value: "  unchanged  "}, &RuntimeResumeMsg{Request: runtime.ResumeReject("  unchanged  ")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, HandleToolRejectionResult(tc.result))
		})
	}
}

type recordingScrollView struct {
	messages.Model

	updates []tea.Msg
	next    messages.Model
}

func (s *recordingScrollView) Init() tea.Cmd {
	return core.CmdHandler("scroll init")
}

func (s *recordingScrollView) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	s.updates = append(s.updates, msg)
	return s.next, core.CmdHandler("scroll update")
}

func TestConfirmationScrollForwarding(t *testing.T) {
	t.Parallel()

	for _, msg := range []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyUp},
		tea.KeyPressMsg{Code: tea.KeyDown},
		tea.KeyPressMsg{Code: tea.KeyPgUp},
		tea.KeyPressMsg{Code: tea.KeyPgDown},
		tuimessages.WheelCoalescedMsg{Delta: -3, X: 10, Y: 20},
	} {
		d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.EmbeddedSessionState{}).(*toolConfirmationDialog)
		next := &recordingScrollView{}
		scroll := &recordingScrollView{next: next}
		d.scrollView = scroll
		assert.Equal(t, "scroll init", d.Init()())
		model, cmd := d.Update(msg)
		require.Same(t, d, model)
		require.NotNil(t, cmd)
		assert.Equal(t, "scroll update", cmd())
		assert.Equal(t, []tea.Msg{msg}, scroll.updates)
		assert.Same(t, next, d.scrollView)
	}
}

func TestConfirmationIgnoresNonActionsAndQuits(t *testing.T) {
	t.Parallel()

	state := &trackedSessionState{}
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), state)
	for _, msg := range []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyEscape},
		tea.KeyPressMsg{Code: tea.KeyEnter},
		tea.KeyPressMsg{Code: 'x', Text: "x"},
		tea.MouseClickMsg{Button: tea.MouseRight},
	} {
		_, cmd := d.Update(msg)
		assert.Nil(t, cmd)
	}
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	assert.Equal(t, tea.QuitMsg{}, cmd())
	assert.Nil(t, state.changes)
}
