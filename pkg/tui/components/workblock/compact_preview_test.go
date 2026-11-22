package workblock

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func compactPreviewFixture(t *testing.T) (*Model, *reasoningblock.Model, *reasoningblock.Model) {
	t.Helper()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	first := reasoningblock.New(ar, "first", "root", service.StaticSessionState{})
	first.SetReasoning("alpha\n\nbeta\n\ngamma")
	second := reasoningblock.New(ar, "second", "root", service.StaticSessionState{})
	second.SetReasoning("delta\n\nepsilon")
	model := New(ar)
	model.SetSize(60, 20)
	model.SetItems([]Item{{SourceIndex: 1, View: first}, {SourceIndex: 2, View: second}})
	t.Cleanup(model.StopAnimation)
	return model, first, second
}

func countLinesWith(view string, words ...string) int {
	n := 0
	for line := range strings.SplitSeq(view, "\n") {
		for _, word := range words {
			if strings.Contains(line, word) {
				n++
				break
			}
		}
	}
	return n
}

func TestCollapsedWorkShowsThreeMostRecentReasoningLines(t *testing.T) {
	model, first, second := compactPreviewFixture(t)
	require.False(t, model.Expanded())
	require.False(t, first.IsExpanded(), "nested reasoning keeps its own state while Work is collapsed")
	require.False(t, second.IsExpanded())

	view := ansi.Strip(model.View())
	require.Contains(t, view, "Work")
	require.NotContains(t, view, "Thinking", "collapsed Work shows lines, not nested thinking headers")
	require.NotContains(t, view, "alpha")
	require.Contains(t, view, "gamma")
	require.Contains(t, view, "delta")
	require.Contains(t, view, "epsilon")
	require.Equal(t, 3, countLinesWith(view, "gamma", "delta", "epsilon"), "exactly the three most recent lines are previewed")

	// Preview lines are inert: only the header row toggles the block.
	require.True(t, model.IsToggleAt(0, 5))
	require.Empty(t, model.Rows(), "collapsed preview lines add no source rows")
}

func TestExpandedWorkShowsNestedReasoningWithoutDisclosure(t *testing.T) {
	model, _, _ := compactPreviewFixture(t)
	model.SetExpandedImmediately(true)

	view := ansi.Strip(model.View())
	for _, word := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		require.Contains(t, view, word, "expanded Work shows all reasoning content")
	}
	require.NotContains(t, view, "Thinking", "hosted reasoning has no nested header")
	require.Equal(t, 1, strings.Count(view, "["), "only the Work header carries a disclosure marker")
	requireNoNestedToggle(t, model)

	model.SetExpandedImmediately(false)
	view = ansi.Strip(model.View())
	require.NotContains(t, view, "alpha")
	require.Contains(t, view, "gamma")
}

// Regression: nested reasoning cannot hide content inside an expanded Work block,
// whether it was collapsed beforehand, rebuilt by a projection, newly joined, or streamed.
func TestExpandedWorkNeverHidesNestedReasoning(t *testing.T) {
	model, first, _ := compactPreviewFixture(t)
	first.SetExpandedImmediately(false)
	model.SetExpandedImmediately(true)
	require.Contains(t, ansi.Strip(model.View()), "alpha", "a child collapsed before expansion still renders in full")

	model.SetItems([]Item{{SourceIndex: 1, View: first}, {SourceIndex: 2, View: compactPreviewChild(t, "second", "delta\n\nepsilon")}})
	require.Contains(t, ansi.Strip(model.View()), "alpha", "a projection rebuild keeps nested reasoning fully visible")

	joined := compactPreviewChild(t, "joined", "zeta\n\neta")
	model.SetItems([]Item{{SourceIndex: 1, View: first}, {SourceIndex: 3, View: joined}})
	require.Contains(t, ansi.Strip(model.View()), "eta", "reasoning joining an expanded Work block renders in full")

	joined.AppendReasoning("\n\ntheta")
	model.InvalidateSource(3)
	require.Contains(t, ansi.Strip(model.View()), "theta", "streamed reasoning appears in full inside expanded Work")

	requireNoNestedToggle(t, model)
	require.Contains(t, ansi.Strip(model.View()), "alpha", "clicks on hosted reasoning never collapse it")
}

// Released reasoning renders standalone again, with its own thinking header.
func TestReleasedReasoningRendersStandaloneWithHeader(t *testing.T) {
	model, first, second := compactPreviewFixture(t)
	model.SetExpandedImmediately(true)
	require.NotContains(t, ansi.Strip(first.View()), "Thinking", "hosted reasoning has no header")

	model.SetItems([]Item{{SourceIndex: 1, View: first}})
	require.Contains(t, ansi.Strip(second.View()), "Thinking", "a child that left the block renders standalone")
	require.NotContains(t, ansi.Strip(first.View()), "Thinking", "a child still in the block stays hosted")

	model.Release()
	require.Contains(t, ansi.Strip(first.View()), "Thinking", "releasing the block restores standalone rendering")
}

func compactPreviewChild(t *testing.T, id, text string) *reasoningblock.Model {
	t.Helper()
	child := reasoningblock.New(animation.NewRuntime(), id, "root", service.StaticSessionState{})
	child.SetReasoning(text)
	return child
}

// requireNoNestedToggle proves no content line inside the expanded Work block
// toggles anything: hosted reasoning exposes no disclosure at any column.
func requireNoNestedToggle(t *testing.T, model *Model) {
	t.Helper()
	view := strings.Split(ansi.Strip(model.View()), "\n")
	for line := 1; line < len(view); line++ {
		for col := range model.width {
			require.False(t, model.IsToggleAt(line, col), "content line %d col %d must not toggle", line, col)
		}
	}
}
