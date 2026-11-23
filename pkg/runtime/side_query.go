package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/runtime/pricing"
	"github.com/docker/docker-agent/pkg/session"
)

const sideQueryInstruction = `Answer only the user's side question, concisely, using the conversation as background. Do not continue the main task. You have no tools and cannot inspect files or take actions. If the context does not contain the answer, say so. The conversation snapshot may omit work still in progress.`

// SideQuery is an immutable context snapshot. Run may be retried without recapturing history.
type SideQuery struct {
	Model string
	run   func(context.Context, string, func(string)) error
}

// Run executes the side question against the captured snapshot. Safe to
// call again after a failure; each call is an independent request.
func (q *SideQuery) Run(ctx context.Context, question string, emit func(string)) error {
	if strings.TrimSpace(question) == "" {
		return errors.New("a side question is required")
	}
	return q.run(ctx, question, emit)
}

// PrepareSideQuery never enters the agent loop or modifies its foreground operation.
func (r *LocalRuntime) PrepareSideQuery(ctx context.Context, sess *session.Session) (*SideQuery, error) {
	a := r.CurrentAgent()
	if sess == nil || a == nil || a.HasHarness() {
		return nil, errors.New("/btw requires a local model-backed session")
	}
	model := a.Model(ctx)
	if model == nil {
		return nil, errors.New("no model available for /btw")
	}
	store, ok := r.sessionStore.(session.AuxiliaryUsageStore)
	if !ok {
		return nil, errors.New("session store does not support /btw usage accounting")
	}
	// Keep item positions intact: compaction summaries refer to history indexes.
	snapshot := session.New()
	snapshot.Messages = sess.MessagesSnapshot()
	for i, item := range snapshot.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant && item.Message.Message.FinishReason == "" && item.Message.Message.Usage == nil {
			snapshot.Messages[i].Message = nil
		}
	}
	history, _ := snapshot.GetMessagesWithInstructionContext(a, sess.InstructionContextSnapshot())
	messages := sideQueryMessages(history)
	return &SideQuery{Model: model.ID().String(), run: func(ctx context.Context, question string, emit func(string)) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		// A separate conversation identity also isolates provider-side session caches.
		ctx = httpclient.ContextWithSessionID(ctx, uuid.NewV4().String())
		cloned, err := provider.Clone(ctx, model, options.WithStructuredOutput(nil), options.WithNoThinking(), options.WithMaxTokens(2048), options.WithTextOnly(), options.WithDisableAutoPull())
		if err != nil {
			return err
		}
		limit := r.contextLimitForAgentModel(ctx, a, model.ID())
		prompt, reduced, err := fitSideQuery(messages, question, limit)
		if err != nil {
			return err
		}
		if reduced {
			emit("*Using reduced conversation context.*\n\n")
		}
		stream, err := cloned.CreateChatCompletionStream(ctx, prompt, nil)
		if err != nil {
			return err
		}
		defer stream.Close()
		var usage *chat.Usage
		var attempts []chat.UsageAttempt
		var content strings.Builder
		var streamErr error
		for {
			if streamErr = ctx.Err(); streamErr != nil {
				break
			}
			response, err := stream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					streamErr = err
				}
				break
			}
			if response.Usage != nil {
				usageSnapshot := *response.Usage
				usage = &usageSnapshot
			}
			if len(response.UsageAttempts) > 0 {
				attempts = response.UsageAttempts
			}
			if len(response.Choices) == 0 {
				continue
			}
			choice := response.Choices[0]
			if choice.Delta.ServerFallback != nil {
				streamErr = errors.New("side request switched models; retry the question")
				break
			}
			if len(choice.Delta.ToolCalls) > 0 || choice.Delta.FunctionCall != nil {
				streamErr = errors.New("side questions cannot call tools")
				break
			}
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
				emit(choice.Delta.Content)
			}
			if choice.FinishReason == chat.FinishReasonLength {
				streamErr = errors.New("answer reached the output limit")
				break
			}
		}
		cfg := cloned.BaseConfig()
		id := cloned.ID()
		if serving, ok := stream.(provider.ServingStream); ok {
			served := serving.ServingModel()
			cfg, id = served.Config, served.ID
		}
		if served, ok := provider.ServedBy(stream); ok {
			cfg, id = served.Config, served.ID
		}
		catalog, _ := r.modelsStore.GetModel(context.WithoutCancel(ctx), id)
		cost := pricing.ComputeMessageCost(usage, pricing.ApplyModelCost(catalog, id, usage, cfg))
		if len(attempts) > 0 {
			cost = attemptsCost(context.WithoutCancel(ctx), attempts, cloned.BaseConfig(), r.modelsStore)
			usage = &chat.Usage{}
			for _, attempt := range attempts {
				if attempt.Billed {
					usage.Add(attempt.Usage())
				}
			}
		}
		record := &session.AuxiliaryUsage{ID: uuid.NewV4().String(), Purpose: "/btw", Model: id.String(), Usage: usage, Cost: cost}
		if usage != nil {
			sess.AddAuxiliaryUsage(record)
			persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer persistCancel()
			if err := r.sessionStore.UpdateSession(persistCtx, sess); err != nil {
				return errors.Join(streamErr, fmt.Errorf("saving /btw usage: %w", err))
			}
			if err := store.AddAuxiliaryUsage(persistCtx, sess.ID, record); err != nil {
				return errors.Join(streamErr, fmt.Errorf("saving /btw usage: %w", err))
			}
		}
		if streamErr != nil {
			return streamErr
		}
		if strings.TrimSpace(content.String()) == "" {
			return errors.New("model returned an empty answer")
		}
		return nil
	}}, nil
}

// Flatten historical tool traffic into reference text, never provider continuation state.
func sideQueryMessages(history []chat.Message) []chat.Message {
	messages := []chat.Message{{Role: chat.MessageRoleSystem, Content: sideQueryInstruction}}
	for _, msg := range history {
		var textBuilder strings.Builder
		textBuilder.WriteString(msg.Content)
		for _, part := range msg.MultiContent {
			if part.Text != "" {
				textBuilder.WriteByte('\n')
				textBuilder.WriteString(part.Text)
			}
		}
		content := textBuilder.String()
		if strings.TrimSpace(content) == "" {
			continue
		}
		role := msg.Role
		if role == chat.MessageRoleSystem {
			content = "Agent instructions (background):\n" + content
			role = chat.MessageRoleUser
		}
		if role == chat.MessageRoleTool {
			content = "Completed tool result (background):\n" + content
			role = chat.MessageRoleUser
		}
		messages = append(messages, chat.Message{Role: role, Content: content})
	}
	return messages
}

func fitSideQuery(history []chat.Message, question string, limit int64) ([]chat.Message, bool, error) {
	prompt := slices.Clone(history)
	prompt = append(prompt, chat.Message{Role: chat.MessageRoleUser, Content: "Side question (answer only this):\n" + question})
	if limit <= 0 {
		limit = 32768
	}
	budget := limit - 4096
	var total int64
	for i := range prompt {
		total += compaction.EstimateMessageTokens(&prompt[i])
	}
	reduced := false
	for total > budget && len(prompt) > 2 {
		total -= compaction.EstimateMessageTokens(&prompt[1])
		prompt = append(prompt[:1], prompt[2:]...)
		reduced = true
	}
	if total > budget {
		return nil, reduced, errors.New("side question exceeds the model's context window")
	}
	return prompt, reduced, nil
}
