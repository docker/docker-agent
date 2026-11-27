// Package tui_test contains end-to-end tests for the docker-agent terminal UI.
//
// These tests drive the real top-level TUI model through the tuitest harness
// (pkg/tui/tuitest) against a replaying VCR proxy, so a whole user journey —
// type a prompt, submit it, watch the agent stream its answer — runs offline
// and deterministically. They are the regression net for the finished
// product: a visual or behavioral change in a covered screen surfaces as a
// failed matcher or a golden diff.
package tui_test

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// TestChat_BasicMath types a question, submits it, and waits for the agent's
// streamed answer to appear in the transcript. The agent's response is
// replayed from testdata/cassettes/TestChat_BasicMath.yaml.
func TestChat_BasicMath(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("What's 2+2?")). // the user's message echoes in the transcript
		WaitFor(tuitest.Contains("2 + 2 equals 4."))
}

// TestChat_PromptIsEditable proves typed input shows up in the editor before
// submission and moves into the transcript after sending — a basic but
// easy-to-regress piece of the finished product.
func TestChat_PromptIsEditable(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	d.Type("What's 2+2?").
		WaitFor(tuitest.Contains("What's 2+2?"))

	// After submitting, the draft is sent and the agent's reply streams in.
	d.Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))
}

// TestChat_MultilinePromptReturnsToOneRow types a newline into the prompt,
// which grows the bottom-anchored input upward by one row. Sending empties
// the input, which holds its height briefly and then returns to one row.
func TestChat_MultilinePromptReturnsToOneRow(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	d.Type("What's 2+2?").
		WaitFor(tuitest.Contains("What's 2+2?"))
	_, row := d.MustFindText("What's 2+2?")

	d.Press('j', tea.ModCtrl).
		WaitFor(textOnRow("What's 2+2?", row-1))

	d.Enter().
		WaitFor(textOnRow("Type your message here…", row-1)).
		WaitFor(tuitest.Contains("2 + 2 equals 4.")).
		WaitFor(textOnRow("Type your message here…", row))
}

func textOnRow(text string, row int) tuitest.Matcher {
	return tuitest.Matches(fmt.Sprintf(`^(?:.*\n){%d}.*%s`, row, regexp.QuoteMeta(text)))
}

func TestChat_CopyAssistantMessageToClipboard(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithHideSidebar())

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))

	d.MoveMouseToText("2 + 2 equals 4.").
		WaitFor(tuitest.Contains(types.MessageCopyLabel)).
		ClickText(types.MessageCopyLabel).
		WaitForClipboard("2 + 2 equals 4.").
		// The clicked label transiently reads "copied" instead of "⎘ copy".
		WaitFor(tuitest.Not(tuitest.Contains(types.MessageCopyLabel))).
		WaitFor(tuitest.Contains(types.CopiedFeedbackLabel))
}

// TestChat_DragSelectionCopiesAcrossRegions guards the mouse routing that
// keeps a text-selection drag alive outside the chat content region: the
// drag starts on the response text and releases near the bottom of the
// screen (over the editor/status area). The release must still finalize the
// selection and copy it.
func TestChat_DragSelectionCopiesAcrossRegions(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithHideSidebar())

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))

	x, y := d.MustFindText("2 + 2 equals 4.")
	d.Drag(x, y, x+30, 37).
		WaitForClipboard("2 + 2 equals 4.")
}

// TestCommandPalette_Opens exercises a pure-UI interaction that needs no agent
// response: Ctrl+K opens the command palette overlay, Esc closes it. It runs
// against an empty cassette since no LLM call is made.
func TestCommandPalette_Opens(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	// Wait for the UI to finish its initial render before interacting.
	d.WaitFor(tuitest.Not(tuitest.Contains("Loading")))

	// The palette shows a distinctive search placeholder when open.
	const placeholder = "Type to search commands"
	d.Assert(tuitest.Absent(placeholder))

	d.Press('k', tea.ModCtrl).
		WaitFor(tuitest.Contains(placeholder))

	// Esc closes the palette again.
	d.Press(tea.KeyEscape).
		WaitFor(tuitest.Absent(placeholder))
}

// TestGolden_Chat_BasicMath snapshots the full finished frame after the agent
// answers, so unintended visual drift in the chat surface shows up as a diff.
// Refresh the snapshot after an intentional UI change with:
//
//	go test ./e2e/tui/ -run TestGolden_Chat_BasicMath -tuitest.update
func TestGolden_Chat_BasicMath(t *testing.T) {
	// Hide the sidebar so the snapshot doesn't capture the machine-specific
	// working directory and git branch, and pin the version so a release
	// build's ldflags-injected version can't change the status bar. Both keep
	// the golden portable.
	d := newTUI(t, "testdata/basic.yaml", 120, 40,
		tui.WithHideSidebar(),
		tui.WithVersion("test"),
	)

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4.")).
		WaitForStable(200 * time.Millisecond).
		AssertGolden("chat_basic_math")
}
