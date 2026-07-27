package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newConfirmationEvent builds a tool-call confirmation event carrying the
// supplied metadata for use in the dialog tests.
func newConfirmationEvent(metadata map[string]string) *runtime.ToolCallConfirmationEvent {
	return &runtime.ToolCallConfirmationEvent{
		Type:           "tool_call_confirmation",
		ToolCall:       tools.ToolCall{ID: "x", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}},
		ToolDefinition: tools.Tool{Name: "shell"},
		Metadata:       metadata,
	}
}

func TestToolConfirmationDialog_RendersMetadata(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{"danger": "high", "reason": "policy-x"}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Metadata")
	assert.Contains(t, view, "danger: high")
	assert.Contains(t, view, "reason: policy-x")
}

func TestToolConfirmationDialog_NoMetadataSection_WhenEmpty(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(newConfirmationEvent(nil), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.NotContains(t, view, "Metadata")
}

func TestToolConfirmationDialog_MetadataKeysSorted(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{"zebra": "1", "apple": "2", "mango": "3"}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	apple := strings.Index(view, "apple:")
	mango := strings.Index(view, "mango:")
	zebra := strings.Index(view, "zebra:")
	require.NotEqual(t, -1, apple)
	require.NotEqual(t, -1, mango)
	require.NotEqual(t, -1, zebra)
	assert.Less(t, apple, mango, "keys must render in sorted order")
	assert.Less(t, mango, zebra, "keys must render in sorted order")
}

// TestToolConfirmationDialog_RendersSafetyWarning pins the destructive-
// command UX: when the confirmation event carries the safer_shell
// builtin's `blast_radius` metadata, the dialog composes a polished
// warning block instead of rendering raw key/value pairs. The
// convention keys (blast_radius, category, reason) are suppressed
// from the plain Metadata section.
func TestToolConfirmationDialog_RendersSafetyWarning(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{
			"blast_radius": "high",
			"category":     "fs-delete",
			"reason":       "Command matches destructive operation: rm -rf <path>",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Destructive command", "warning block must name what it's warning about")
	assert.Contains(t, view, "high blast radius", "warning block must name the severity in prose")
	assert.Contains(t, view, "Command matches destructive operation: rm -rf <path>",
		"the matched-pattern reason must surface as supporting context")
	assert.NotContains(t, view, "blast_radius:",
		"raw blast_radius key must not appear; the warning block replaces it")
	assert.NotContains(t, view, "category: fs-delete",
		"raw category key must not appear when blast_radius is in play")
	assert.NotContains(t, view, "Metadata",
		"the plain Metadata section must not render when only convention keys are present")
}

// The runtime attaches safety_label to every confirmation for API
// consumers; the dialog must not leak it as a raw metadata row.
func TestToolConfirmationDialog_SafetyLabelNeverRendersRaw(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{
			"safety_label": "destructive",
			"blast_radius": "high",
			"reason":       "Command matches destructive operation: rm -rf <path>",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Destructive command")
	assert.NotContains(t, view, "safety_label:",
		"programmatic classifier state must not render as a raw pair")
}

// An unrecognised command must not be presented as destructive: crying
// wolf on every unlisted command would erode trust in real warnings.
func TestToolConfirmationDialog_UnknownRadiusIsNotDestructive(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{
			"safety_label": "unknown",
			"blast_radius": "unknown",
			"reason":       "Shell command is not positively recognised by the safety classifier.",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Unrecognised command")
	assert.NotContains(t, view, "Destructive command")
}

// TestToolConfirmationDialog_RendersSafetyWarningPlusExtraMetadata
// covers the case where a permission_request hook contributes its own
// metadata alongside safer_shell's verdict. The warning block uses
// the safer_shell convention keys, and the extra keys still render
// as plain pairs in the Metadata section.
func TestToolConfirmationDialog_RendersSafetyWarningPlusExtraMetadata(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{
			"blast_radius": "medium",
			"reason":       "rm without recursion flag",
			"team_policy":  "review-required",
			"ticket":       "SEC-1234",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "medium blast radius", "warning block consumes blast_radius")
	assert.Contains(t, view, "rm without recursion flag", "warning block consumes reason")
	assert.Contains(t, view, "team_policy: review-required",
		"non-convention keys still render as plain pairs")
	assert.Contains(t, view, "ticket: SEC-1234")
}

// TestToolConfirmationDialog_ReasonOutsideSafetyVerdictRendersPlain
// pins the orthogonality: the `reason` key is generic and can be
// used by permission_request hooks for unrelated purposes. When
// blast_radius is NOT present, reason renders as a plain pair so
// existing permission_request consumers aren't affected.
func TestToolConfirmationDialog_ReasonOutsideSafetyVerdictRendersPlain(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(
		newConfirmationEvent(map[string]string{"reason": "policy-x"}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Metadata")
	assert.Contains(t, view, "reason: policy-x",
		"without blast_radius, reason is just a regular metadata key")
	assert.NotContains(t, view, "Destructive command",
		"no warning block without a blast_radius classification")
}

// The dialog must work with an embedder-provided session state, not just the
// full application's *service.SessionState; the "all tools" decision flips
// the embedder's session-wide approval.
func TestToolConfirmationDialog_EmbeddedSessionState(t *testing.T) {
	t.Parallel()

	state := &service.EmbeddedSessionState{}
	dialog := NewToolConfirmationDialog(newConfirmationEvent(nil), state)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "shell")
	assert.Contains(t, view, "Do you want to allow this tool call?")

	require.False(t, state.YoloMode())
	_, cmd := dialog.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	require.NotNil(t, cmd)
	assert.True(t, state.YoloMode(), "approving all tools must flip the embedder's session-wide approval")
}

// Clicking the leading action key must fire at every dialog width.
// RenderHelpKeys bakes lipgloss centering spaces into the options line;
// hit-testing must factor them out or 'Y' clicks dispatch on a padding
// space and silently no-op whenever the centering padding is odd.
func TestToolConfirmationDialog_ClickOnYFiresAtEveryWidth(t *testing.T) {
	t.Parallel()

	tested := 0
	for width := 60; width <= 130; width++ {
		dialog := NewToolConfirmationDialog(newConfirmationEvent(nil), &service.SessionState{})
		_, _ = dialog.Update(tea.WindowSizeMsg{Width: width, Height: 30})

		d, ok := dialog.(*toolConfirmationDialog)
		require.True(t, ok)

		// Locate the rendered options line and the on-screen column of
		// its 'Y' key, then click it.
		dialogRow, dialogCol := d.Position()
		view := ansi.Strip(d.View())
		lines := strings.Split(view, "\n")
		optionsRow := ContentEndRow(dialogRow, len(lines))
		optionsLine := lines[optionsRow-dialogRow]
		yIdx := strings.Index(optionsLine, "Y yes")
		if yIdx < 0 {
			// Narrow widths wrap the help line; hit-testing only
			// targets the single-line layout.
			continue
		}
		tested++

		_, cmd := d.handleMouseClick(tea.MouseClickMsg{
			X:      dialogCol + yIdx,
			Y:      optionsRow,
			Button: tea.MouseLeft,
		})
		assert.NotNilf(t, cmd, "width %d: click on 'Y' at col %d must fire", width, dialogCol+yIdx)
	}
	// The sweep must cover several widths (both centering parities).
	assert.Greater(t, tested, 10)
}

// Separator gaps between the option segments are dead zones: a click
// there fires nothing, because attributing the gap to either neighbour
// would fire some action on a near-miss (left would make the Y/N gap
// approve, right would make the B/A gap go autonomous).
func TestToolConfirmationDialog_GapClicksAreDeadZones(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(newConfirmationEvent(nil), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	d, ok := dialog.(*toolConfirmationDialog)
	require.True(t, ok)

	dialogRow, dialogCol := d.Position()
	view := ansi.Strip(d.View())
	lines := strings.Split(view, "\n")
	optionsRow := ContentEndRow(dialogRow, len(lines))
	optionsLine := lines[optionsRow-dialogRow]

	click := func(col int) tea.Cmd {
		_, cmd := d.handleMouseClick(tea.MouseClickMsg{
			X:      dialogCol + col,
			Y:      optionsRow,
			Button: tea.MouseLeft,
		})
		return cmd
	}

	// Clicking inside a segment's text fires.
	nIdx := strings.Index(optionsLine, "N no")
	require.GreaterOrEqual(t, nIdx, 0)
	assert.NotNil(t, click(nIdx), "click on 'N' must fire")
	assert.NotNil(t, click(nIdx+3), "click on N's label must fire")

	// Clicking the two-space gap between "Y yes" and "N no" fires nothing.
	assert.Nil(t, click(nIdx-1), "gap click must be a dead zone")
	assert.Nil(t, click(nIdx-2), "gap click must be a dead zone")

	// The trailing segment is clickable to its last character.
	aIdx := strings.Index(optionsLine, "A all tools")
	require.GreaterOrEqual(t, aIdx, 0)
	assert.NotNil(t, click(aIdx), "click on 'A' must fire")
	assert.NotNil(t, click(aIdx+len("A all tools")-1), "click on the last label char must fire")
	assert.Nil(t, click(aIdx+len("A all tools")), "click past the line must be a no-op")
}
