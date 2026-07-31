package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// These tests pin how the real elicitation dialog interprets the runtime's
// workspace-escape confirmation schema (runtime.MediaEscapeDecisionSchema):
// a bare Enter must submit the safe decline choice, and the affirmative
// choice must require an explicit selection. Together with the runtime-side
// content check in confirmMediaEscape they guarantee no external write
// happens without an explicit affirmative answer.

func newMediaEscapeDialog(t *testing.T) *ElicitationDialog {
	t.Helper()
	d, ok := NewElicitationDialog("Save cat.png outside the workspace?", runtime.MediaEscapeDecisionSchema(), nil, "elic-1").(*ElicitationDialog)
	require.True(t, ok)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	return d
}

func elicitationResponse(t *testing.T, cmd tea.Cmd) messages.ElicitationResponseMsg {
	t.Helper()
	resp, ok := firstMsgOfType[messages.ElicitationResponseMsg](collectMsgs(cmd))
	require.True(t, ok, "submitting must produce an elicitation response")
	return resp
}

func TestElicitationDialog_MediaEscapeSchema_BareEnterIsSafeChoice(t *testing.T) {
	t.Parallel()
	d := newMediaEscapeDialog(t)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	resp := elicitationResponse(t, cmd)
	assert.Equal(t, tools.ElicitationActionAccept, resp.Action)
	assert.Equal(t, runtime.MediaEscapeDeclineChoice, resp.Content[runtime.MediaEscapeDecisionField],
		"a bare Enter must submit the safe choice, never the external-write one")
}

func TestElicitationDialog_MediaEscapeSchema_ExplicitSelectionAccepts(t *testing.T) {
	t.Parallel()
	d := newMediaEscapeDialog(t)

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	resp := elicitationResponse(t, cmd)
	assert.Equal(t, tools.ElicitationActionAccept, resp.Action)
	assert.Equal(t, runtime.MediaEscapeAcceptChoice, resp.Content[runtime.MediaEscapeDecisionField],
		"the affirmative choice must be reachable by explicit selection")
}

func TestElicitationDialog_MediaEscapeSchema_EscapeCancels(t *testing.T) {
	t.Parallel()
	d := newMediaEscapeDialog(t)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	resp := elicitationResponse(t, cmd)
	assert.Equal(t, tools.ElicitationActionCancel, resp.Action)
	assert.Empty(t, resp.Content)
}
