package sidebar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestDefaultLayoutConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultLayoutConfig()

	assert.Equal(t, 1, cfg.PaddingLeft, "default PaddingLeft should be 1")
	assert.Equal(t, 0, cfg.PaddingRight, "default PaddingRight should be 0")
	assert.Equal(t, 1, cfg.ScrollbarGap, "default ScrollbarGap should be 1")
}

func TestScrollbarWidthConstant(t *testing.T) {
	t.Parallel()

	// Verify scrollbar.Width is the expected value
	assert.Equal(t, 1, scrollbar.Width, "scrollbar.Width constant should be 1")
}

func TestLayoutConfigCompute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		config           LayoutConfig
		outerWidth       int
		scrollbarVisible bool
		wantContentWidth int
	}{
		{
			name:             "default config without scrollbar",
			config:           DefaultLayoutConfig(),
			outerWidth:       40,
			scrollbarVisible: false,
			wantContentWidth: 39, // 40 - 1 (paddingLeft) - 0 (paddingRight)
		},
		{
			name:             "default config with scrollbar",
			config:           DefaultLayoutConfig(),
			outerWidth:       40,
			scrollbarVisible: true,
			wantContentWidth: 37, // 40 - 1 (paddingLeft) - 0 (paddingRight) - 1 (scrollbarWidth) - 1 (scrollbarGap)
		},
		{
			name: "custom config without scrollbar",
			config: LayoutConfig{
				PaddingLeft:  2,
				PaddingRight: 1,
				ScrollbarGap: 2,
			},
			outerWidth:       50,
			scrollbarVisible: false,
			wantContentWidth: 47, // 50 - 2 - 1
		},
		{
			name: "custom config with scrollbar",
			config: LayoutConfig{
				PaddingLeft:  2,
				PaddingRight: 1,
				ScrollbarGap: 2,
			},
			outerWidth:       50,
			scrollbarVisible: true,
			wantContentWidth: 44, // 50 - 2 - 1 - 1 (scrollbarWidth constant) - 2
		},
		{
			name:             "minimum content width",
			config:           DefaultLayoutConfig(),
			outerWidth:       2,
			scrollbarVisible: true,
			wantContentWidth: 1, // Should never go below 1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			metrics := tt.config.Compute(tt.outerWidth, tt.scrollbarVisible)

			assert.Equal(t, tt.outerWidth, metrics.OuterWidth)
			assert.Equal(t, tt.wantContentWidth, metrics.ContentWidth)
			assert.Equal(t, tt.scrollbarVisible, metrics.ScrollbarVisible)
		})
	}
}

func TestMetricsAccessors(t *testing.T) {
	t.Parallel()

	cfg := LayoutConfig{
		PaddingLeft:  3,
		PaddingRight: 2,
		ScrollbarGap: 2,
	}

	metrics := cfg.Compute(50, true)

	assert.Equal(t, 3, metrics.PaddingLeft())
	assert.Equal(t, 2, metrics.PaddingRight())
	assert.Equal(t, 2, metrics.ScrollbarGap())
}

func TestScrollbarGapInOutput(t *testing.T) {
	t.Parallel()

	// This test verifies that when a scrollbar is rendered,
	// there's a visible gap between the content and the scrollbar.
	cfg := DefaultLayoutConfig()
	require.Equal(t, 1, cfg.ScrollbarGap, "test requires ScrollbarGap of 1")

	// Simulate what verticalView does when scrollbar is visible
	contentWidth := cfg.Compute(40, true).ContentWidth
	gap := strings.Repeat(" ", cfg.ScrollbarGap)
	scrollbarView := "│" // Simplified scrollbar representation

	// Create a sample content line
	contentLine := strings.Repeat("x", contentWidth)

	// Join them as the sidebar does
	combined := contentLine + gap + scrollbarView

	// Verify the total width matches expected
	expectedWidth := contentWidth + cfg.ScrollbarGap + scrollbar.Width
	actualWidth := lipgloss.Width(combined)

	assert.Equal(t, expectedWidth, actualWidth,
		"combined line should have content + gap + scrollbar width")

	// Verify there's actually a space between content and scrollbar
	assert.Contains(t, combined, " │",
		"there should be a space (gap) before the scrollbar")
}

// BenchmarkSidebarVerticalView_Scroll benchmarks the verticalView() method during scrolling.
// This verifies that scrolling uses the render cache instead of re-rendering sections.
func BenchmarkSidebarVerticalView_Scroll(b *testing.B) {
	sessionState := &service.SessionState{}
	m := New(sessionState).(*model)
	m.SetSize(35, 50)
	m.SetMode(ModeVertical)

	// Add some agents to create content
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "agent1", Model: "gpt-4", Description: "First agent with a long description that might wrap"},
		{Name: "agent2", Model: "claude-3", Description: "Second agent with another description"},
		{Name: "agent3", Model: "gemini", Description: "Third agent"},
	})

	// Add some token usage
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "agent1"},
		Usage: &runtime.Usage{
			InputTokens:  10000,
			OutputTokens: 5000,
			Cost:         0.50,
		},
	})

	// Add some tools
	m.SetToolsetInfo(25, false)

	// Initial render to populate cache
	_ = m.verticalView()

	b.ResetTimer()
	b.ReportAllocs()

	for i := range b.N {
		// Simulate scrolling by updating scroll offset
		m.scrollview.SetScrollOffset(i % 10)
		_ = m.verticalView()
	}
}

// BenchmarkSidebarVerticalView_NoCache benchmarks verticalView() when cache is always dirty.
// This shows the cost of full re-rendering for comparison.
func BenchmarkSidebarVerticalView_NoCache(b *testing.B) {
	sessionState := &service.SessionState{}
	m := New(sessionState).(*model)
	m.SetSize(35, 50)
	m.SetMode(ModeVertical)

	// Add some agents to create content
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "agent1", Model: "gpt-4", Description: "First agent with a long description that might wrap"},
		{Name: "agent2", Model: "claude-3", Description: "Second agent with another description"},
		{Name: "agent3", Model: "gemini", Description: "Third agent"},
	})

	// Add some token usage
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "agent1"},
		Usage: &runtime.Usage{
			InputTokens:  10000,
			OutputTokens: 5000,
			Cost:         0.50,
		},
	})

	// Add some tools
	m.SetToolsetInfo(25, false)

	b.ResetTimer()
	b.ReportAllocs()

	for range b.N {
		// Invalidate cache before each render to force full re-render
		m.invalidateCache()
		_ = m.verticalView()
	}
}
