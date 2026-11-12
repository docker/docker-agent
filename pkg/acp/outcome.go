package acp

import (
	"fmt"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
)

func sessionNotFound(sid string) *acp.RequestError {
	return &acp.RequestError{Code: -32002, Message: "Resource not found", Data: map[string]any{"sessionId": sid, "error": fmt.Sprintf("session %s not found", sid)}}
}

type promptOutcome struct {
	sessionID    string
	finishReason chat.FinishReason
	streamReason string
	fatal        *acp.RequestError
}

func (o *promptOutcome) observe(event runtime.Event) {
	scoped, ok := event.(runtime.SessionScoped)
	if !ok || scoped.GetSessionID() != o.sessionID {
		return
	}
	switch e := event.(type) {
	case *runtime.MessageAddedEvent:
		if e.Message != nil && e.Message.Message.Role == chat.MessageRoleAssistant {
			o.finishReason = e.Message.Message.FinishReason
		}
	case *runtime.StreamStoppedEvent:
		o.streamReason = e.Reason
		if e.FinishReason != "" {
			o.finishReason = e.FinishReason
		}
	case *runtime.ErrorEvent:
		// Uncoded compaction errors and unscoped RAG errors can be recoverable.
		switch e.Code {
		case runtime.ErrorCodeModelError, runtime.ErrorCodeRateLimited,
			runtime.ErrorCodeContextExceeded, runtime.ErrorCodeRequestTooLarge, runtime.ErrorCodeMediaTooLarge,
			runtime.ErrorCodeToolFailed, runtime.ErrorCodeHookBlocked, runtime.ErrorCodeLoopDetected,
			runtime.ErrorCodeStructuredOutputFailed, runtime.ErrorCodeRoutingFailed:
			o.fatal = runtimePromptError(o.sessionID, e.Code, e.Error)
		}
	case *runtime.BudgetExceededEvent:
		o.fatal = runtimePromptError(o.sessionID, "budget_exceeded", e.Message)
	}
}

func (o *promptOutcome) result() (acp.StopReason, error) {
	if o.streamReason == "canceled" {
		return acp.StopReasonCancelled, nil
	}
	if o.fatal != nil {
		return "", o.fatal
	}
	switch o.streamReason {
	case runtime.StreamStopReasonMaxIterations:
		return acp.StopReasonMaxTurnRequests, nil
	case "error", "hook_blocked", "loop_detected", "budget_exceeded":
		return "", runtimePromptError(o.sessionID, o.streamReason, "agent execution stopped: "+o.streamReason)
	}
	switch o.finishReason {
	case chat.FinishReasonLength:
		return acp.StopReasonMaxTokens, nil
	case chat.FinishReasonRefusal:
		return acp.StopReasonRefusal, nil
	default:
		return acp.StopReasonEndTurn, nil
	}
}

func runtimePromptError(sid, code, message string) *acp.RequestError {
	return acp.NewInternalError(map[string]any{"sessionId": sid, "runtimeCode": code, "error": message})
}
