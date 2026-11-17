package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/markdowntransform"
	"github.com/docker/docker-agent/pkg/tui/streamcontent"
)

func transformingTranscript(t *testing.T) (*Transcript, *markdowntransform.Display, chan markdowntransform.Input) {
	t.Helper()
	inputs := make(chan markdowntransform.Input, 20)
	display := markdowntransform.NewDisplay(func(_ context.Context, in markdowntransform.Input) (string, error) {
		inputs <- in
		return strings.ReplaceAll(in.Markdown, "[[Done]]", "\x1b[32mDone\x1b[0m"), nil
	})
	display.Start(t.Context())
	transcript := NewTranscript()
	transcript.SetMarkdownDisplay(display)
	return transcript, display, inputs
}

func awaitTransform(t *testing.T, display *markdowntransform.Display, render func()) {
	t.Helper()
	changed := display.Changed()
	render()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("transform did not complete")
	}
}

func TestTranscriptMarkdownDisplay(t *testing.T) {
	t.Parallel()
	transcript, display, inputs := transformingTranscript(t)
	identity := streamcontent.Identity{SessionID: "s", MessageID: "m"}
	transcript.AppendAssistantContent(identity, "[[Done]]")

	awaitTransform(t, display, func() { transcript.pendingLines(40) })
	assert.True(t, (<-inputs).IsStreaming)
	require.True(t, transcript.RefreshMarkdown())
	assert.Contains(t, strings.Join(transcript.pendingLines(40), "\n"), "\x1b[32mDone")

	transcript.FlushPending()
	assert.Contains(t, ansi.Strip(strings.Join(transcript.BlockLines(0, 40), "\n")), "Done",
		"a flushed segment keeps its result")
	assert.Empty(t, inputs, "flushing at a tool boundary does not complete the message")

	awaitTransform(t, display, func() {
		transcript.CompleteMessage(identity)
		transcript.BlockLines(0, 40)
	})
	assert.False(t, (<-inputs).IsStreaming)
	assert.Equal(t, "[[Done]]", transcript.blocks[0].text)

	transcript.SetFullFilePreview(true)
	transcript.BlockLines(0, 40)
	assert.Empty(t, inputs, "unrelated invalidation reuses transformed results")
}

func TestTranscriptCompleteStreamCompletesLegacySegments(t *testing.T) {
	t.Parallel()
	transcript, _, _ := transformingTranscript(t)
	transcript.AppendAssistant("one")
	transcript.FlushPending()
	transcript.AppendAssistant("two")
	transcript.CompleteStream()
	for _, b := range transcript.blocks {
		assert.False(t, b.markdown.streaming)
	}
}

func TestTranscriptSettleMarkdownShowsSourceForPendingSnapshot(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	display := markdowntransform.NewDisplay(func(_ context.Context, in markdowntransform.Input) (string, error) {
		<-release
		return "T:" + in.Markdown, nil
	})
	display.Start(t.Context())
	t.Cleanup(func() { close(release) })
	transcript := NewTranscript()
	transcript.SetMarkdownDisplay(display)
	identity := streamcontent.Identity{SessionID: "s", MessageID: "m"}

	transcript.AppendAssistantContent(identity, "partial")
	awaitTransform(t, display, func() {
		transcript.pendingLines(40)
		release <- struct{}{}
	})
	transcript.AppendAssistantContent(identity, " and the rest")
	transcript.CompleteStream()
	require.True(t, transcript.RefreshMarkdown())
	assert.Contains(t, ansi.Strip(strings.Join(transcript.BlockLines(0, 40), "\n")), "T:partial",
		"live frames keep the previous result while the latest snapshot is pending")

	transcript.SettleMarkdown()
	final := ansi.Strip(strings.Join(transcript.BlockLines(0, 40), "\n"))
	assert.Contains(t, final, "partial and the rest", "the final frame never shows a truncated result")
	assert.NotContains(t, final, "T:")
}

func TestRendererKeepsScrollbackWhenArchivedRowsChange(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(2)
	r.Frame([]string{"one", "two", "three", "four"}, 3, 0)
	buf.Reset()
	r.Frame([]string{"\x1b[32mone\x1b[0m", "two", "three", "four"}, 3, 0)
	assert.NotContains(t, buf.String(), "\x1b[3J", "terminal history is never erased")
}

func TestTranscriptSettleMarkdownPicksUpUnrefreshedResult(t *testing.T) {
	t.Parallel()
	display := markdowntransform.NewDisplay(func(_ context.Context, in markdowntransform.Input) (string, error) {
		return "T:" + in.Markdown, nil
	})
	display.Start(t.Context())
	transcript := NewTranscript()
	transcript.SetMarkdownDisplay(display)
	identity := streamcontent.Identity{SessionID: "s", MessageID: "m"}

	transcript.AppendAssistantContent(identity, "partial")
	awaitTransform(t, display, func() { transcript.pendingLines(40) })
	transcript.AppendAssistantContent(identity, " and the rest")
	transcript.CompleteStream()
	require.True(t, transcript.RefreshMarkdown())
	awaitTransform(t, display, func() { transcript.BlockLines(0, 40) })

	transcript.SettleMarkdown()
	assert.Contains(t, ansi.Strip(strings.Join(transcript.BlockLines(0, 40), "\n")), "T:partial and the rest",
		"a result that landed before the batched refresh is shown")
}
