package message

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/markdowntransform"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func recordingDisplay(t *testing.T, transform func(string) string) (*markdowntransform.Display, chan markdowntransform.Input) {
	t.Helper()
	inputs := make(chan markdowntransform.Input, 20)
	display := markdowntransform.NewDisplay(func(_ context.Context, in markdowntransform.Input) (string, error) {
		inputs <- in
		return transform(in.Markdown), nil
	})
	display.Start(t.Context())
	return display, inputs
}

func renderTransformed(t *testing.T, display *markdowntransform.Display, mv *messageModel) string {
	t.Helper()
	changed := display.Changed()
	mv.Render(60)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("transform did not complete")
	}
	require.True(t, mv.RefreshMarkdown())
	return mv.Render(60)
}

func TestMarkdownDisplayIsDisplayOnly(t *testing.T) {
	t.Parallel()
	display, inputs := recordingDisplay(t, func(md string) string {
		return strings.ReplaceAll(md, "[[Done]]", "\x1b[32mDone\x1b[0m")
	})
	msg := types.Agent(types.MessageTypeAssistant, "root", "[[Done]]")
	mv := New(animation.NewRuntime(), msg, nil, WithMarkdownDisplay(display))

	rendered := renderTransformed(t, display, mv)
	assert.Contains(t, rendered, "\x1b[32mDone")
	assert.NotContains(t, ansi.Strip(rendered), "[[Done]]")
	assert.Equal(t, "[[Done]]", msg.Content)
	assert.False(t, (<-inputs).IsStreaming)

	for i := range 10 {
		mv.SetHovered(i%2 == 0)
		mv.Render(60)
	}
	assert.False(t, mv.RefreshMarkdown())
	assert.Empty(t, inputs, "hover renders reuse the transformed result")
}

func TestMarkdownDisplayStreamingState(t *testing.T) {
	t.Parallel()
	display, inputs := recordingDisplay(t, func(md string) string { return md })
	msg := types.Agent(types.MessageTypeAssistant, "root", "partial")
	mv := New(animation.NewRuntime(), msg, nil, WithMarkdownDisplay(display))
	mv.SetStreaming(true)
	renderTransformed(t, display, mv)
	assert.True(t, (<-inputs).IsStreaming)

	mv.SetMessage(msg)
	assert.True(t, mv.Streaming(), "reconciling content mid-stream keeps the reply live")
	mv.Finalize()
	assert.True(t, mv.Streaming(), "releasing render caches is not completion")
	mv.SetStreaming(false)
	changed := display.Changed()
	mv.Render(60)
	<-changed
	assert.False(t, (<-inputs).IsStreaming)
}

func TestMarkdownDisplayDisablesCodeBlockCopy(t *testing.T) {
	t.Parallel()
	display, _ := recordingDisplay(t, func(string) string { return "```go\nreplacement\n```" })
	msg := types.Agent(types.MessageTypeAssistant, "root", "```go\noriginal\n```")
	mv := New(animation.NewRuntime(), msg, nil, WithMarkdownDisplay(display))

	rendered := renderTransformed(t, display, mv)
	assert.Contains(t, ansi.Strip(rendered), "replacement")
	assert.NotContains(t, rendered, "⎘ copy")
	assert.Empty(t, mv.CodeBlocks())
	segments, segmented := mv.RenderedSegments(60)
	require.True(t, segmented, "transformed replies keep incremental streaming segments")
	assert.NotContains(t, strings.Join(segments.Tail, "\n"), "⎘ copy")
	assert.Empty(t, mv.CodeBlocks())
	assert.Contains(t, msg.Content, "original")
}
