package toolconfirmation

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/dialog/common"
	"github.com/docker/docker-agent/pkg/tui/dialog/internal/testutil"
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.NotContains(t, view, "Metadata")
}

func TestToolConfirmationDialog_MetadataKeysSorted(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
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
	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), state)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "shell")
	assert.Contains(t, view, "Do you want to allow this tool call?")

	require.False(t, state.YoloMode())
	_, cmd := dialog.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	require.NotNil(t, cmd)
	assert.True(t, state.YoloMode(), "approving all tools must flip the embedder's session-wide approval")
}

// locateInView finds needle in the rendered dialog and returns the
// absolute screen row and column (terminal cells) of its first cell.
// The prefix before needle is measured in cells, not bytes, because
// the dialog border rune is multi-byte and mouse coordinates are cells.
func locateInView(d *toolConfirmationDialog, needle string) (y, x int, ok bool) {
	dialogRow, dialogCol := d.Position()
	for i, line := range strings.Split(ansi.Strip(d.View()), "\n") {
		if before, _, found := strings.Cut(line, needle); found {
			return dialogRow + i, dialogCol + ansi.StringWidth(before), true
		}
	}
	return 0, 0, false
}

// clickCell dispatches a left click at the given absolute cell position.
func clickCell(d *toolConfirmationDialog, y, x int) tea.Cmd {
	_, cmd := d.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

// Clicking the leading action key must fire at every dialog width,
// including widths where the options block wraps onto several rows.
// RenderHelpKeys bakes lipgloss centering spaces into each line;
// hit-testing must factor them out or 'Y' clicks dispatch on a padding
// space and silently no-op whenever the centering padding is odd.
func TestToolConfirmationDialog_ClickOnYFiresAtEveryWidth(t *testing.T) {
	t.Parallel()

	for width := 60; width <= 130; width++ {
		dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
		_, _ = dialog.Update(tea.WindowSizeMsg{Width: width, Height: 30})

		d, ok := dialog.(*toolConfirmationDialog)
		require.True(t, ok)

		y, x, found := locateInView(d, "Y yes")
		require.Truef(t, found, "width %d: 'Y yes' must be visible on some options row", width)

		resume, ok := testutil.FindMsg[RuntimeResumeMsg](testutil.CollectMsgs(clickCell(d, y, x)))
		require.Truef(t, ok, "width %d: click on 'Y' at col %d must fire", width, x)
		assert.Equalf(t, runtime.ResumeApprove(), resume.Request, "width %d: 'Y' must approve the single call", width)
	}
}

// Adding the Balanced option makes the options block wrap at everyday
// terminal sizes (80 columns → ~50 columns of dialog content). The
// block must break only at segment separators, and every segment on
// every wrapped row must stay clickable.
func TestToolConfirmationDialog_WrappedLayoutClicksDispatch(t *testing.T) {
	t.Parallel()

	state := &service.EmbeddedSessionState{}
	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), state)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 80, Height: 30})

	d, ok := dialog.(*toolConfirmationDialog)
	require.True(t, ok)

	_, contentWidth := d.dialogDimensions()
	rows := d.optionRows(contentWidth)
	require.Greater(t, len(rows), 1, "width 80 must wrap the options block")
	for _, row := range rows {
		assert.LessOrEqual(t, ansi.StringWidth(optionRowText(row)), contentWidth,
			"rows must break at segment separators, never overflow")
	}
	require.Equal(t, len(rows), lipgloss.Height(d.renderOptions(contentWidth)),
		"each laid-out row must render as exactly one line")

	bY, bX, found := locateInView(d, "B balanced")
	require.True(t, found, "the Balanced segment must be visible")
	aY, aX, found := locateInView(d, "A all tools")
	require.True(t, found, "the all-tools segment must be visible")
	require.NotEqual(t, bY, aY, "the wrapped layout must spread segments across rows")

	resume, ok := testutil.FindMsg[RuntimeResumeMsg](testutil.CollectMsgs(clickCell(d, bY, bX)))
	require.True(t, ok, "click on 'B balanced' must dispatch a resume")
	assert.Equal(t, runtime.ResumeApproveBalanced(), resume.Request)

	require.False(t, state.YoloMode())
	resume, ok = testutil.FindMsg[RuntimeResumeMsg](testutil.CollectMsgs(clickCell(d, aY, aX)))
	require.True(t, ok, "click on 'A all tools' on the wrapped row must dispatch a resume")
	assert.Equal(t, runtime.ResumeApproveAutonomous(), resume.Request)
	assert.True(t, state.YoloMode(), "the all-tools click must flip the session-wide approval")
}

// A realistic narrow split pane (45 columns → 25 columns of dialog
// content) lays the options out on at least three rows, with the
// always-allow segment on a middle row. Clicking there must dispatch
// that segment's decision: the hit-map counts rows from the dialog's
// bottom line, so an off-by-one shows up exactly on middle rows.
func TestToolConfirmationDialog_MiddleRowClickInNarrowSplitPane(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 45, Height: 30})

	d, ok := dialog.(*toolConfirmationDialog)
	require.True(t, ok)

	_, contentWidth := d.dialogDimensions()
	rows := d.optionRows(contentWidth)
	require.GreaterOrEqual(t, len(rows), 3, "width 45 must lay the options out on at least three rows")
	middle := rows[1]
	require.Equal(t, "T", middle[0].action, "the always-allow segment must sit on a middle row")

	tY, tX, found := locateInView(d, optionRowText(middle))
	require.True(t, found, "the middle row must be visible")

	resume, ok := testutil.FindMsg[RuntimeResumeMsg](testutil.CollectMsgs(clickCell(d, tY, tX)))
	require.True(t, ok, "click on the middle row's action key must dispatch a resume")
	assert.Equal(t, runtime.ResumeApproveTool("shell"), resume.Request,
		"the middle-row click must grant this tool, not a neighbouring row's decision")
}

// expectedOptionResume is the resume request each clickable action key
// dispatches for the newConfirmationEvent fixture (pattern "shell").
// "N" is absent: rejection opens the reason dialog instead of resuming.
var expectedOptionResume = map[string]runtime.ResumeRequest{
	"Y": runtime.ResumeApprove(),
	"T": runtime.ResumeApproveTool("shell"),
	"B": runtime.ResumeApproveBalanced(),
	"A": runtime.ResumeApproveAutonomous(),
}

// assertOptionClick clicks the given cell and asserts it dispatches
// exactly the clicked segment's decision — anything else is a misdispatch.
func assertOptionClick(t *testing.T, d *toolConfirmationDialog, seg optionSegment, y, x, width int) {
	t.Helper()
	msgs := testutil.CollectMsgs(clickCell(d, y, x))
	if seg.action == "N" {
		assert.Truef(t, testutil.HasMsg[common.OpenDialogMsg](msgs),
			"width %d: click on %q must open the rejection-reason dialog", width, seg.action)
		return
	}
	resume, ok := testutil.FindMsg[RuntimeResumeMsg](msgs)
	require.Truef(t, ok, "width %d: click on %q must dispatch a resume", width, seg.action)
	assert.Equalf(t, expectedOptionResume[seg.action], resume.Request,
		"width %d: click on %q dispatched another segment's decision", width, seg.action)
}

// Tiny terminals — all the way down to zero columns — must never wrap a
// laid-out option row across physical lines: the layout width is clamped
// so even a fully truncated "<key> …" segment fits its row, the extremely
// narrow terminal clips the dialog instead. The sweep proves the
// invariants the hit-map depends on: renderOptions' physical height
// equals optionRows' logical row count, every row renders on the physical
// line the hit-map expects, clicks dispatch exactly the segment they land
// on, and near-misses stay dead. A wrapped or misaligned row wouldn't
// panic — it would silently fire the wrong decision, the one failure mode
// a confirmation dialog cannot have.
func TestToolConfirmationDialog_TinyWidthsKeepRowsAlignedAndClickable(t *testing.T) {
	t.Parallel()

	for width := range 60 {
		dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
		_, _ = dialog.Update(tea.WindowSizeMsg{Width: width, Height: 30})

		d, ok := dialog.(*toolConfirmationDialog)
		require.True(t, ok)

		_, contentWidth := d.dialogDimensions()
		require.GreaterOrEqualf(t, contentWidth, toolConfirmMinOptionWidth,
			"width %d: the layout width must clamp to fit a minimal segment", width)
		rows := d.optionRows(contentWidth)
		require.NotEmptyf(t, rows, "width %d: the options must always lay out", width)
		for _, row := range rows {
			assert.LessOrEqualf(t, ansi.StringWidth(optionRowText(row)), contentWidth,
				"width %d: no row may exceed the clamped layout width", width)
		}
		require.Equalf(t, len(rows), lipgloss.Height(d.renderOptions(contentWidth)),
			"width %d: each logical row must render as exactly one physical line", width)

		// Walk the rows bottom-anchored, exactly like handleMouseClick.
		dialogRow, dialogCol := d.Position()
		view := d.View()
		renderedLines := strings.Split(ansi.Strip(view), "\n")
		endY := common.ContentEndRow(dialogRow, lipgloss.Height(view))
		for i, row := range rows {
			y := endY - (len(rows) - 1) + i
			line := renderedLines[y-dialogRow]
			byteStart := strings.Index(line, optionRowText(row))
			require.GreaterOrEqualf(t, byteStart, 0,
				"width %d: row %d must render whole on the physical line the hit-map expects", width, i)
			x := dialogCol + ansi.StringWidth(line[:byteStart])

			for _, seg := range row {
				assertOptionClick(t, d, seg, y, x+seg.startX, width)
				assertOptionClick(t, d, seg, y, x+seg.endX-1, width)
			}
			last := row[len(row)-1]
			assert.Nilf(t, clickCell(d, y, x-1), "width %d: the cell before row %d must be dead", width, i)
			assert.Nilf(t, clickCell(d, y, x+last.endX), "width %d: the cell after row %d must be dead", width, i)
			for s := 1; s < len(row); s++ {
				assert.Nilf(t, clickCell(d, y, x+row[s].startX-1),
					"width %d: the separator gap on row %d must be dead", width, i)
			}
		}
	}
}

// A pathological permission pattern (very long first command word) must
// not overflow the dialog: the lone oversize segment's displayed label
// truncates, while the key stays visible and a click still grants the
// full untruncated pattern.
func TestToolConfirmationDialog_OversizeSegmentTruncatesLabel(t *testing.T) {
	t.Parallel()

	longWord := "very-long-binary-name-" + strings.Repeat("x", 100)
	event := newConfirmationEvent(nil)
	event.ToolCall.Function.Arguments = `{"cmd":"` + longWord + ` --flag"}`

	dialog := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 80, Height: 30})

	d, ok := dialog.(*toolConfirmationDialog)
	require.True(t, ok)

	wantPattern := "shell:cmd=" + longWord + "*"
	require.Equal(t, wantPattern, d.permissionPattern)

	_, contentWidth := d.dialogDimensions()
	rows := d.optionRows(contentWidth)
	for _, row := range rows {
		assert.LessOrEqual(t, ansi.StringWidth(optionRowText(row)), contentWidth,
			"the oversize label must truncate so no row exceeds the content width")
	}
	require.Equal(t, len(rows), lipgloss.Height(d.renderOptions(contentWidth)),
		"each laid-out row must render as exactly one line")
	assert.Contains(t, ansi.Strip(d.renderOptions(contentWidth)), "…",
		"the truncated label must show an ellipsis")

	tY, tX, found := locateInView(d, "T always allow")
	require.True(t, found, "the truncated segment must keep its key and label prefix visible")

	resume, ok := testutil.FindMsg[RuntimeResumeMsg](testutil.CollectMsgs(clickCell(d, tY, tX)))
	require.True(t, ok, "click on the truncated segment must dispatch a resume")
	assert.Equal(t, runtime.ResumeApproveTool(wantPattern), resume.Request,
		"the granted pattern must be the full untruncated pattern")
}

// Separator gaps between the option segments are dead zones: a click
// there fires nothing, because attributing the gap to either neighbour
// would fire some action on a near-miss (left would make the Y/N gap
// approve, right would make the B/A gap go autonomous).
func TestToolConfirmationDialog_GapClicksAreDeadZones(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	d, ok := dialog.(*toolConfirmationDialog)
	require.True(t, ok)

	// This width keeps the single-line layout: all segments on one row.
	_, contentWidth := d.dialogDimensions()
	require.Len(t, d.optionRows(contentWidth), 1)

	// Clicking inside a segment's text fires.
	nY, nX, found := locateInView(d, "N no")
	require.True(t, found)
	assert.NotNil(t, clickCell(d, nY, nX), "click on 'N' must fire")
	assert.NotNil(t, clickCell(d, nY, nX+3), "click on N's label must fire")

	// Clicking the two-space gap between "Y yes" and "N no" fires nothing.
	assert.Nil(t, clickCell(d, nY, nX-1), "gap click must be a dead zone")
	assert.Nil(t, clickCell(d, nY, nX-2), "gap click must be a dead zone")

	// The trailing segment is clickable to its last cell.
	aY, aX, found := locateInView(d, "A all tools")
	require.True(t, found)
	require.Equal(t, nY, aY, "single-line layout keeps every segment on one row")
	assert.NotNil(t, clickCell(d, aY, aX), "click on 'A' must fire")
	assert.NotNil(t, clickCell(d, aY, aX+ansi.StringWidth("A all tools")-1), "click on the last label cell must fire")
	assert.Nil(t, clickCell(d, aY, aX+ansi.StringWidth("A all tools")), "click past the line must be a no-op")
}

// A preempt/custom hook can force a confirmation while the session is
// already autonomous. Choosing Balanced switches the runtime session off
// autonomous, so it must also clear the cached TUI yolo flag — otherwise
// the sidebar keeps saying YOLO and Ctrl+Y "toggles" the session straight
// back to autonomous.
func TestToolConfirmationDialog_BalancedClearsYoloMode(t *testing.T) {
	t.Parallel()

	state := &service.EmbeddedSessionState{}
	state.SetYoloMode(true)
	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), state)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	_, cmd := dialog.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	require.NotNil(t, cmd)
	assert.False(t, state.YoloMode(), "choosing Balanced must drop the session-wide yolo flag")

	resume, ok := testutil.FindMsg[RuntimeResumeMsg](testutil.CollectMsgs(cmd))
	require.True(t, ok, "Balanced must dispatch a resume request")
	assert.Equal(t, runtime.ResumeApproveBalanced(), resume.Request)
}
