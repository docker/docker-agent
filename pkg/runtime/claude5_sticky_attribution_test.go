package runtime

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/session"
)

func TestClaude5EarlyStickyAttribution(t *testing.T) {
	t.Parallel()
	const target = "claude-sonnet-4-6"
	for _, tc := range []struct {
		name, model, reply, want string
		opts                     map[string]any
		history                  bool
	}{
		{name: "explicit truncated", model: "claude-opus-4-8", opts: map[string]any{"fallbacks": []string{target}}, reply: anthropictest.StickyTruncatedReply("m", target, "partial"), want: target},
		{name: "history truncated", model: "claude-sonnet-5-5", history: true, reply: anthropictest.StickyTruncatedReply("m", target, "partial"), want: target},
		{name: "explicit message-only usage", model: "claude-opus-4-8", opts: map[string]any{"fallbacks": []string{target}}, reply: anthropictest.StickyMessageUsageReply("m", target, "partial"), want: "claude-opus-4-8"},
		{name: "ordinary Claude5 alias", model: "claude-sonnet-5-5", reply: anthropictest.StickyMessageUsageReply("m", "claude-sonnet-5-5-20261001", "partial"), want: "claude-sonnet-5-5"},
		{name: "ordinary alias", model: target, opts: map[string]any{"interleaved_thinking": true}, reply: anthropictest.StickyMessageUsageReply("m", "claude-sonnet-4-6-20260217", "partial"), want: target},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			replies := []string{tc.reply}
			if tc.history {
				replies = append([]string{anthropictest.FallbackMidOutputReply("prior", "claude-sonnet-5-5", target, "before", "after", "t1")}, replies...)
			}
			server := anthropictest.NewServer(t, replies...)
			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			store := openSessionStore(t, dbPath)
			rt := anthropicRuntime(t, server, tc.model, tc.opts, store, mockModelStoreWithCostAndLimit{limit: 40_000})
			sess := session.New(session.WithUserMessage("q"), session.WithTitle("Sticky attribution"))
			if tc.history {
				requireNoErrorEvents(t, drainRunStream(t, rt, sess))
				sess.AddMessage(session.UserMessage("next"))
			}
			events := drainRunStream(t, rt, sess)
			requireNoErrorEvents(t, events)

			var usage *MessageUsage
			var added *session.Message
			for _, event := range events {
				switch event := event.(type) {
				case *TokenUsageEvent:
					if event.Usage.LastMessage != nil {
						usage = event.Usage.LastMessage
					}
				case *MessageAddedEvent:
					added = event.Message
				}
			}
			require.NotNil(t, added)
			assert.Equal(t, "anthropic/"+tc.want, added.Message.Model)
			assert.Equal(t, "partial", added.Message.Content)
			if added.Message.Usage == nil {
				assert.Nil(t, usage, "truncated streams must not invent final usage")
			} else {
				require.NotNil(t, usage)
				assert.Equal(t, added.Message.Model, usage.Model)
				assert.Equal(t, int64(20), usage.InputTokens)
			}

			reopened := openSessionStore(t, dbPath)
			loaded, err := reopened.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			for _, current := range []*session.Session{sess, loaded} {
				messages := current.OwnMessages()
				wantCount := 2
				if tc.history {
					wantCount = 4
				}
				require.Len(t, messages, wantCount)
				assert.Equal(t, chat.MessageRoleAssistant, messages[wantCount-1].Message.Role)
				assert.Equal(t, added.Message, messages[wantCount-1].Message)
			}
			assert.Len(t, server.Requests(), len(replies))
		})
	}
}
