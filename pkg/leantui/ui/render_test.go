package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRenderUserLinesMarksPromptStart(t *testing.T) {
	t.Parallel()

	lines := RenderUserLines("jump back here", 24)
	require.NotEmpty(t, lines)
	assert.True(t, strings.HasPrefix(lines[0], seqPromptStart))
	assert.True(t, strings.HasSuffix(lines[0], seqOutputStart))
	assert.Equal(t, 1, strings.Count(strings.Join(lines, ""), seqPromptStart))
	assert.Equal(t, 1, strings.Count(strings.Join(lines, ""), seqOutputStart))
	assert.Equal(t, 24, DisplayWidth(lines[0]))
}

func TestRenderUserLinesUsesDistinctFullWidthBackground(t *testing.T) {
	t.Parallel()

	const width = 24
	lines := RenderUserLines("make this visible", width)
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.Equal(t, width, DisplayWidth(line))
	}

	style := StUserBox(width)
	assert.Equal(t, styles.MobyBlue, style.GetBackground())
	assert.NotEqual(t, StToolBox(width).GetBackground(), style.GetBackground())
}

func TestRenderUserLinesWrapsInsideBackgroundPadding(t *testing.T) {
	t.Parallel()

	const width = 12
	lines := RenderUserLines("a user message that wraps", width)
	require.Greater(t, len(lines), 1)
	for _, line := range lines {
		assert.LessOrEqual(t, DisplayWidth(line), width)
	}
}
