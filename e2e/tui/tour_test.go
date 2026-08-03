package tui_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

// TestTour_WalkThrough drives the getting-started tour end to end: the card
// appears on launch, Enter skips action steps, a real Ctrl+k completes the
// palette step, read steps advance with Enter, and finishing the tour
// removes the card. No LLM call is made, so the cassette is empty.
func TestTour_WalkThrough(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithTourStart())

	d.WaitFor(tuitest.Contains("Getting started")).
		WaitFor(tuitest.Contains("Talk to your agent")).
		Enter(). // skip step 1 (send a message)
		WaitFor(tuitest.Contains("Tools run with your OK")).
		Enter(). // skip step 2 (tool approval)
		WaitFor(tuitest.Contains("One menu for everything"))

	// Step 3 performed for real: Ctrl+k opens the palette and completes the
	// step. Esc closes the palette (not the tour) and the celebration timer
	// advances to the slash-commands step.
	d.Press('k', tea.ModCtrl).
		WaitFor(tuitest.Contains("Type to search commands")).
		Press(tea.KeyEscape).
		WaitFor(tuitest.Contains("Slash commands")).
		Enter(). // skip step 4 (slash commands)
		WaitFor(tuitest.Contains("Agents are just YAML")).
		WaitFor(tuitest.Contains(invocation.DockerAgent() + " new")).
		Enter(). // read step
		WaitFor(tuitest.Contains("You're all set")).
		Enter(). // finish
		WaitFor(tuitest.Contains("Tour complete")).
		WaitFor(tuitest.Absent("Getting started"))
}

// TestTour_EscQuits checks the "skippable at any point" guardrail: Esc
// dismisses the tour and points at the replay command.
func TestTour_EscQuits(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithTourStart())

	d.WaitFor(tuitest.Contains("Getting started")).
		Press(tea.KeyEscape).
		WaitFor(tuitest.Absent("Talk to your agent")).
		WaitFor(tuitest.Contains("/getting-started"))
}

// TestTour_FirstRunOffer exercises the first-run prompt: the offer dialog
// appears, "n" declines it for this run, and a hint explains how to start
// the tour later.
func TestTour_FirstRunOffer(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithTourOffer(false))

	d.WaitFor(tuitest.Contains("Welcome to docker agent")).
		WaitFor(tuitest.Contains("take the tour")).
		Type("n").
		WaitFor(tuitest.Absent("take the tour")).
		WaitFor(tuitest.Contains("Maybe later"))
}
