package chatserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// agentPolicy decides which agent in a team is exposed by the server and
// which one to run for a given request. It is built once at startup and is
// read-only thereafter, so it's safe to share across goroutines.
type agentPolicy struct {
	// exposed is the list of agent names advertised on /v1/models.
	exposed []string
	// fallback is used when the request's "model" field doesn't match any
	// exposed agent (so we don't fail when clients hard-code "gpt-4").
	fallback string
}

// newAgentPolicy validates the requested agent name against the team and
// returns the selection policy. If agentName is empty, every agent in the
// team is exposed and the team's default agent is used as fallback.
// Otherwise only that one agent is exposed and used.
func newAgentPolicy(t *team.Team, agentName string) (agentPolicy, error) {
	if agentName != "" {
		if !slices.Contains(t.AgentNames(), agentName) {
			return agentPolicy{}, fmt.Errorf("agent %q not found", agentName)
		}
		return agentPolicy{exposed: []string{agentName}, fallback: agentName}, nil
	}
	a, err := t.DefaultAgent()
	if err != nil {
		return agentPolicy{}, fmt.Errorf("resolving default agent: %w", err)
	}
	return agentPolicy{exposed: t.AgentNames(), fallback: a.Name()}, nil
}

// pick returns the agent name to use for a request. The "model" field is
// honoured when it matches an exposed agent; otherwise we silently fall
// back, mirroring how OpenAI's API behaves with unknown model strings on
// some compatible servers.
func (p agentPolicy) pick(model string) string {
	if model != "" && slices.Contains(p.exposed, model) {
		return model
	}
	return p.fallback
}

// buildSession converts an OpenAI-style message history into a docker-agent
// session. System messages are added as system context, prior user/
// assistant/tool turns are replayed verbatim so the agent sees the full
// conversation, and the latest user message becomes the prompt.
//
// Tool approval and non-interactive mode are forced on: this is a headless
// HTTP endpoint, there's no human in the loop to approve anything.
//
// Returns nil when the history contains no usable user message, in which
// case the caller should reject the request.
func buildSession(messages []ChatCompletionMessage) *session.Session {
	sess := session.New(
		session.WithToolsApproved(true),
		session.WithNonInteractive(true),
	)

	hasUser := false
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if len(m.Parts) > 0 && (role == "" || (role != "system" && role != "assistant" && role != "tool")) {
			// Multi-part content: route through chat.MultiContent so the
			// runtime/provider sees image parts. Only user-style messages
			// support images today.
			parts := convertParts(m.Parts)
			if len(parts) == 0 {
				continue
			}
			sess.AddMessage(&session.Message{Message: chat.Message{
				Role:         chat.MessageRoleUser,
				Content:      m.Content,
				MultiContent: parts,
			}})
			hasUser = true
			continue
		}

		content := m.Content
		if strings.TrimSpace(content) == "" {
			continue
		}
		switch role {
		case "system":
			sess.AddMessage(session.SystemMessage(content))
		case "assistant":
			sess.AddMessage(&session.Message{Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: content,
			}})
		case "tool":
			sess.AddMessage(&session.Message{Message: chat.Message{
				Role:       chat.MessageRoleTool,
				Content:    content,
				ToolCallID: m.ToolCallID,
			}})
		default:
			// user, developer, or any other role: feed it to the agent
			// as user input rather than rejecting the request.
			sess.AddMessage(session.UserMessage(content))
			hasUser = true
		}
	}

	if !hasUser {
		return nil
	}
	return sess
}

// convertParts maps the chatserver wire shape to chat.MessagePart so
// images and (future) other typed parts reach the runtime intact.
// Unknown part types are dropped; an empty result tells the caller to
// skip the message entirely.
func convertParts(in []ContentPart) []chat.MessagePart {
	out := make([]chat.MessagePart, 0, len(in))
	for _, p := range in {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			out = append(out, chat.MessagePart{
				Type: chat.MessagePartTypeText,
				Text: p.Text,
			})
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			out = append(out, chat.MessagePart{
				Type: chat.MessagePartTypeImageURL,
				ImageURL: &chat.MessageImageURL{
					URL:    p.ImageURL.URL,
					Detail: chat.ImageURLDetail(p.ImageURL.Detail),
				},
			})
		}
	}
	return out
}

// appendLatestUser walks msgs from the end and appends only the last
// user-role message into sess. Used by conversation continuation, where
// the session already contains the full prior history and we just need
// to inject what the client just said.
func appendLatestUser(sess *session.Session, msgs []ChatCompletionMessage) {
	for _, m := range slices.Backward(msgs) {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		// Treat any non-system/assistant/tool role as user (matches
		// buildSession's policy).
		if role == "system" || role == "assistant" || role == "tool" {
			continue
		}
		parts := convertParts(m.Parts)
		if len(parts) > 0 {
			sess.AddMessage(&session.Message{Message: chat.Message{
				Role:         chat.MessageRoleUser,
				Content:      m.Content,
				MultiContent: parts,
			}})
			return
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		sess.AddMessage(session.UserMessage(m.Content))
		return
	}
}

// agentEmit collects the side-effect callbacks invoked by runAgentLoop as
// it drives the runtime. All callbacks are optional; nil means "ignore
// this kind of event".
type agentEmit struct {
	// onContent fires for every assistant text delta from the model.
	onContent func(string)
	// onToolCall fires when the agent dispatches a tool. Called once per
	// tool, with the tool already populated with its arguments.
	onToolCall func(ToolCallReference)
}

// runAgentLoop drives the runtime to completion, forwarding events to
// the supplied callbacks.
//
// The session is built with ToolsApproved=true and NonInteractive=true,
// which means the runtime auto-approves tool calls and auto-stops on
// max-iterations. The handler cases below are intentionally kept as
// defence-in-depth: if those session settings ever drift, this handler
// still won't hang the request. Elicitation is the exception — the
// runtime always blocks until we respond, so its case is required for
// correctness, not just defence.
//
// All ErrorEvents seen in the run are joined into the returned error so
// callers can see the full picture; the loop keeps draining until the
// stream closes so the runtime can shut down cleanly.
func runAgentLoop(ctx context.Context, rt runtime.Runtime, sess *session.Session, emit agentEmit) error {
	var runErrs []error
	toolIndex := 0
	for ev := range rt.RunStream(ctx, sess) {
		switch e := ev.(type) {
		case *runtime.AgentChoiceEvent:
			if emit.onContent != nil {
				emit.onContent(e.Content)
			}
		case *runtime.ToolCallEvent:
			if emit.onToolCall != nil {
				emit.onToolCall(ToolCallReference{
					Index:    toolIndex,
					ID:       e.ToolCall.ID,
					Type:     string(e.ToolCall.Type),
					Function: ToolCallFunction{Name: e.ToolCall.Function.Name, Arguments: e.ToolCall.Function.Arguments},
				})
				toolIndex++
			}
		case *runtime.ToolCallConfirmationEvent:
			// Defensive: should never fire while ToolsApproved=true.
			rt.Resume(ctx, runtime.ResumeApprove())
		case *runtime.ElicitationRequestEvent:
			// Required: the runtime blocks until we respond, regardless
			// of NonInteractive. Decline so the tool call fails fast.
			_ = rt.ResumeElicitation(ctx, tools.ElicitationActionDecline, nil)
		case *runtime.MaxIterationsReachedEvent:
			// Defensive: in non-interactive mode the runtime already
			// stops on its own and this Resume is dropped.
			rt.Resume(ctx, runtime.ResumeReject(""))
		case *runtime.ErrorEvent:
			runErrs = append(runErrs, errors.New(e.Error))
		}
	}
	return errors.Join(runErrs...)
}

// sessionUsage extracts approximate token usage from a completed session
func sessionUsage(sess *session.Session) *ChatCompletionUsage {
	return &ChatCompletionUsage{
		PromptTokens:     sess.InputTokens,
		CompletionTokens: sess.OutputTokens,
		TotalTokens:      sess.InputTokens + sess.OutputTokens,
	}
}
