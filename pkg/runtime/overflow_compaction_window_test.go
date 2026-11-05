package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// windowProvider serves a smaller real context window than the catalogue
// advertises: any request whose estimated size exceeds window is rejected
// as a token overflow, whether it is a main-agent or a summary request.
type windowProvider struct {
	window int64

	mu           sync.Mutex
	summarySizes []int64
	summaryFails int
	mainFails    int
}

func (p *windowProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/window-model") }

func (p *windowProvider) BaseConfig() base.Config {
	return base.Config{RebuildProvider: func(context.Context, *latest.ModelConfig, ...options.Opt) (provider.Provider, error) {
		return p, nil
	}}
}

func (p *windowProvider) MaxTokens() int { return 0 }

func (p *windowProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	var size int64
	isSummary := false
	for i := range messages {
		size += compaction.EstimateMessageTokens(&messages[i])
		if messages[i].Role == chat.MessageRoleSystem && messages[i].Content == compaction.SystemPrompt {
			isSummary = true
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if isSummary {
		p.summarySizes = append(p.summarySizes, size)
	}
	if size > p.window {
		if isSummary {
			p.summaryFails++
		} else {
			p.mainFails++
		}
		return nil, modelerrors.NewContextOverflowError(fmt.Errorf("maximum context length is %d tokens, request has %d", p.window, size))
	}
	if isSummary {
		return newStreamBuilder().AddContent("the summary").AddStopWithUsage(1, 1).Build(), nil
	}
	return newStreamBuilder().AddContent("answer").AddStopWithUsage(size, 1).Build(), nil
}

// largeHistory builds a session of turns big enough to overflow window
// while staying far below the advertised catalogue limit, so proactive
// compaction never fires and only overflow recovery can save the run.
func largeHistory(turns int) *session.Session {
	big := strings.Repeat("x", 7_000) // ~2k estimated tokens
	sess := session.New()
	for i := range turns {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("step %d", i)))
		id := fmt.Sprintf("call_%d", i)
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{{ID: id, Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}}},
		}))
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, Content: big}))
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("done %d", i)}))
	}
	sess.AddMessage(session.UserMessage("continue"))
	return sess
}

// TestOverflowRecovery_SummaryOverflowShrinksConversation reproduces a
// session that overflows a provider whose real window is smaller than the
// catalogue limit. The summary request built against the catalogue limit
// carries almost the whole history and overflows too; recovery must retry
// the summary with less conversation instead of failing the run.
func TestOverflowRecovery_SummaryOverflowShrinksConversation(t *testing.T) {
	t.Parallel()

	prov := &windowProvider{window: 40_000}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 1_000_000}),
	)
	require.NoError(t, err)

	sess := largeHistory(30)

	var errs []string
	var summaries, answers int
	for ev := range rt.RunStream(t.Context(), sess) {
		switch e := ev.(type) {
		case *ErrorEvent:
			errs = append(errs, e.Error)
		case *SessionSummaryEvent:
			summaries++
		case *AgentChoiceEvent:
			if e.Content == "answer" {
				answers++
			}
		}
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	require.Empty(t, errs, "overflow recovery must not surface an error")
	assert.Equal(t, 1, summaries, "exactly one summary must be applied")
	assert.Equal(t, 1, answers, "the main request must succeed after compaction")
	assert.Equal(t, 1, prov.mainFails, "only the first main request should overflow")
	require.GreaterOrEqual(t, len(prov.summarySizes), 2, "the first summary request must overflow and be retried")
	for i := 1; i < len(prov.summarySizes); i++ {
		assert.Less(t, prov.summarySizes[i], prov.summarySizes[i-1], "each summary retry must send less conversation")
	}
	assert.LessOrEqual(t, prov.summarySizes[len(prov.summarySizes)-1], prov.window)
}

// TestRunCompactionAgent_PreservesTokenOverflow verifies the child run's
// structured context_exceeded code survives as a typed overflow so the
// compactor can tell it apart from other failures.
func TestRunCompactionAgent_PreservesTokenOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantOverflow bool
		wantMsg      string
	}{
		{"token overflow", modelerrors.NewContextOverflowError(errors.New("maximum context length exceeded")), true, "exceeded the model's context window"},
		{"other model error", errors.New("boom"), false, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			prov := &errorProvider{id: "test/error-model", err: tt.err}
			root := agent.New("root", "test", agent.WithModel(prov))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
			require.NoError(t, err)

			parent := session.New()
			child := session.New(session.WithUserMessage("summarize"))
			runErr := rt.runCompactionAgent(t.Context(), root, child, parent, NewChannelSink(make(chan Event, 64)))
			require.Error(t, runErr)
			assert.Contains(t, runErr.Error(), tt.wantMsg, "the provider's error must be what surfaced")
			assert.Equal(t, tt.wantOverflow, modelerrors.OverflowKindOf(runErr) == modelerrors.OverflowKindTokens)
		})
	}
}

// TestOverflowRecovery_ExhaustedRetriesKeepErrorWording: when the window is
// too small for any summary, the run still fails cleanly with the
// pre-existing user-facing wording.
func TestOverflowRecovery_ExhaustedRetriesKeepErrorWording(t *testing.T) {
	t.Parallel()

	prov := &windowProvider{window: 1}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 1_000_000}),
	)
	require.NoError(t, err)

	var errs []string
	for ev := range rt.RunStream(t.Context(), largeHistory(30)) {
		if e, ok := ev.(*ErrorEvent); ok {
			errs = append(errs, e.Error)
		}
	}
	require.NotEmpty(t, errs)
	assert.Contains(t, errs, "run compaction agent: The conversation has exceeded the model's context window. Compact the conversation to reduce its size, or start a new session.")
	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.summarySizes, 4, "1 attempt + 3 bounded retries")
}
