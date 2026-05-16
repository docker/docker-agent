package runtime

import (
	"fmt"
	"log/slog"

	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools/builtin/rag"
)

// ragEventForwarder returns a callback that converts RAG manager events to runtime events.
func ragEventForwarder(ragName string, r *LocalRuntime, sendEvent func(Event)) rag.EventCallback {
	return func(ragEvent ragtypes.Event) {
		agentName := r.CurrentAgentName()
		slog.Debug("Forwarding RAG event", "type", ragEvent.Type, "rag", ragName, "agent", agentName)

		switch ragEvent.Type {
		case ragtypes.EventTypeIndexingStarted:
			sendEvent(RAGIndexingStarted(ragName, ragEvent.StrategyName))
		case ragtypes.EventTypeIndexingProgress:
			if ragEvent.Progress != nil {
				sendEvent(RAGIndexingProgress(ragName, ragEvent.StrategyName, ragEvent.Progress.Current, ragEvent.Progress.Total, agentName))
			}
		case ragtypes.EventTypeIndexingComplete:
			sendEvent(RAGIndexingCompleted(ragName, ragEvent.StrategyName))
		case ragtypes.EventTypeUsage:
			sendEvent(NewTokenUsageEvent("", agentName, &Usage{
				InputTokens:   ragEvent.TotalTokens,
				ContextLength: ragEvent.TotalTokens,
				Cost:          ragEvent.Cost,
			}))
		case ragtypes.EventTypeError:
			if ragEvent.Error != nil {
				sendEvent(Error(fmt.Sprintf("RAG %s error: %v", ragName, ragEvent.Error)))
			}
		default:
			slog.Debug("Unhandled RAG event type", "type", ragEvent.Type, "rag", ragName)
		}
	}
}
