package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
)

type lastResponse struct {
	sessionID    string
	messageID    string
	agentName    string
	content      strings.Builder
	stopReason   string
	finishReason chat.FinishReason
}

func (r *lastResponse) observe(event runtime.Event) {
	if scoped, ok := event.(runtime.SessionScoped); ok {
		if id := scoped.GetSessionID(); id != "" && id != r.sessionID {
			return
		}
	}

	switch e := event.(type) {
	case *runtime.AgentChoiceEvent:
		if e.MessageID != r.messageID || e.AgentName != r.agentName {
			r.content.Reset()
		}
		r.messageID = e.MessageID
		r.agentName = e.AgentName
		r.content.WriteString(e.Content)
	case *runtime.AgentChoiceReasoningEvent:
		if e.MessageID != r.messageID || e.AgentName != r.agentName {
			r.content.Reset()
			r.messageID = e.MessageID
			r.agentName = e.AgentName
		}
	case *runtime.MessageAddedEvent:
		if e.AssistantMessageEmpty || e.HasToolCalls {
			r.content.Reset()
		}
		if e.Message == nil || e.Message.Implicit || e.Message.Message.Role != chat.MessageRoleAssistant {
			return
		}
		msg := e.Message.Message
		r.content.Reset()
		r.messageID = msg.MessageID
		r.agentName = e.AgentName
		if len(msg.ToolCalls) == 0 && msg.FunctionCall == nil {
			r.content.WriteString(chat.VisibleAssistantContent(msg.Content))
		}
	case *runtime.StreamStoppedEvent:
		r.stopReason = e.Reason
		r.finishReason = e.FinishReason
		if e.AgentName != "" && e.AgentName != r.agentName {
			r.content.Reset()
		}
	case *runtime.MaxIterationsReachedEvent:
		r.content.Reset()
	case *runtime.TokenUsageEvent:
		if e.AssistantMessageEmpty {
			r.content.Reset()
		}
		if e.Usage != nil && e.Usage.LastMessage != nil && e.Usage.LastMessage.FinishReason == chat.FinishReasonToolCalls {
			r.content.Reset()
		}
	}
}

func (r *lastResponse) validate() error {
	if r.stopReason != "" && r.stopReason != runtime.TurnEndReasonNormal {
		return fmt.Errorf("agent stopped without a final answer: %s", r.stopReason)
	}
	if r.finishReason == chat.FinishReasonLength {
		return errors.New("agent answer was truncated by the token limit")
	}
	if strings.TrimSpace(r.content.String()) == "" {
		return errors.New("agent produced no final answer")
	}
	return nil
}

func (r *lastResponse) print(out io.Writer, outputJSON bool) error {
	if err := r.validate(); err != nil {
		return err
	}
	content := r.content.String()
	if outputJSON {
		// Keep structured answers as JSON values, not quoted JSON strings.
		var value any = content
		if json.Valid([]byte(content)) {
			value = json.RawMessage(content)
		}
		return json.NewEncoder(out).Encode(value)
	}
	_, err := fmt.Fprintln(out, content)
	return err
}
