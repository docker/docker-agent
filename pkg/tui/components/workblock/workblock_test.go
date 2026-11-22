package workblock

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(delay); return create(c.now) }
}

type fakeView struct {
	text        string
	views       int
	width       int
	hovers      []int
	wheelCols   []int
	toggleCols  []int
	toggleCount int
}

func (v *fakeView) Init() tea.Cmd                          { return nil }
func (v *fakeView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *fakeView) View() string                           { v.views++; return v.text }
func (v *fakeView) SetSize(width, _ int) tea.Cmd           { v.width = width; return nil }
func (v *fakeView) WheelAt(line, col, delta int) bool {
	v.wheelCols = append(v.wheelCols, col)
	return line >= 0 && col == 0 && delta != 0
}
func (v *fakeView) IsToggleAt(line, col int) bool { return line == 0 && col == 0 }
func (v *fakeView) ToggleAt(line, col int) {
	v.toggleCols = append(v.toggleCols, col)
	if v.IsToggleAt(line, col) {
		v.Toggle()
	}
}
func (v *fakeView) Toggle() { v.toggleCount++ }

func (v *fakeView) SetHoveredAt(line int, hovered bool) bool {
	v.hovers = append(v.hovers, line)
	return hovered
}

func advance(t *testing.T, ar *animation.Runtime, model *Model, _ *fakeClock, d time.Duration) {
	t.Helper()
	steps := int((d + animation.TickRate - 1) / animation.TickRate)
	// Lease starts made by state changes are returned by TakeAnimationCmd, as the root would deliver them.
	pending := model.TakeAnimationCmd()
	for range steps {
		if ar.ActiveCount() == 0 {
			return
		}
		cmd := pending
		if cmd == nil {
			cmd = ar.Continue()
		}
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		updated, next := model.Update(tick)
		require.Same(t, model, updated)
		pending = next
	}
}

func TestDefaultCollapsedAndExpansionRevealsAndMapsVisibleChildRows(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	child := &fakeView{text: "zero\none\ntwo\nthree"}
	model := New(ar)
	model.SetSize(60, 20)
	model.SetItems([]Item{{SourceIndex: 7, View: child}})

	require.False(t, model.IsExpanded())
	require.True(t, strings.HasPrefix(ansi.Strip(model.View()), "  Work [+]"), "header uses the shared two-cell transcript gutter")
	require.Equal(t, 1, model.Height())
	require.Empty(t, model.Rows())
	require.Equal(t, 0, child.views, "collapsed rendering must not render hidden children")

	model.SetExpanded(true)
	require.Equal(t, 1, model.Height(), "transition begins at the collapsed height")
	advance(t, ar, model, clock, 70*time.Millisecond)
	require.Greater(t, model.Height(), 1)
	require.LessOrEqual(t, model.Height(), 6)
	rows := model.Rows()
	require.Len(t, rows, 1)
	require.Equal(t, 2, rows[0].Start, "one blank row separates the header from its first child")
	require.Equal(t, model.Height(), rows[0].End)
	for line := rows[0].Start; line < rows[0].End; line++ {
		source, local, ok := model.SourceAt(line)
		require.True(t, ok)
		require.Equal(t, 7, source)
		require.Equal(t, line-2, local)
	}
	require.False(t, model.WheelAt(0, 2, 1), "header isn't a child wheel target")
	require.False(t, model.WheelAt(1, 2, 1), "the header spacer is not a child wheel target")
	require.False(t, model.WheelAt(2, 0, 1), "first left margin does not forward wheel events")
	require.False(t, model.WheelAt(2, 1, 1), "second left margin does not forward wheel events")
	require.True(t, model.WheelAt(2, 2, 1))
	require.Equal(t, []int{0}, child.wheelCols, "wheel coordinates are translated to the source view")

	for model.disclosure.NeedsTick() {
		advance(t, ar, model, clock, 30*time.Millisecond)
	}
	require.Equal(t, 6, model.Height())
	require.Equal(t, 1, child.views, "body cache prevents redundant child rendering")

	model.SetExpanded(false)
	require.Equal(t, 6, model.Height(), "collapse starts from the current height")
	for model.disclosure.NeedsTick() {
		advance(t, ar, model, clock, 30*time.Millisecond)
	}
	require.Equal(t, 1, model.Height())
	require.Empty(t, model.Rows())
	model.StopAnimation()
	require.Zero(t, ar.ActiveCount())
}

func TestInvalidationAndStatusDoNotDoubleUpdateOrStaleHeader(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	child := &fakeView{text: "first\nsecond"}
	model := New(ar)
	model.SetSize(80, 20)
	model.SetItems([]Item{{SourceIndex: 1, View: child}})
	model.SetExpanded(true)
	for model.disclosure.NeedsTick() {
		advance(t, ar, model, clock, 50*time.Millisecond)
	}
	model.View()
	require.Equal(t, 1, child.views)

	model.SetStatus(Status{Active: true, ToolCount: 2})
	require.Contains(t, ansi.Strip(model.View()), "Working")
	require.Contains(t, ansi.Strip(model.View()), "(2 tools)")
	require.Equal(t, 1, child.views, "status/header changes must not rerender cached child body")
	advance(t, ar, model, clock, animation.ChatSpinnerFrameDuration)
	require.Contains(t, ansi.Strip(model.View()), "Working")
	require.Equal(t, 1, child.views, "spinner ticks must only recompose the header")

	child.text = "changed\nchild"
	model.Invalidate()
	require.Contains(t, model.View(), "changed")
	require.Equal(t, 2, child.views)

	replacement := &fakeView{text: "replacement"}
	model.SetItems([]Item{{SourceIndex: 1, View: replacement}})
	require.Contains(t, model.View(), "replacement")
	require.Equal(t, 1, replacement.views)
	model.SetStatus(Status{})
	model.StopAnimation()
	require.Zero(t, ar.ActiveCount())
}

func TestHeaderSpinnerPrecedesWorkingAndCountsTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   Status
		expanded bool
		want     string
	}{
		{name: "reasoning only", want: "  Work [+]"},
		{name: "active without tools", status: Status{Active: true}, want: "  %s Working [+]"},
		{name: "active singular", status: Status{Active: true, ToolCount: 1, Elapsed: time.Minute}, want: "  %s Working [+] (1 tool)"},
		{name: "active plural expanded", status: Status{Active: true, ToolCount: 2}, expanded: true, want: "  %s Working [-] (2 tools)"},
		{name: "settled singular", status: Status{ToolCount: 1}, want: "  Work [+] (1 tool)"},
		{name: "settled sub-second", status: Status{Elapsed: 999 * time.Millisecond}, want: "  Work [+]"},
		{name: "settled plural elapsed", status: Status{ToolCount: 12, Elapsed: 3200 * time.Millisecond}, want: "  Work [+] (12 tools) · 3s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := animation.NewRuntimeWithScheduler(&fakeClock{now: time.Unix(1, 0)})
			model := New(ar)
			t.Cleanup(model.StopAnimation)
			model.SetExpanded(tc.expanded)
			model.SetStatus(tc.status)
			want := strings.ReplaceAll(tc.want, "%s", model.spinner.RawFrame())
			require.Equal(t, want, ansi.Strip(model.header()))
			label := "Work"
			if tc.status.Active {
				label = "Working"
			}
			require.Contains(t, model.header(), workHeaderStyle().Render(label))
			indicator := " [+]"
			if tc.expanded {
				indicator = " [-]"
			}
			require.Contains(t, model.header(), styles.MutedStyle.Bold(true).Render(indicator))
			for _, width := range []int{0, 1, 4, 8, 16, 24} {
				model.SetSize(width, 20)
				header := ansi.Strip(model.header())
				require.LessOrEqual(t, ansi.StringWidth(header), width)
				require.NotContains(t, header, "\n")
			}
		})
	}
}

func TestExpandedBodyResizesChildrenForBothMarginsAndNarrowWidths(t *testing.T) {
	model := New(animation.NewRuntime())
	child := &fakeView{text: strings.Repeat("x", 18)}
	model.SetSize(22, 10)
	model.SetItems([]Item{{SourceIndex: 1, View: child}})
	model.SetExpandedImmediately(true)
	view := ansi.Strip(model.View())
	line := strings.Split(view, "\n")[2]
	require.Equal(t, 18, child.width, "child source owns its reduced render width")
	require.Equal(t, 22, ansi.StringWidth(line), "visible body line includes left and right margins")
	require.True(t, strings.HasPrefix(line, " "))
	require.True(t, strings.HasSuffix(line, " "))
	require.Equal(t, 18, ansi.StringWidth(line[2:20]), "the complete source row, not a clipped full-width row, is nested")

	child.text = strings.Repeat("x", 6)
	model.SetSize(9, 10)
	require.Equal(t, 7, child.width)
	resized := ansi.Strip(model.View())
	require.Contains(t, resized, "x", "a resize invalidates prior cached child rendering")
	for renderedLine := range strings.SplitSeq(resized, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(renderedLine), 9)
	}
	require.Equal(t, 2, child.views)
	model.StopAnimation()
}

func TestWorkHeaderThemeUsesAccentAndRemainsBoldItalicAfterThemeSwitch(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme := styles.DefaultTheme()
	theme.Colors.Accent = "#e719a5"
	styles.ApplyTheme(theme)
	model := New(animation.NewRuntime())
	model.SetStatus(Status{Active: true})
	header := model.header()
	require.Contains(t, header, workHeaderStyle().Render("Working"))
	require.True(t, workHeaderStyle().GetBold())
	require.True(t, workHeaderStyle().GetItalic())
	require.Equal(t, "#e719a5", styles.RGBToHex(styles.ColorToRGB(workHeaderStyle().GetForeground())))
	styles.ApplyTheme(styles.DefaultTheme())
	changed := New(animation.NewRuntime())
	changed.SetStatus(Status{Active: true})
	changedHeader := changed.header()
	require.Contains(t, changedHeader, workHeaderStyle().Render("Working"))
	require.True(t, workHeaderStyle().GetBold())
	require.True(t, workHeaderStyle().GetItalic())
	require.NotEqual(t, "#e719a5", styles.RGBToHex(styles.ColorToRGB(workHeaderStyle().GetForeground())))
	changed.StopAnimation()
	model.StopAnimation()
}

func TestThinkingStyleDisclosureTogglesAtIndicator(t *testing.T) {
	for _, active := range []bool{false, true} {
		model := New(animation.NewRuntime())
		t.Cleanup(model.StopAnimation)
		model.SetItems([]Item{{SourceIndex: 1, View: &fakeView{text: "child body"}}})
		model.SetStatus(Status{Active: active, ToolCount: 1})
		header := ansi.Strip(model.header())
		col := ansi.StringWidth(strings.SplitN(header, "[+]", 2)[0]) + 1
		require.True(t, model.IsToggleAt(0, col))
		model.ToggleAt(0, col)
		require.True(t, model.Expanded())
		require.Contains(t, ansi.Strip(model.header()), "[-] (1 tool)")
		model.SetSize(81, 20)
		require.Contains(t, model.View(), "child body")
		model.ToggleAt(0, col)
		require.False(t, model.Expanded())
		require.Contains(t, ansi.Strip(model.header()), "[+] (1 tool)")
		model.SetSize(80, 20)
		require.NotContains(t, model.View(), "child body")
	}
}

func TestSectionSpacingPreservesChildRowMappings(t *testing.T) {
	ar := animation.NewRuntime()
	child := &fakeView{text: "first child line\nlast child line"}
	model := New(ar)
	model.SetItems([]Item{{SourceIndex: 8, View: child}, {SourceIndex: 9, View: &fakeView{text: "next item"}}})
	model.SetExpandedImmediately(true)
	view := ansi.Strip(model.View())
	require.NotContains(t, view, "Earlier reply")
	require.Contains(t, view, "last child line")
	require.Contains(t, view, "\n\n", "each source section has a consistent blank line before the next")
	firstLine := strings.Index(view, "first child line")
	lastLine := strings.Index(view, "last child line")
	require.GreaterOrEqual(t, firstLine, 0)
	require.Greater(t, lastLine, firstLine)
	firstRow := strings.Count(view[:firstLine], "\n")
	lastRow := strings.Count(view[:lastLine], "\n")
	source, local, ok := model.SourceAt(firstRow)
	require.True(t, ok)
	require.Equal(t, 8, source)
	require.Zero(t, local)
	require.True(t, strings.HasPrefix(strings.Split(view, "\n")[firstRow], " "), "expanded child output has a left gutter")
	require.Equal(t, model.width, ansi.StringWidth(strings.Split(view, "\n")[firstRow]), "expanded child rows fill both gutters without clipping")
	source, local, ok = model.SourceAt(lastRow)
	require.True(t, ok, "the final source line remains in the child hit range")
	require.Equal(t, 8, source)
	require.Equal(t, 1, local)
	firstSection := model.Rows()[0]
	secondSection := model.Rows()[1]
	require.Equal(t, 2, firstSection.Start)
	require.Equal(t, firstSection.End+1, secondSection.Start, "one blank row separates source sections")
	model.StopAnimation()
	ar.Stop()
}

func TestChildToggleUsesSourceOwnedColumnAndDoesNotHitMargins(t *testing.T) {
	child := &fakeView{text: "toggle child"}
	model := New(animation.NewRuntime())
	model.SetSize(40, 10)
	model.SetItems([]Item{{SourceIndex: 7, View: child}})
	model.SetExpandedImmediately(true)
	rows := model.Rows()
	require.Len(t, rows, 1)
	line := rows[0].Start
	require.False(t, model.IsToggleAt(line, 0), "left margin does not fall through to child toggle")
	require.False(t, model.IsToggleAt(line, 1), "second left margin cell stays inert")
	require.True(t, model.IsToggleAt(line, 2), "child column zero moves inside the composite")
	model.ToggleAt(line, 2)
	require.Equal(t, []int{0}, child.toggleCols)
	require.Equal(t, 1, child.toggleCount)
	require.False(t, model.IsToggleAt(line, 39), "right margin does not fall through to child toggle")
	model.StopAnimation()
}

func TestFoldCandidateIsBoundedInertAndUsesCanonicalDuration(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	model := New(ar)
	model.SetSize(40, 20)
	model.FoldCandidate(12, "first response line\nsecond response line\nthird response line\nfourth response line")
	initial := ansi.Strip(model.View())
	require.Contains(t, initial, "first response line")
	require.Contains(t, initial, "third response line")
	require.NotContains(t, initial, "fourth response line")
	require.Equal(t, 3, model.foldRows)
	_, _, ok := model.SourceAt(1)
	require.False(t, ok, "excerpt lines are inert rather than mapped to hidden source content")
	require.Equal(t, animation.ShortDuration, model.fold.Duration())
	advance(t, ar, model, clock, animation.ShortDuration)
	require.NotContains(t, ansi.Strip(model.View()), "first response line")
	model.StopAnimation()
	require.Zero(t, ar.ActiveCount())
}

func TestWidthChangeSettlesDisclosureAndBoundsHeader(t *testing.T) {
	model := New(animation.NewRuntime())
	model.SetItems([]Item{{SourceIndex: 0, View: &fakeView{text: strings.Repeat("body ", 10)}}})
	model.SetExpanded(true)
	model.SetSize(18, 10)
	require.LessOrEqual(t, ansi.StringWidth(ansi.Strip(strings.Split(model.View(), "\n")[0])), 18)
	require.False(t, model.disclosure.NeedsTick())
	model.StopAnimation()
}

func TestInvalidateSourceAndSetItemsReuseUnchangedRenderedChildren(t *testing.T) {
	ar := animation.NewRuntime()
	first := &fakeView{text: "first child"}
	second := &fakeView{text: "second child"}
	model := New(ar)
	model.SetSize(80, 20)
	model.SetItems([]Item{{SourceIndex: 1, View: first}, {SourceIndex: 2, View: second}})
	model.SetExpanded(true)
	model.SetSize(81, 20) // resize settles the disclosure to its expanded target
	require.Contains(t, model.View(), "first child")
	require.Equal(t, 1, first.views)
	require.Equal(t, 1, second.views)

	first.text = "first updated"
	model.InvalidateSource(1)
	require.Contains(t, model.View(), "first updated")
	require.Equal(t, 2, first.views)
	require.Equal(t, 1, second.views, "targeted invalidation preserves unrelated child render cache")

	model.SetItems([]Item{{SourceIndex: 2, View: second}, {SourceIndex: 1, View: first}})
	require.Contains(t, model.View(), "first updated")
	require.Equal(t, 2, first.views, "reordered unchanged pointer reuses its output")
	require.Equal(t, 1, second.views, "same-pointer output remains cached")

	replacement := &fakeView{text: "replacement child"}
	model.SetItems([]Item{{SourceIndex: 2, View: replacement}, {SourceIndex: 1, View: first}})
	require.Contains(t, model.View(), "replacement child")
	require.Equal(t, 1, replacement.views, "replaced pointer renders once")
	require.Equal(t, 2, first.views, "unchanged sibling remains cached after pointer replacement")
	model.StopAnimation()
	ar.Stop()
}

func TestSetItemsGrowingLongLoopListRemainsCollapsedAndLazy(t *testing.T) {
	ar := animation.NewRuntime()
	model := New(ar)
	items := make([]Item, 0, 1001)
	children := make([]*fakeView, 1000)
	for i := range children {
		children[i] = &fakeView{text: "hidden source child"}
		items = append(items, Item{SourceIndex: i, View: children[i]})
	}
	model.SetSize(100, 30)
	model.SetItems(items)
	require.Equal(t, 1, model.Height())
	require.Empty(t, model.Rows())
	for _, child := range children {
		require.Zero(t, child.views, "collapsed work list must not render hidden chronology")
	}
	items = append(items, Item{SourceIndex: 1000, View: &fakeView{text: "new"}})
	model.SetItems(items)
	require.Equal(t, 1, model.Height())
	require.Empty(t, model.Rows())
	for _, child := range children {
		require.Zero(t, child.views)
	}
	model.StopAnimation()
	ar.Stop()
}

func BenchmarkCollapsedThousandChildrenHeaderFoldAndExpandedCachedHover(b *testing.B) {
	ar := animation.NewRuntime()
	defer ar.Stop()
	model := New(ar)
	model.SetSize(100, 30)
	items := make([]Item, 1000)
	children := make([]*fakeView, len(items))
	for i := range items {
		children[i] = &fakeView{text: "hidden child"}
		items[i] = Item{SourceIndex: i, View: children[i]}
	}
	model.SetItems(items)
	model.SetStatus(Status{Active: true, ToolCount: 1000})
	b.Run("collapsed-header-and-fold", func(b *testing.B) {
		b.ReportAllocs()
		for i := range b.N {
			model.SetStatus(Status{Active: true, ToolCount: i})
			model.FoldCandidate(1000, "candidate line one\ncandidate line two\ncandidate line three")
			model.View()
			model.StopAnimation()
		}
		for _, child := range children {
			if child.views != 0 {
				b.Fatalf("collapsed child render count=%d", child.views)
			}
		}
	})
	b.Run("expanded-cached-hover", func(b *testing.B) {
		model.SetItems(items[:4])
		model.SetExpanded(true)
		model.SetSize(101, 30)
		model.View()
		b.ResetTimer()
		b.ReportAllocs()
		for range b.N {
			model.SetHoveredAt(1, true)
		}
		for _, child := range children[:4] {
			if child.views != 1 {
				b.Fatalf("expanded child render count=%d", child.views)
			}
		}
	})
}

func TestStopAnimationClearsCancelledFoldPreview(t *testing.T) {
	ar := animation.NewRuntime()
	model := New(ar)
	model.FoldCandidate(3, "candidate being folded")
	require.Contains(t, ansi.Strip(model.View()), "candidate being folded")
	model.StopAnimation()
	require.NotContains(t, ansi.Strip(model.View()), "candidate being folded")
	require.False(t, model.NeedsTick())
	ar.Stop()
}

func TestWrappedReasoningInsideExpandedWorkHasNoDisclosure(t *testing.T) {
	for _, width := range []int{80, 12, 8, 4, 3} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			ar := animation.NewRuntime()
			child := reasoningblock.New(ar, "wrapped", "root", &service.SessionState{})
			child.SetReasoning(strings.Repeat("- reasoning content\n", 12))
			block := New(ar)
			t.Cleanup(func() { block.StopAnimation(); child.StopAnimation(); ar.Stop() })
			block.SetSize(width, 0)
			block.SetItems([]Item{{SourceIndex: 7, View: child}})
			block.SetExpandedImmediately(true)
			rows := block.Rows()
			require.Len(t, rows, 1)
			lines := strings.Split(ansi.Strip(block.View()), "\n")
			for line := rows[0].Start; line < rows[0].End; line++ {
				require.NotContains(t, lines[line], "[", "hosted reasoning shows no nested disclosure at width %d", width)
				for col := range width {
					require.False(t, block.IsToggleAt(line, col), "hosted reasoning content at (%d,%d) does not toggle", line, col)
				}
			}
			// Narrow frames leave no room for reasoning text below width 8, so only wider blocks must show every wrapped line.
			if width >= 8 {
				content := 0
				for line := rows[0].Start; line < rows[0].End; line++ {
					if strings.TrimSpace(lines[line]) != "" {
						content++
					}
				}
				require.GreaterOrEqual(t, content, 12, "every wrapped reasoning line stays visible at width %d", width)
			}
		})
	}
}
