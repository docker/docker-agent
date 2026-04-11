package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

const maxSummaryTokens = 16_000

// maxKeepTokens is the maximum number of tokens to preserve from the end of
// the conversation during compaction. These recent messages are kept verbatim
// so the LLM can continue naturally after compaction.
const maxKeepTokens = 20_000

// doCompact runs compaction on a session and applies the result (events,
// persistence, token count updates). The agent is used to extract the
// conversation from the session and to obtain the model for summarization.
func (r *LocalRuntime) doCompact(ctx context.Context, sess *session.Session, a *agent.Agent, additionalPrompt string, events chan Event) {
	lg := slog.With("session_id", sess.ID, "agent", a.Name(), "action", "compaction")

	lg.Debug("Generating summary for session")
	events <- SessionCompaction(sess.ID, "started", a.Name())
	defer func() {
		events <- SessionCompaction(sess.ID, "completed", a.Name())
	}()

	// Build a model just for compaction.
	summaryModel := provider.CloneWithOptions(ctx, a.Model(),
		options.WithStructuredOutput(nil),
		options.WithMaxTokens(maxSummaryTokens),
	)

	m, err := r.modelsStore.GetModel(ctx, summaryModel.ID())
	if err != nil && errors.Is(err, modelsdev.ErrProviderNotFound) {
		lg.Debug("Provider not found; attempting to find by model name", "error", err)

		db, dberr := r.modelsStore.GetDatabase(ctx)
		if dberr != nil {
			lg.Error("Provider not found and failed to find by model name", "error", dberr)
			events <- Error("Failed to get db to find model definition: " + dberr.Error())
			return
		}

		// Find the lowest context limit for this model, regardless of the provider.
		for _, provider := range db.Providers {
			if v, ok := provider.Models[summaryModel.BaseConfig().ModelConfig.Model]; ok {
				if m == nil || v.Limit.Context < m.Limit.Context {
					m = &v
					err = nil
				}
			}
		}
	}

	if err != nil {
		lg.Error("Failed to get model definition to generate session summary", "error", err)
		events <- Error("Failed to get model definition: " + err.Error())
		return
	}

	compactionAgent := agent.New("root", compaction.SystemPrompt, agent.WithModel(summaryModel))

	// Compute the messages to compact, keeping recent messages aside.
	messages, firstKeptEntry := extractMessagesToCompact(sess, compactionAgent, int64(m.Limit.Context), additionalPrompt)

	// Run the compaction.
	compactionSession := session.New(
		session.WithTitle("Generating summary"),
		session.WithMessages(toItems(messages)),
	)

	t := team.New(team.WithAgents(compactionAgent))
	rt, err := New(t, WithSessionCompaction(false))
	if err != nil {
		lg.Error("Failed to generate session summary", "error", err)
		events <- Error(err.Error())
		return
	}
	if _, err = rt.Run(ctx, compactionSession); err != nil {
		lg.Error("Failed to generate session summary", "error", err)
		events <- Error(err.Error())
		return
	}

	summary := compactionSession.GetLastAssistantMessageContent()
	if summary == "" {
		return
	}

	// Update the session.
	sess.InputTokens = compactionSession.OutputTokens
	sess.OutputTokens = 0
	sess.Messages = append(sess.Messages, session.Item{
		Summary:        summary,
		FirstKeptEntry: firstKeptEntry,
		Cost:           compactionSession.TotalCost(),
	})
	_ = r.sessionStore.UpdateSession(ctx, sess)

	lg.Debug("Generated session summary", "summary_length", len(summary))
	events <- SessionSummary(sess.ID, summary, a.Name(), firstKeptEntry)
}

// extractMessagesToCompact returns the messages to send to the compaction model
// and the index (into sess.Messages) of the first message that was kept aside.
// Recent messages (up to maxKeepTokens) are excluded from compaction so they
// can be preserved verbatim in the session after summarization.
func extractMessagesToCompact(sess *session.Session, compactionAgent *agent.Agent, contextLimit int64, additionalPrompt string) ([]chat.Message, int) {
	// Add all the existing messages.
	var messages []chat.Message
	for _, msg := range sess.GetMessages(compactionAgent) {
		if msg.Role == chat.MessageRoleSystem {
			continue
		}

		msg.Cost = 0
		msg.CacheControl = false

		messages = append(messages, msg)
	}

	// Split: keep the last N tokens of messages aside so the LLM retains
	// recent context after compaction.
	splitIdx := splitIndexForKeep(messages, maxKeepTokens)
	messagesToCompact := messages[:splitIdx]
	// Compute firstKeptEntry: index into sess.Messages of the first kept message.
	// The kept messages start at splitIdx in the non-system filtered list. We
	// need to map this back to the original sess.Messages index.
	firstKeptEntry := mapToSessionIndex(sess, splitIdx)

	messages = messagesToCompact

	// Prepare the first (system) message.
	systemPromptMessage := chat.Message{
		Role:      chat.MessageRoleSystem,
		Content:   compaction.SystemPrompt,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	systemPromptMessageLen := compaction.EstimateMessageTokens(&systemPromptMessage)

	// Prepare the last (user) message.
	userPrompt := compaction.UserPrompt
	if additionalPrompt != "" {
		userPrompt += "\n\n" + additionalPrompt
	}
	userPromptMessage := chat.Message{
		Role:      chat.MessageRoleUser,
		Content:   userPrompt,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	userPromptMessageLen := compaction.EstimateMessageTokens(&userPromptMessage)

	// Truncate the messages so that they fit in the available context limit
	// (minus the expected max length of the summary).
	contextAvailable := max(0, contextLimit-maxSummaryTokens-systemPromptMessageLen-userPromptMessageLen)
	firstIndex := firstMessageToKeep(messages, contextAvailable)
	if firstIndex < len(messages) {
		messages = messages[firstIndex:]
	} else {
		messages = nil
	}

	// Prepend the first (system) message.
	messages = append([]chat.Message{systemPromptMessage}, messages...)

	// Append the last (user) message.
	messages = append(messages, userPromptMessage)

	return messages, firstKeptEntry
}

// splitIndexForKeep returns the index that splits messages into [0:idx] (to
// compact) and [idx:] (to keep). It walks backwards accumulating tokens up to
// maxTokens, snapping to user/assistant boundaries.
func splitIndexForKeep(messages []chat.Message, maxTokens int64) int {
	if len(messages) == 0 {
		return 0
	}

	var tokens int64
	// Walk from the end; find the earliest index whose suffix fits in maxTokens.
	lastValidBoundary := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		tokens += compaction.EstimateMessageTokens(&messages[i])
		if tokens > maxTokens {
			return lastValidBoundary
		}
		role := messages[i].Role
		if role == chat.MessageRoleUser || role == chat.MessageRoleAssistant {
			lastValidBoundary = i
		}
	}
	// All messages fit within maxTokens — don't keep any aside (compact everything).
	return len(messages)
}

// mapToSessionIndex maps an index in the non-system-filtered message list back
// to the corresponding index in sess.Messages. It counts only message items
// that are not system messages.
func mapToSessionIndex(sess *session.Session, filteredIdx int) int {
	count := 0
	for i, item := range sess.Messages {
		if item.IsMessage() && item.Message.Message.Role != chat.MessageRoleSystem {
			if count == filteredIdx {
				return i
			}
			count++
		}
	}
	// filteredIdx is past the end — no messages to keep.
	return len(sess.Messages)
}

func firstMessageToKeep(messages []chat.Message, contextLimit int64) int {
	var tokens int64

	lastValidMessageSeen := len(messages)

	for i := len(messages) - 1; i >= 0; i-- {
		tokens += compaction.EstimateMessageTokens(&messages[i])
		if tokens > contextLimit {
			return lastValidMessageSeen
		}

		role := messages[i].Role
		if role == chat.MessageRoleUser || role == chat.MessageRoleAssistant {
			lastValidMessageSeen = i
		}
	}

	return lastValidMessageSeen
}

func toItems(messages []chat.Message) []session.Item {
	var items []session.Item

	for _, message := range messages {
		items = append(items, session.Item{
			Message: &session.Message{
				Message: message,
			},
		})
	}

	return items
}
