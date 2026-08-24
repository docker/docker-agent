package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

func TestCommitWelcomePadsBanner(t *testing.T) {
	t.Parallel()
	m := &model{screen: ui.NewScreen("", "", "")}
	m.commitWelcome()

	require.Equal(t, 1, m.screen.Transcript.BlockCount())
	lines := m.screen.Transcript.BlockLines(0, 80)
	require.GreaterOrEqual(t, len(lines), bannerTopPadding+len(bannerLines))

	for i := range bannerTopPadding {
		assert.Empty(t, ansi.Strip(lines[i]))
	}

	leftPad := strings.Repeat(" ", bannerLeftPadding)
	firstBannerLine := ansi.Strip(lines[bannerTopPadding])
	assert.True(t, strings.HasPrefix(firstBannerLine, leftPad))
	assert.Equal(t, leftPad+bannerLines[0], firstBannerLine)

	helpLine := ansi.Strip(lines[len(lines)-1])
	assert.True(t, strings.HasPrefix(helpLine, leftPad))
}

func TestCommitWelcomeSkipsHiddenBanner(t *testing.T) {
	t.Parallel()
	m := &model{screen: ui.NewScreen("", "", ""), hideBanner: true}
	m.commitWelcome()

	require.Equal(t, 1, m.screen.Transcript.BlockCount())
	lines := m.screen.Transcript.BlockLines(0, 80)
	for _, line := range lines {
		assert.NotContains(t, ansi.Strip(line), bannerLines[0])
	}

	helpLine := ansi.Strip(lines[len(lines)-1])
	assert.True(t, strings.HasPrefix(helpLine, strings.Repeat(" ", bannerLeftPadding)),
		"the help hint survives a hidden banner")
}

func TestCommitWelcomeUsesConfiguredBanner(t *testing.T) {
	t.Parallel()
	custom := []string{"custom banner line one", "custom banner line two"}
	m := &model{screen: ui.NewScreen("", "", ""), banner: custom}
	m.commitWelcome()

	require.Equal(t, 1, m.screen.Transcript.BlockCount())
	lines := m.screen.Transcript.BlockLines(0, 80)
	require.GreaterOrEqual(t, len(lines), bannerTopPadding+len(custom))

	leftPad := strings.Repeat(" ", bannerLeftPadding)
	for i, want := range custom {
		assert.Equal(t, leftPad+want, ansi.Strip(lines[bannerTopPadding+i]))
	}
	// The configured banner replaces the built-in art, it does not append to it.
	assert.NotEqual(t, leftPad+bannerLines[0], ansi.Strip(lines[bannerTopPadding]))
}
