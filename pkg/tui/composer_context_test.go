package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation/animationtest"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type contextHarness struct {
	t         *testing.T
	root      *appModel
	scheduler *animationtest.Clock
}

func newContextHarness(t *testing.T) *contextHarness {
	t.Helper()
	scheduler, ar := animationtest.NewClock(time.Unix(1, 0))
	root, _, _ := harnessRoot(t, 96, 28, ar)
	return &contextHarness{t: t, root: root, scheduler: scheduler}
}

func (h *contextHarness) warmTab(title string) string {
	sess := session.New(session.WithTitle(title), session.WithAgentName("root"))
	application := app.New(h.t.Context(), stubRuntime{}, sess)
	id := h.root.supervisor.AddSession(h.t.Context(), application, sess, "", nil)
	h.root.Update(messages.TabsUpdatedMsg{})
	return id
}

func (h *contextHarness) coldTab(input, output int64) (tabID, savedID string) {
	store := session.NewInMemorySessionStore()
	saved := session.New(session.WithAgentName("root"), session.WithTitle("Saved"))
	saved.AddMessage(session.UserMessage("restored conversation"))
	saved.SetUsage(input, output)
	require.NoError(h.t, store.AddSession(h.t.Context(), saved))
	placeholder := session.New(session.WithAgentName("root"))
	application := app.New(h.t.Context(), storeRuntime{store: store}, placeholder)
	id := h.root.supervisor.AddSession(h.t.Context(), application, placeholder, "", nil)
	h.root.tabs[id] = &tabModel{
		sessionState:   service.NewSessionState(placeholder),
		pendingRestore: &saved.ID,
	}
	h.root.Update(messages.TabsUpdatedMsg{})
	return id, saved.ID
}

func (h *contextHarness) route(tabID string, event tea.Msg) {
	h.root.Update(messages.RoutedMsg{SessionID: tabID, Inner: event})
}

func (h *contextHarness) usage(tabID, sessionID string, used, limit int64) {
	h.route(tabID, runtime.NewTokenUsageEvent(sessionID, "root", &runtime.Usage{ContextLength: used, ContextLimit: limit}))
}

func (h *contextHarness) switchTo(tabID string) {
	h.root.Update(messages.SwitchTabMsg{SessionID: tabID})
	h.root.Update(messages.TabsUpdatedMsg{})
	require.Equal(h.t, tabID, h.root.supervisor.ActiveID())
}

func (h *contextHarness) frame() string {
	return strings.Split(h.root.View().Content, "\n")[h.root.composerDock().stripTop]
}

func (h *contextHarness) fill() string {
	track, _, _ := strings.Cut(lipgloss.NewStyle().Background(styles.EditorBg).Foreground(styles.Background).Render("▄"), "▄")
	line := h.frame()
	if before, _, found := strings.Cut(line, track); found {
		return before
	}
	return line[:strings.LastIndex(line, "▄")+len("▄")]
}

func (h *contextHarness) filled() int { return strings.Count(h.fill(), "▄") }

func (h *contextHarness) label() string {
	_, reading, found := strings.Cut(ansi.Strip(h.frame()), "Context ")
	require.True(h.t, found)
	return strings.TrimSpace(reading)
}

func (h *contextHarness) step() { stepClock(h.t, h.scheduler, h.root) }

func (h *contextHarness) settle() {
	for range 60 {
		if !h.root.ar.HasActive() {
			break
		}
		h.step()
	}
	require.Zero(h.t, h.root.ar.ActiveCount())
}

func (h *contextHarness) requirePending(heldCells int) {
	h.t.Helper()
	require.Equal(h.t, "—", h.label())
	require.Equal(h.t, heldCells, h.filled(), "pending keeps the last visible fill")
	require.Zero(h.t, h.root.ar.ActiveCount())
}

func (h *contextHarness) requireReadouts(sidebar, composer string) {
	h.t.Helper()
	require.Contains(h.t, ansi.Strip(h.root.View().Content), "("+sidebar+")")
	require.Equal(h.t, composer, h.label())
}

func TestComposerContextRetargetsAcrossTabs(t *testing.T) {
	h := newContextHarness(t)
	second := h.warmTab("Second")
	unknown := h.warmTab("Unknown")
	zero := h.warmTab("Zero")

	h.usage("profile", "profile", 60, 100)
	h.usage(second, second, 35, 100)
	h.usage(zero, zero, 0, 100)
	h.settle()
	require.Equal(t, "60%", h.label())

	// Switch to second tab: first frame keeps outgoing fill, then eases down.
	start, from := h.fill(), h.filled()
	h.switchTo(second)
	require.Equal(t, start, h.fill())
	require.Equal(t, "60%", h.label())
	h.step()
	down, downLabel := h.filled(), h.label()
	require.Less(t, down, from)
	require.NotEqual(t, "60%", downLabel)
	require.NotEqual(t, "35%", downLabel)

	// Reversing mid-flight starts from the current fill and label.
	middle := h.fill()
	h.switchTo("profile")
	require.Equal(t, middle, h.fill())
	require.Equal(t, downLabel, h.label())
	h.step()
	require.Greater(t, h.filled(), down)
	h.switchTo(second)
	h.settle()
	require.Equal(t, "35%", h.label())

	// Equal percentages do not replay the animation.
	h.usage("profile", "profile", 35, 100)
	settled := h.frame()
	h.switchTo("profile")
	require.Equal(t, settled, h.frame())
	require.Zero(t, h.root.ar.ActiveCount())

	// Empty/unknown tab eases to empty track; zero tab shows 0%.
	h.switchTo(unknown)
	require.Equal(t, "—", h.label())
	h.settle()
	require.Zero(t, h.filled())

	h.switchTo(zero)
	require.Equal(t, "0%", h.label())
}

func TestComposerContextColdRestoreHoldsFillUntilOwnUsage(t *testing.T) {
	h := newContextHarness(t)
	h.usage("profile", "profile", 60, 100)
	h.settle()
	held, heldCells := h.fill(), h.filled()

	coldID, savedID := h.coldTab(20, 10)
	h.switchTo(coldID)
	require.Equal(t, savedID, h.root.application.Session().ID)
	h.requirePending(heldCells)
	pending := h.frame()

	// Unrelated events and stale placeholder usage keep the bar pending.
	h.route(coldID, runtime.AgentInfo("root", "model", "", ""))
	h.usage(coldID, "placeholder-session", 90, 100)
	require.Equal(t, pending, h.frame())

	// Restored session's own usage eases from the held fill.
	h.usage(coldID, savedID, 30, 100)
	require.Equal(t, held, h.fill())
	require.Equal(t, "60%", h.label())
	h.step()
	require.Less(t, h.filled(), heldCells)
	h.settle()
	require.Equal(t, "30%", h.label())

	// Usage without a context window eases to empty track.
	h.usage(coldID, savedID, 30, 0)
	require.Equal(t, "—", h.label())
	h.settle()
	require.Zero(t, h.filled())
}

type contextRestoreRuntime struct{ storeRuntime }

func (contextRestoreRuntime) EmitStartupInfo(_ context.Context, sess *session.Session, sink runtime.EventSink) {
	input, output := sess.Usage()
	if input+output == 0 {
		return
	}
	sink.Emit(runtime.AgentInfo("root", "test/model", "", "", 100))
	sink.Emit(runtime.NewTokenUsageEvent(sess.ID, "root", runtime.SessionUsage(sess, 100)))
}

func TestComposerContextAgreesOnPersistedStartupRestore(t *testing.T) {
	setupSettingsConfigTest(t)
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(true)}
		return nil
	}))
	ctx := t.Context()
	sessions := session.NewInMemorySessionStore()
	saved := session.New(session.WithAgentName("root"), session.WithTitle("Saved"))
	saved.AddMessage(session.UserMessage("synthetic restore conversation"))
	saved.SetUsage(20, 10)
	require.NoError(t, sessions.AddSession(ctx, saved))
	tabs, err := tuistate.New(ctx)
	require.NoError(t, err)
	require.NoError(t, tabs.AddTab(ctx, saved.ID, ""))
	require.NoError(t, tabs.SetActiveTab(ctx, saved.ID))
	require.NoError(t, tabs.ToggleSidebarCollapsed(ctx, saved.ID))
	require.NoError(t, tabs.Close())

	placeholder := session.New(session.WithAgentName("root"))
	a := app.New(ctx, contextRestoreRuntime{storeRuntime{store: sessions}}, placeholder)
	scheduler, ar := animationtest.NewClock(time.Unix(1, 0))
	root := New(ctx, nil, a, "", func() {}, func(m *appModel) {
		m.ar = ar
		m.tabBar = tabbar.New(m.ar, userconfig.Get().GetTabTitleMaxLength())
	}).(*appModel)
	t.Cleanup(root.Shutdown)
	startup := make(chan tea.Msg, 2)
	ready := make(chan struct{})
	go a.SubscribeReliable(ctx, func(msg tea.Msg) { startup <- msg }, app.WithSubscriptionReady(func() { close(ready) }))
	<-ready
	root.Init()
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	h := &contextHarness{t: t, root: root, scheduler: scheduler}
	require.Equal(t, "—", h.label())

	info := <-startup
	h.route(placeholder.ID, info)
	h.requireReadouts("30%", "30%")
	settled := h.filled()
	h.route(placeholder.ID, <-startup)
	require.Equal(t, settled, h.filled())
	require.Zero(t, root.ar.ActiveCount())

	h.usage(placeholder.ID, saved.ID, 25, 200)
	h.requireReadouts("12%", "30%")
	h.settle()
	h.requireReadouts("12%", "12%")
}

func TestComposerContextAgreesOnSessionAndWindowChanges(t *testing.T) {
	h := newContextHarness(t)
	h.root.hideSidebar = false
	h.root.activeTab.chatPage = chat.New(h.root.ar, t.Context(), h.root.application, h.root.activeTab.sessionState)
	h.root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	const root, child, detached = "profile", "child", "detached"
	h.route(root, runtime.StreamStarted(root, "root"))
	stepContext := func() {
		for start := h.root.ar.Now(); h.root.ar.Now()-start < 700*time.Millisecond; {
			h.step()
		}
	}
	h.route(root, runtime.NewTokenUsageEvent(root, "root", &runtime.Usage{InputTokens: 20, OutputTokens: 10, ContextLength: 30, ContextLimit: 100}))
	stepContext()
	h.requireReadouts("30%", "30%")
	// Partial accounting-only updates must retain the context target.
	h.route(root, runtime.NewTokenUsageEvent(root, "root", &runtime.Usage{InputTokens: 900}))
	h.requireReadouts("30%", "30%")
	h.route(root, runtime.AgentInfo("root", "test/model", "", "", 200))
	stepContext()
	h.requireReadouts("15%", "15%")

	// Subagent sessions don't alias the root session's readings.
	h.route(root, runtime.StreamStarted(child, "root"))
	require.Equal(t, "—", h.label())
	h.usage(root, child, 50, 100)
	stepContext()
	h.requireReadouts("50%", "50%")
	h.usage(root, detached, 98, 100)
	h.requireReadouts("50%", "50%")
	h.route(root, runtime.StreamStopped(child, "root", "normal"))
	stepContext()
	h.requireReadouts("15%", "15%")
	h.route(root, runtime.StreamStopped(root, "root", "normal"))
	h.usage(root, root, 130, 100)
	stepContext()
	h.requireReadouts("130%", "100%")
}
