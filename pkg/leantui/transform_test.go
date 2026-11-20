package leantui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/markdowntransform"
	"github.com/docker/docker-agent/pkg/runtime"
)

// transformInputs returns a check that the latest is_streaming value sent
// for markdown becomes want.
func transformInputs(t *testing.T, m *model) func(markdown string, want bool, msg string) {
	t.Helper()
	inputs := make(chan markdowntransform.Input, 100)
	display := markdowntransform.NewDisplay(func(_ context.Context, in markdowntransform.Input) (string, error) {
		inputs <- in
		return in.Markdown, nil
	})
	display.Start(t.Context())
	m.screen.Transcript.SetMarkdownDisplay(display)
	streaming := map[string]bool{}
	return func(markdown string, want bool, msg string) {
		t.Helper()
		leanTranscriptText(m)
		require.Eventually(t, func() bool {
			for {
				select {
				case in := <-inputs:
					streaming[in.Markdown] = in.IsStreaming
				default:
					got, seen := streaming[markdown]
					return seen && got == want
				}
			}
		}, 5*time.Second, time.Millisecond, "%q: %s", markdown, msg)
	}
}

func TestLeanCompactionKeepsLiveReplyStreaming(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	assertStreaming := transformInputs(t, m)
	ctx := t.Context()
	m.handleEvent(ctx, runtime.StreamStarted("root", "root"))
	m.handleEvent(ctx, runtime.AgentChoice("root", "root", "partial", "answer"))
	m.handleEvent(ctx, runtime.SessionCompaction("root", "started", "root"))
	m.handleEvent(ctx, runtime.SessionCompaction("root", "completed", "root"))
	assertStreaming("partial", true, "compaction inside a live stream does not complete the reply")
}

func TestLeanNestedStopCompletesOnlyItsSession(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	assertStreaming := transformInputs(t, m)
	ctx := t.Context()
	m.handleEvent(ctx, runtime.StreamStarted("root", "root"))
	m.handleEvent(ctx, runtime.AgentChoice("root", "root", "parent reply", "parent"))
	m.handleEvent(ctx, runtime.StreamStarted("child", "worker"))
	m.handleEvent(ctx, runtime.AgentChoice("worker", "child", "child reply", "child"))
	m.handleEvent(ctx, runtime.StreamStopped("child", "worker", "normal"))
	assertStreaming("child reply", false, "the stopped nested stream's reply completes")
	assertStreaming("parent reply", true, "the parent's reply stays live")
}

func TestLeanNestedStreamSharingTheSessionKeepsParentLive(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	assertStreaming := transformInputs(t, m)
	ctx := t.Context()
	m.handleEvent(ctx, runtime.StreamStarted("root", "root"))
	m.handleEvent(ctx, runtime.AgentChoice("root", "root", "parent reply", "parent"))
	m.handleEvent(ctx, runtime.StreamStarted("root", "worker"))
	m.handleEvent(ctx, runtime.StreamStopped("root", "worker", "normal"))
	m.handleEvent(ctx, runtime.AgentChoice("root", "root", " continues", "parent"))
	assertStreaming("parent reply continues", true, "the parent's reply stays live and unsplit")
	m.handleEvent(ctx, runtime.StreamStopped("root", "root", "normal"))
	assertStreaming("parent reply continues", false, "the reply completes")
}

func TestLeanCompletionDoesNotSplitChunksWithoutTransformers(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	ctx := t.Context()
	m.handleEvent(ctx, runtime.StreamStarted("root", "root"))
	m.handleEvent(ctx, runtime.StreamStarted("child", "worker"))
	m.handleEvent(ctx, runtime.AgentChoice("worker", "child", "one", "child"))
	m.handleEvent(ctx, runtime.StreamStopped("child", "worker", "normal"))
	assert.Equal(t, 0, m.screen.Transcript.BlockCount(), "completing a session does not flush its pending block")
}
