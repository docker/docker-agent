package chat

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newLayoutTestPage builds a chatPage large enough for the vertical sidebar
// layout, with the given sidebar position applied.
func newLayoutTestPage(t *testing.T, position msgtypes.SidebarPosition) *chatPage {
	t.Helper()
	sessionState := &service.SessionState{}
	p := &chatPage{
		sidebar:           sidebar.New(animation.NewRuntime(), t.Context(), sessionState),
		messages:          messages.New(animation.NewRuntime(), sessionState),
		sessionState:      sessionState,
		width:             160,
		height:            40,
		showStartupBanner: true,
	}
	p.layoutSettings = msgtypes.LayoutSettings{SidebarPosition: position}
	return p
}

func TestNewChatPageShowsBannerBeforeAgentInfo(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	p := New(animation.NewRuntime(), t.Context(), nil, sessionState).(*chatPage)
	p.SetSize(160, 40)

	assert.True(t, p.showStartupBanner)
	assert.Contains(t, ansi.Strip(p.messagesView(p.computeSidebarLayout())), tuibanner.Lines[0])
}

func TestStartupBannerIsCenteredInEmptyChat(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.showStartupBanner = true
	p.SetSize(p.width, p.height)
	sl := p.computeSidebarLayout()

	lines := strings.Split(p.messagesView(sl), "\n")
	require.Len(t, lines, sl.chatHeight)

	firstBannerLine := -1
	for i, line := range lines {
		if strings.Contains(ansi.Strip(line), tuibanner.Lines[0]) {
			firstBannerLine = i
			break
		}
	}
	require.NotEqual(t, -1, firstBannerLine)
	assert.Equal(t, (sl.chatHeight-len(tuibanner.Lines))/2, firstBannerLine)
	assert.Equal(t, (sl.chatWidth-ansi.StringWidth(tuibanner.Lines[0]))/2,
		strings.Index(ansi.Strip(lines[firstBannerLine]), "█"))
}

func TestStartupBannerHiddenWhenChatIsTooSmall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		width  int
		height int
	}{
		{name: "too narrow", width: tuibanner.Width - 1, height: tuibanner.Height},
		{name: "too short", width: tuibanner.Width, height: tuibanner.Height - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newLayoutTestPage(t, msgtypes.SidebarRight)
			p.showStartupBanner = true

			assert.Empty(t, p.messagesView(sidebarLayout{
				chatWidth:  tt.width,
				chatHeight: tt.height,
			}))
		})
	}
}

func TestEmptyAgentInfoDoesNotReactivateStartupBanner(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.showStartupBanner = false
	handled, _ := p.handleRuntimeEvent(runtime.AgentInfo("root", "model", "", ""))

	require.True(t, handled)
	assert.False(t, p.showStartupBanner)
}

func TestAgentWelcomeMessageControlsStartupBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		welcomeMessage string
		wantBanner     bool
	}{
		{name: "empty welcome", wantBanner: true},
		{name: "configured welcome", welcomeMessage: "Welcome to the agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newLayoutTestPage(t, msgtypes.SidebarRight)
			event := runtime.AgentInfo("root", "model", "", tt.welcomeMessage)
			handled, _ := p.handleRuntimeEvent(event)

			require.True(t, handled)
			assert.Equal(t, tt.wantBanner, p.showStartupBanner)
			out := ansi.Strip(p.messagesView(p.computeSidebarLayout()))
			if tt.wantBanner {
				assert.Contains(t, out, tuibanner.Lines[0])
			} else {
				assert.Contains(t, out, tt.welcomeMessage)
				assert.NotContains(t, out, tuibanner.Lines[0])
			}
		})
	}
}

func TestComputeSidebarLayout_RightDefault(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	sl := p.computeSidebarLayout()

	require.Equal(t, sidebarVertical, sl.mode)
	assert.False(t, sl.sidebarOnLeft)
	assert.Equal(t, 0, sl.chatStartX, "chat starts at the left edge")
	assert.Equal(t, sl.chatWidth, sl.handleX, "handle sits right after the chat")
	assert.Equal(t, sl.chatWidth+toggleColumnWidth, sl.sidebarStartX)
	assert.True(t, sl.isInSidebar(sl.sidebarStartX+1))
	assert.False(t, sl.isInSidebar(2))
}

func TestComputeSidebarLayout_ZeroValueDefaultsToRight(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, "")
	sl := p.computeSidebarLayout()

	require.Equal(t, sidebarVertical, sl.mode)
	assert.False(t, sl.sidebarOnLeft)
}

func TestComputeSidebarLayout_Left(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarLeft)
	sl := p.computeSidebarLayout()

	require.Equal(t, sidebarVertical, sl.mode)
	assert.True(t, sl.sidebarOnLeft)
	assert.Equal(t, 0, sl.sidebarStartX, "sidebar starts at the left edge")
	assert.Equal(t, sl.sidebarWidth-toggleColumnWidth, sl.handleX)
	assert.Equal(t, sl.sidebarWidth, sl.chatStartX, "chat starts after the sidebar")
	assert.Equal(t, sl.innerWidth, sl.sidebarWidth+sl.chatWidth)

	assert.True(t, sl.isInSidebar(0))
	assert.True(t, sl.isInSidebar(sl.handleX-1))
	assert.False(t, sl.isInSidebar(sl.handleX), "the handle column is not sidebar content")
	assert.False(t, sl.isInSidebar(sl.chatStartX+5))
	assert.True(t, sl.isOnHandle(sl.handleX))
}

func TestComputeSidebarLayout_Top(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarTop)
	sl := p.computeSidebarLayout()

	require.Equal(t, sidebarCollapsedNarrow, sl.mode, "top position always uses the band layout")
	assert.False(t, sl.bandAtBottom)
	assert.Equal(t, 0, sl.bandY())
	assert.Equal(t, sl.innerWidth, sl.chatWidth)
	assert.Equal(t, p.height-sl.sidebarHeight, sl.chatHeight)
	assert.True(t, sl.isInBand(0))
	assert.False(t, sl.isInBand(sl.sidebarHeight))
	assert.Equal(t, 1, sl.bandContentY(1), "top band content starts at its first line")
}

func TestComputeSidebarLayout_Bottom(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarBottom)
	sl := p.computeSidebarLayout()

	require.Equal(t, sidebarCollapsedNarrow, sl.mode, "bottom position always uses the band layout")
	assert.True(t, sl.bandAtBottom)
	assert.Equal(t, sl.chatHeight, sl.bandY())
	assert.False(t, sl.isInBand(0), "chat rows are not in the band")
	assert.True(t, sl.isInBand(sl.chatHeight))
	assert.True(t, sl.isInBand(sl.chatHeight+sl.sidebarHeight-1))
	assert.False(t, sl.isInBand(sl.chatHeight+sl.sidebarHeight))
	assert.Equal(t, 0, sl.bandContentY(sl.chatHeight+1),
		"bottom band renders its divider first, so content starts one line lower")
}

func TestComputeSidebarLayout_NarrowWindowKeepsConfiguredBandEdge(t *testing.T) {
	t.Parallel()

	right := newLayoutTestPage(t, msgtypes.SidebarRight)
	right.width = 80
	sl := right.computeSidebarLayout()
	require.Equal(t, sidebarCollapsedNarrow, sl.mode)
	assert.False(t, sl.bandAtBottom, "narrow side-by-side layouts collapse to a top band")

	bottom := newLayoutTestPage(t, msgtypes.SidebarBottom)
	bottom.width = 80
	sl = bottom.computeSidebarLayout()
	require.Equal(t, sidebarCollapsedNarrow, sl.mode)
	assert.True(t, sl.bandAtBottom, "bottom position keeps its band at the bottom when narrow")
}

func TestComputeSidebarLayout_LeftCollapsesToTopBand(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarLeft)
	p.sidebar.SetCollapsed(true)
	sl := p.computeSidebarLayout()

	require.Equal(t, sidebarCollapsed, sl.mode)
	assert.False(t, sl.bandAtBottom)
	assert.True(t, sl.showToggle())
}

func TestSetLayoutSettingsForwardsVisibilityToSidebar(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetLayoutSettings(msgtypes.LayoutSettings{
		SidebarPosition: msgtypes.SidebarLeft,
		HideUsage:       true,
	})

	assert.Equal(t, msgtypes.SidebarLeft, p.layoutSettings.SidebarPosition)

	sl := p.computeSidebarLayout()
	assert.True(t, sl.sidebarOnLeft)
}

func TestSectionVisibilityMapsAllFields(t *testing.T) {
	t.Parallel()

	assert.Equal(t, sidebar.SectionVisibility{}, sectionVisibility(msgtypes.LayoutSettings{}),
		"the default layout hides nothing")

	assert.Equal(t, sidebar.SectionVisibility{
		HideSessionPath: true,
		HideUsage:       true,
		HideAgents:      true,
		HideTools:       true,
		HideTodos:       true,
	}, sectionVisibility(msgtypes.LayoutSettings{
		HideSessionPath: true,
		HideUsage:       true,
		HideAgents:      true,
		HideTools:       true,
		HideTodos:       true,
	}))
}

func TestAgentInfoModeMapsValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, sidebar.AgentInfoCompact, agentInfoMode(""), "the zero value maps to compact")
	assert.Equal(t, sidebar.AgentInfoCompact, agentInfoMode(msgtypes.InfoModeCompact))
	assert.Equal(t, sidebar.AgentInfoDetailed, agentInfoMode(msgtypes.InfoModeDetailed))
	assert.Equal(t, sidebar.AgentInfoCompact, agentInfoMode(msgtypes.SidebarInfoMode("bogus")),
		"unknown values map to compact")
}

// TestSetLayoutSettingsForwardsInfoModeToSidebar verifies the info mode
// reaches the sidebar's roster renderer and can be switched live: the
// detailed cards carry the labeled effort metric — "Eff high" in the compact
// vocabulary the cards adapt to at the default sidebar width — while the
// compact roster shows only the gauge badge, never the label.
func TestSetLayoutSettingsForwardsInfoModeToSidebar(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.sessionState.SetCurrentAgentName("root")
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4", Thinking: "high"},
	})
	p.SetSize(160, 40)

	require.NotContains(t, ansi.Strip(p.View()), "Eff high",
		"the default layout renders the compact roster")

	p.SetLayoutSettings(msgtypes.LayoutSettings{SidebarInfoMode: msgtypes.InfoModeDetailed})
	assert.Contains(t, ansi.Strip(p.View()), "Eff high",
		"detailed mode renders the labeled agent cards")

	p.SetLayoutSettings(msgtypes.LayoutSettings{})
	assert.NotContains(t, ansi.Strip(p.View()), "Eff high",
		"switching back live restores the compact roster")
}

// TestWithLayoutSettingsAppliesInfoMode verifies the initial page option
// forwards the persisted info mode to the sidebar.
func TestWithLayoutSettingsAppliesInfoMode(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	sessionState.SetCurrentAgentName("root")
	p := &chatPage{
		sidebar:      sidebar.New(animation.NewRuntime(), t.Context(), sessionState),
		messages:     messages.New(animation.NewRuntime(), sessionState),
		sessionState: sessionState,
	}
	WithLayoutSettings(msgtypes.LayoutSettings{SidebarInfoMode: msgtypes.InfoModeDetailed})(p)
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4", Thinking: "high"},
	})
	p.SetSize(160, 40)

	assert.Contains(t, ansi.Strip(p.View()), "Eff high",
		"the initial layout option applies the detailed mode")
}

// TestSetLayoutSettingsForwardsActiveAgentsOnlyToSidebar verifies the agent
// filter reaches the sidebar's roster and can be switched live: an agent
// without session participation disappears while the filter is on and
// returns when it is turned off.
func TestSetLayoutSettingsForwardsActiveAgentsOnlyToSidebar(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.sessionState.SetCurrentAgentName("root")
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4"},
		{Name: "idle", Provider: "openai", Model: "gpt-4o"},
	})
	p.SetSize(160, 40)

	require.Contains(t, ansi.Strip(p.View()), "idle",
		"the default layout renders the whole team")

	p.SetLayoutSettings(msgtypes.LayoutSettings{ActiveAgentsOnly: true})
	assert.NotContains(t, ansi.Strip(p.View()), "idle",
		"the filter hides agents without session participation")

	p.SetLayoutSettings(msgtypes.LayoutSettings{})
	assert.Contains(t, ansi.Strip(p.View()), "idle",
		"switching the filter off live restores the whole team")
}

func TestSetLayoutSettingsBeforeSizingReturnsNil(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	p := &chatPage{
		sidebar:      sidebar.New(animation.NewRuntime(), t.Context(), sessionState),
		messages:     messages.New(animation.NewRuntime(), sessionState),
		sessionState: sessionState,
	}

	cmd := p.SetLayoutSettings(msgtypes.LayoutSettings{SidebarPosition: msgtypes.SidebarTop})
	assert.Nil(t, cmd, "no relayout before the first WindowSizeMsg")
	assert.Equal(t, msgtypes.SidebarTop, p.layoutSettings.SidebarPosition)
}

// sidebarTitleLine returns the index of the first rendered line containing the
// sidebar's default session title, or -1 when absent.
func sidebarTitleLine(view string) int {
	for i, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(line, "New session") {
			return i
		}
	}
	return -1
}

func TestViewRendersEveryPosition(t *testing.T) {
	t.Parallel()

	for _, position := range []msgtypes.SidebarPosition{
		msgtypes.SidebarRight, msgtypes.SidebarLeft, msgtypes.SidebarTop, msgtypes.SidebarBottom,
	} {
		p := newLayoutTestPage(t, position)
		p.SetSize(160, 40)
		view := p.View()
		assert.Equal(t, 40, lipgloss.Height(view), "position %s", position)
		assert.GreaterOrEqual(t, sidebarTitleLine(view), 0, "position %s renders the sidebar", position)
	}
}

func TestViewPlacesBandAtConfiguredEdge(t *testing.T) {
	t.Parallel()

	top := newLayoutTestPage(t, msgtypes.SidebarTop)
	top.SetSize(160, 40)
	topIdx := sidebarTitleLine(top.View())
	require.GreaterOrEqual(t, topIdx, 0)
	assert.Less(t, topIdx, top.computeSidebarLayout().sidebarHeight, "top band renders above the chat")

	bottom := newLayoutTestPage(t, msgtypes.SidebarBottom)
	bottom.SetSize(160, 40)
	bottomIdx := sidebarTitleLine(bottom.View())
	require.GreaterOrEqual(t, bottomIdx, 0)
	assert.GreaterOrEqual(t, bottomIdx, bottom.computeSidebarLayout().chatHeight, "bottom band renders below the chat")
}
