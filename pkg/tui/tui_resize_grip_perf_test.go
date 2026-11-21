package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type gripFrameCounts struct {
	frames, ticks, invalidations, rebuilds, previews, resizes int
}

func (c *gripFrameCounts) frame(root *appModel, msg tea.Msg) tea.Cmd {
	page := root.activeTab.chatPage
	beforeHeight, beforeWidth := root.contentHeight, page.GetSidebarSettings().PreferredWidth
	wasCached := root.viewCacheValid
	beforePreview := root.sidebarGuideCache.content
	_, cmd := root.Update(msg)
	needsView := !root.viewCacheValid
	// A temporary invalidation restored by Update's pointer fast path doesn't count.
	if wasCached && needsView {
		c.invalidations++
	}
	view := root.View()
	if view.Content != root.viewCache.Content && root.viewCacheValid && (needsView || beforePreview != root.sidebarGuideCache.content) {
		c.previews++
	}
	if needsView && root.viewCacheValid {
		c.rebuilds++
	}
	if beforeHeight != root.contentHeight || beforeWidth != page.GetSidebarSettings().PreferredWidth {
		c.resizes++
	}
	c.frames++
	return cmd
}

func (c *gripFrameCounts) animate(b *testing.B, root *appModel, cmd tea.Cmd) {
	b.Helper()
	started := root.ar.Now()
	for root.ar.HasActive() {
		if root.ar.Now()-started > animation.ShortDuration+animation.TickRate {
			b.Fatal("grip animation did not settle")
		}
		var next tea.Cmd
		var found bool
		for _, msg := range collectMsgs(cmd) {
			if tick, ok := msg.(animation.TickMsg); ok {
				c.ticks++
				next = c.frame(root, tick)
				found = true
			}
		}
		if !found {
			b.Fatal("active grip did not schedule a tick")
		}
		cmd = next
	}
}

func (c *gripFrameCounts) report(b *testing.B) {
	b.Helper()
	b.ReportMetric(float64(c.frames), "frames")
	b.ReportMetric(float64(c.ticks), "animation-ticks")
	b.ReportMetric(float64(c.invalidations), "root-invalidations")
	b.ReportMetric(float64(c.rebuilds), "root-rebuilds")
	b.ReportMetric(float64(c.resizes), "geometry-changes")
	b.ReportMetric(float64(c.previews), "preview-repaints")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(c.frames), "ns/frame")
}

func newGripBenchmarkRoot(b *testing.B, width, height int) *appModel {
	b.Helper()
	ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	root, _, _ := harnessRoot(b, width, height, ar)
	chat.Cleanup(root.activeTab.chatPage)
	page := chat.New(ar, b.Context(), root.application, root.activeTab.sessionState, chat.WithShowBanner(false))
	b.Cleanup(func() { chat.Cleanup(page) })
	root.activeTab.chatPage = page
	page.SetSidebarSettings(chat.SidebarSettings{PreferredWidth: 45})
	sess, _, _ := mixedHistorySession(50)
	root.application.Session().Messages = sess.Messages
	_ = page.Init()
	_, _ = root.Update(tea.WindowSizeMsg{Width: root.width, Height: root.height})
	_ = root.View()
	return root
}

// BenchmarkResizeGripFrames measures root Update + View without terminal writes,
// using real animation tick messages from a deterministic clock that never sleeps.
func BenchmarkResizeGripFrames(b *testing.B) {
	for _, grip := range []string{"Composer", "Sidebar"} {
		scenarios := []string{"HoverFade", "SettledHover", "HeldSteady", "DragResize"}
		if grip == "Sidebar" {
			scenarios = append(scenarios, "GuideExpand", "GuideSettle", "DropResize")
		}
		for _, scenario := range scenarios {
			b.Run(grip+"/"+scenario, func(b *testing.B) {
				benchmarkResizeGripScenario(b, grip, scenario, 160, 40, false)
			})
		}
	}
}

func BenchmarkSidebarResizeGuideStyledLarge(b *testing.B) {
	benchmarkResizeGripScenario(b, "Sidebar", "DragResize", 320, 100, true)
}

func benchmarkResizeGripScenario(b *testing.B, grip, scenario string, width, height int, styledLarge bool) {
	b.Helper()
	root := newGripBenchmarkRoot(b, width, height)
	rest := root.viewCache.Content
	x, y, glyph := root.width/2, root.contentHeight, strings.Repeat("─", resizeHandleWidth)
	if grip == "Sidebar" {
		line, _, _ := strings.Cut(ansi.Strip(rest), "\n")
		before, _, found := strings.Cut(line, "»")
		require.True(b, found, "right sidebar divider toggle")
		x, y, glyph = lipgloss.Width(before), root.contentHeight/2, "│"
	}
	hover := tea.MouseMotionMsg{X: x, Y: y}
	leave := tea.MouseMotionMsg{X: root.width/2 + 15, Y: root.height - 1}
	held := tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft}

	var warmup gripFrameCounts
	warmup.animate(b, root, warmup.frame(root, hover))
	require.Contains(b, root.viewCache.Content, styles.ResizeHandleHoverStyle.Render(glyph), "pointer hits the painted grip")
	switch scenario {
	case "HoverFade":
		warmup.animate(b, root, warmup.frame(root, leave))
		require.Equal(b, rest, root.viewCache.Content, "fade-out restores the original frame")
	case "HeldSteady", "DragResize":
		warmup.animate(b, root, warmup.frame(root, tea.MouseClickMsg(held)))
		require.True(b, root.isDragging || chat.IsResizingSidebar(root.activeTab.chatPage), "resize capture")
	}

	if styledLarge {
		// Keep the real capture and cached View path, but give every row the
		// styling density of highlighted text instead of mostly blank cells.
		const chunk = "\x1b[38;2;140;120;255mfunc\x1b[m render(\x1b]8;;https://example.test/path\x1b\\界👩‍💻\x1b]8;;\x1b\\) \x1b[1;33mstring\x1b[0m { return value }  "
		rows := strings.Split(root.viewCache.Content, "\n")
		row := ansi.Truncate(strings.Repeat(chunk, width/ansi.StringWidth(chunk)+1), width, "")
		for y := range root.contentHeight {
			rows[y] = row
		}
		root.viewCache.Content = strings.Join(rows, "\n")
		root.sidebarGuideCache.content = ""
	}

	steadyView := root.View().Content
	var counts gripFrameCounts
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		switch scenario {
		case "HoverFade":
			counts.animate(b, root, counts.frame(root, hover))
			counts.animate(b, root, counts.frame(root, leave))
		case "SettledHover":
			counts.frame(root, hover)
		case "HeldSteady":
			counts.frame(root, held)
		case "DragResize":
			drag := held
			if grip == "Composer" {
				drag.Y -= 1 + i%2
			} else {
				drag.X -= 3 + i%2
			}
			counts.frame(root, drag)
		case "GuideExpand":
			counts.animate(b, root, counts.frame(root, tea.MouseClickMsg(held)))
			b.StopTimer()
			warmup.animate(b, root, warmup.frame(root, tea.MouseReleaseMsg(held)))
			b.StartTimer()
		case "GuideSettle":
			b.StopTimer()
			warmup.animate(b, root, warmup.frame(root, tea.MouseClickMsg(held)))
			b.StartTimer()
			counts.animate(b, root, counts.frame(root, tea.MouseReleaseMsg(held)))
		case "DropResize":
			// Alternate releases farther from and closer to the sidebar edge.
			// Coordinates are outside chat and there is deliberately no final motion.
			start := held
			start.X -= 3 * (i % 2)
			queued := counts.frame(root, tea.MouseClickMsg(start))
			start.X -= 3 - 6*(i%2)
			start.Y = root.height
			cmd := counts.frame(root, tea.MouseReleaseMsg(start))
			b.StopTimer()
			warmup.animate(b, root, tea.Batch(queued, cmd))
			b.StartTimer()
		}
	}
	switch {
	case scenario == "DragResize" && grip == "Composer":
		require.Equal(b, counts.frames, counts.resizes, "every live drag frame must move the divider")
	case scenario == "DragResize":
		require.Equal(b, counts.frames, counts.previews, "every sidebar motion must move the guide")
		require.Zero(b, counts.resizes, "sidebar preview never resizes while held")
		require.Zero(b, counts.rebuilds, "sidebar preview reuses the live base frame")
	case scenario == "GuideExpand" || scenario == "GuideSettle":
		require.Positive(b, counts.ticks)
		require.Positive(b, counts.previews)
		require.Zero(b, counts.resizes, "holding and releasing at the origin keeps geometry")
		require.LessOrEqual(b, counts.rebuilds, counts.frames-counts.ticks, "guide-only ticks reuse the base frame")
		require.False(b, root.ar.HasActive(), "settled guide releases the shared clock")
		require.Equal(b, root.viewCache.Content, root.View().Content, "completed settling leaves no preview")
	case scenario == "DropResize":
		require.Equal(b, counts.frames/2, counts.resizes, "exactly one width change per drop")
		require.False(b, root.ar.HasActive(), "quick drops finish settling")
	case scenario == "HeldSteady" || scenario == "SettledHover":
		require.Zero(b, counts.rebuilds, "steady frames reuse the root cache")
		fallthrough
	default:
		require.Zero(b, counts.resizes, "no geometry change outside moving drag")
		require.Equal(b, steadyView, root.View().Content, "frame matches the initial settled state")
	}
	counts.report(b)
}
