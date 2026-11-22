package workblock

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// rowContaining returns the rendered row that carries text.
func rowContaining(t *testing.T, view, text string) string {
	t.Helper()
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, text) {
			return line
		}
	}
	t.Fatalf("no row contains %q in:\n%s", text, view)
	return ""
}

// Compact thinking dims the first preview line when earlier reasoning is cut
// off above it. The collapsed Work preview must do the same, and only then.
func TestCollapsedWorkPreviewDimsFirstLineIffTruncated(t *testing.T) {
	faint := func(s string) string { return styles.MutedStyle.Italic(true).Faint(true).Render(s) }

	t.Run("truncated preview dims only its first row", func(t *testing.T) {
		model, _, _ := compactPreviewFixture(t)
		view := model.View()
		require.Contains(t, rowContaining(t, view, "gamma"), faint("gamma"), "earlier reasoning precedes the preview")
		require.Contains(t, rowContaining(t, view, "gamma"), "\x1b[3;2;", "the faint attribute (SGR 2) is what dims the row")
		require.NotContains(t, rowContaining(t, view, "delta"), "\x1b[3;2;", "later rows carry no faint attribute")
		require.NotContains(t, rowContaining(t, view, "delta"), faint("delta"), "later rows keep the plain style")
		require.NotContains(t, rowContaining(t, view, "epsilon"), faint("epsilon"), "later rows keep the plain style")
	})

	t.Run("untruncated preview dims nothing", func(t *testing.T) {
		ar := animation.NewRuntime()
		t.Cleanup(ar.Stop)
		child := reasoningblock.New(ar, "short", "root", service.StaticSessionState{})
		child.SetReasoning("one\n\ntwo\n\nthree")
		model := New(ar)
		model.SetSize(60, 20)
		model.SetItems([]Item{{SourceIndex: 1, View: child}})
		t.Cleanup(model.StopAnimation)
		view := model.View()
		for _, text := range []string{"one", "two", "three"} {
			require.NotContains(t, rowContaining(t, view, text), faint(text), "nothing precedes the preview")
		}
	})

	t.Run("several short children that together exceed three rows dim the first row", func(t *testing.T) {
		ar := animation.NewRuntime()
		t.Cleanup(ar.Stop)
		first := reasoningblock.New(ar, "first", "root", service.StaticSessionState{})
		first.SetReasoning("a1\n\na2")
		second := reasoningblock.New(ar, "second", "root", service.StaticSessionState{})
		second.SetReasoning("b1\n\nb2")
		model := New(ar)
		model.SetSize(60, 20)
		model.SetItems([]Item{{SourceIndex: 1, View: first}, {SourceIndex: 2, View: second}})
		t.Cleanup(model.StopAnimation)
		view := model.View()
		require.Contains(t, rowContaining(t, view, "a2"), faint("a2"), "reasoning precedes the preview row a2 in the first child")
		require.NotContains(t, rowContaining(t, view, "b1"), faint("b1"))
		require.NotContains(t, rowContaining(t, view, "b2"), faint("b2"))
	})

	t.Run("exactly three rows from one child dim nothing", func(t *testing.T) {
		ar := animation.NewRuntime()
		t.Cleanup(ar.Stop)
		child := reasoningblock.New(ar, "exact", "root", service.StaticSessionState{})
		child.SetReasoning("x1\n\nx2\n\nx3")
		model := New(ar)
		model.SetSize(60, 20)
		model.SetItems([]Item{{SourceIndex: 1, View: child}})
		t.Cleanup(model.StopAnimation)
		view := model.View()
		for _, text := range []string{"x1", "x2", "x3"} {
			require.NotContains(t, rowContaining(t, view, text), faint(text))
		}
	})
}
