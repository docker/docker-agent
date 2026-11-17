package messages

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/markdowntransform"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func transformingModel(t *testing.T) *model {
	t.Helper()
	display := markdowntransform.NewDisplay(func(_ context.Context, in markdowntransform.Input) (string, error) {
		return "T:" + in.Markdown, nil
	})
	display.Start(t.Context())
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.SetMarkdownDisplay(display)
	return m
}

func TestMarkdownDisplayStreamingLifecycle(t *testing.T) {
	t.Parallel()
	m := transformingModel(t)
	m.AppendAssistantContent("s", "reply", "root", "first")
	first := m.views[0].(message.Model)
	assert.True(t, first.Streaming(), "the first chunk creates a live view")

	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)
	assert.True(t, first.Streaming(), "a tool-call boundary does not complete the reply")
	m.AppendReasoningContent("s", "reply", "root", "thinking")
	m.renderedItems.Put(0, renderedItem{})

	m.ReconcileAssistantContent("s", "reply", "root", "first")
	assert.False(t, first.Streaming(), "the saved message completes every segment")
	_, cached := m.renderedItems.Get(0)
	assert.False(t, cached, "completed output must not stay cached as streaming")

	m.AppendAssistantContent("s", "cancelled", "root", "partial")
	last := m.views[len(m.views)-1].(message.Model)
	m.FinalizeStream()
	assert.False(t, last.Streaming(), "a reply without a saved message completes with the stream")
}

func TestMarkdownTransformedSplicesTheLiveReply(t *testing.T) {
	t.Parallel()
	m := transformingModel(t)
	m.AppendToLastMessage("root", "one")
	m.AddUserMessage("between")
	m.AppendToLastMessage("root", "two")
	settle := func(want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			m.Update(tuimessages.MarkdownTransformedMsg{})
			return strings.Contains(m.View(), want)
		}, 5*time.Second, 5*time.Millisecond)
	}
	settle("T:two")
	require.Contains(t, m.View(), "T:one")
	before := make([]renderedItem, 2)
	for i := range before {
		before[i], _ = m.renderedItems.Get(i)
	}

	m.AppendToLastMessage("root", " more")
	m.View()
	settle("T:two more")
	assert.False(t, m.renderDirty, "the live reply is spliced in, not a full rebuild")
	for i := range before {
		item, cached := m.renderedItems.Get(i)
		require.True(t, cached, "item %d", i)
		assert.Equal(t, before[i].lines, item.lines, "unchanged item %d is not re-rendered", i)
	}
}

func TestMarkdownTransformedHistoryRebuildsOnce(t *testing.T) {
	t.Parallel()
	m := transformingModel(t)
	m.AppendToLastMessage("root", "one")
	m.AddUserMessage("between")
	m.AppendToLastMessage("root", "two")
	m.AddUserMessage("last")
	m.View()
	require.Eventually(t, func() bool {
		m.Update(tuimessages.MarkdownTransformedMsg{})
		return m.renderDirty
	}, 5*time.Second, 5*time.Millisecond, "historical results defer to one rebuild")
	assert.Contains(t, m.View(), "T:one")
}

func TestCompleteSessionCompletesOnlyThatSession(t *testing.T) {
	t.Parallel()
	m := transformingModel(t)
	m.AppendAssistantContent("root", "parent", "root", "parent reply")
	m.AppendAssistantContent("child", "reply", "worker", "child reply")
	parent, child := m.views[0].(message.Model), m.views[1].(message.Model)
	m.CompleteSession("child")
	assert.False(t, child.Streaming(), "the stopped nested stream's reply completes")
	assert.True(t, parent.Streaming(), "the parent's reply stays live")
}

func TestCompleteSessionMaterializesDeferredTail(t *testing.T) {
	t.Parallel()
	m := transformingModel(t)
	m.AppendAssistantContent("s", "reply", "root", "first")
	m.userHasScrolled = true
	m.AppendAssistantContent("s", "reply", "root", " tail")
	require.NotEmpty(t, m.deferredTail)
	m.CompleteSession("s")
	assert.Empty(t, m.deferredTail, "the completed snapshot is the full reply")
	assert.Equal(t, "first tail", m.messages[0].Content)
	assert.False(t, m.views[0].(message.Model).Streaming())
}

func TestCompletingSeveralSegmentsSharesOneRebuild(t *testing.T) {
	t.Parallel()
	m := transformingModel(t)
	m.AppendAssistantContent("s", "reply", "root", "before")
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)
	m.AppendAssistantContent("s", "reply", "root", "after")
	m.AddUserMessage("next")
	m.View()
	require.False(t, m.renderDirty)
	m.CompleteSession("s")
	assert.True(t, m.renderDirty, "historical segments are invalidated for one rebuild")
	for _, i := range []int{0, 2} {
		assert.False(t, m.views[i].(message.Model).Streaming(), "segment %d", i)
	}
}
