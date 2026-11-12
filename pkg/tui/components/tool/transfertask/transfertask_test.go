package transfertask

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	transfertasktool "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// spinnerGlyph is the first frame of the shared braille spinner. An in-flight
// delegation must show it instead of a success/error glyph.
const spinnerGlyph = "⠋"

func transferMessage(status types.ToolStatus) *types.Message {
	return &types.Message{
		Type:       types.MessageTypeToolCall,
		Sender:     "root",
		ToolStatus: status,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Arguments: `{"agent":"researcher","task":"Investigate the flaky test"}`,
			},
		},
	}
}

func renderCard(status types.ToolStatus) string {
	view := New(animation.NewRuntime(), transferMessage(status), nil)
	view.SetSize(80, 1)
	return stripANSI(view.View())
}

// TestTransferTaskCard_StatusIcon locks in the status-aware icon: a running or
// pending delegation animates the spinner (never showing a premature ✓), while
// completed/error terminal states show ✓/✗. The parent→child header and the
// task text must render in every state.
func TestTransferTaskCard_StatusIcon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  types.ToolStatus
		want    string   // glyph that must be present
		notWant []string // glyphs that must be absent
	}{
		{"running animates spinner", types.ToolStatusRunning, spinnerGlyph, []string{"✓", "✗"}},
		{"pending animates spinner", types.ToolStatusPending, spinnerGlyph, []string{"✓", "✗"}},
		{"completed shows check", types.ToolStatusCompleted, "✓", []string{spinnerGlyph, "✗"}},
		{"error shows cross", types.ToolStatusError, "✗", []string{spinnerGlyph, "✓"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := renderCard(tc.status)

			assert.Regexp(t, `root\s+calls\s+researcher`, out, "header should name parent and child agents")
			assert.Contains(t, out, "Investigate the flaky test", "task text should always render")
			assert.Contains(t, out, tc.want, "expected status glyph missing")
			for _, n := range tc.notWant {
				assert.NotContains(t, out, n, "unexpected glyph %q present for %s", n, tc.name)
			}
		})
	}
}

// TestTransferTaskCard_IconWidthIsStable guards the A.1 fixed-width contract:
// the icon stays a single glyph with no elapsed-time suffix, so the wrapped
// task text keeps the same indent across statuses. Swapping in an elapsed-time
// icon (e.g. toolcommon.Icon) would shift the running task column and regress.
func TestTransferTaskFallsBackToToolHeaderWhenArgumentsCannotParse(t *testing.T) {
	t.Parallel()

	msg := types.ToolCallMessage("root", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      transfertasktool.ToolNameTransferTask,
			Arguments: `{"agent":`,
		},
	}, tools.Tool{Name: transfertasktool.ToolNameTransferTask}, types.ToolStatusPending)

	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	_ = view.SetSize(80, 0)

	assert.Contains(t, stripANSI(view.View()), transfertasktool.ToolNameTransferTask)
}

func TestTransferTaskCard_IconWidthIsStable(t *testing.T) {
	t.Parallel()

	taskColumn := func(status types.ToolStatus) int {
		// Layout is "header\n\n<icon> task", so the task block is the 3rd part.
		parts := strings.SplitN(renderCard(status), "\n", 3)
		return strings.Index(parts[len(parts)-1], "Investigate")
	}

	running := taskColumn(types.ToolStatusRunning)
	assert.Positive(t, running, "task text should be indented past the icon")
	assert.Equal(t, running, taskColumn(types.ToolStatusPending))
	assert.Equal(t, running, taskColumn(types.ToolStatusCompleted))
	assert.Equal(t, running, taskColumn(types.ToolStatusError))
}
