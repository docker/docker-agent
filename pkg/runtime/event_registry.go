package runtime

import (
	"maps"
	"reflect"
)

// eventTypes maps each event's wire "type" discriminator to its struct type.
// Remote clients decode streamed events through it and the TypeScript SDK
// generator emits one type per entry, so every event must be registered.
var eventTypes = map[string]reflect.Type{
	"agent_choice":                 reflect.TypeFor[AgentChoiceEvent](),
	"agent_choice_reasoning":       reflect.TypeFor[AgentChoiceReasoningEvent](),
	"agent_choice_reasoning_reset": reflect.TypeFor[AgentChoiceReasoningResetEvent](),
	"agent_info":                   reflect.TypeFor[AgentInfoEvent](),
	"agent_route":                  reflect.TypeFor[AgentRouteEvent](),
	"agent_switching":              reflect.TypeFor[AgentSwitchingEvent](),
	"authorization_event":          reflect.TypeFor[AuthorizationEvent](),
	"background_agent_status":      reflect.TypeFor[BackgroundAgentStatusEvent](),
	"budget_exceeded":              reflect.TypeFor[BudgetExceededEvent](),
	"budget_usage":                 reflect.TypeFor[BudgetUsageEvent](),
	"elicitation_closed":           reflect.TypeFor[ElicitationClosedEvent](),
	"elicitation_request":          reflect.TypeFor[ElicitationRequestEvent](),
	"error":                        reflect.TypeFor[ErrorEvent](),
	"evaluation_usage":             reflect.TypeFor[EvaluationUsageEvent](),
	"hook_blocked":                 reflect.TypeFor[HookBlockedEvent](),
	"hook_finished":                reflect.TypeFor[HookFinishedEvent](),
	"hook_started":                 reflect.TypeFor[HookStartedEvent](),
	"max_iterations_reached":       reflect.TypeFor[MaxIterationsReachedEvent](),
	"mcp_init_finished":            reflect.TypeFor[MCPInitFinishedEvent](),
	"mcp_init_started":             reflect.TypeFor[MCPInitStartedEvent](),
	"message_added":                reflect.TypeFor[MessageAddedEvent](),
	"model_fallback":               reflect.TypeFor[ModelFallbackEvent](),
	"partial_tool_call":            reflect.TypeFor[PartialToolCallEvent](),
	"plan_changed":                 reflect.TypeFor[PlanChangedEvent](),
	"rag_indexing_completed":       reflect.TypeFor[RAGIndexingCompletedEvent](),
	"rag_indexing_progress":        reflect.TypeFor[RAGIndexingProgressEvent](),
	"rag_indexing_started":         reflect.TypeFor[RAGIndexingStartedEvent](),
	"routing_decision":             reflect.TypeFor[RoutingDecisionEvent](),
	"runtime_paused":               reflect.TypeFor[PausedEvent](),
	"session_compaction":           reflect.TypeFor[SessionCompactionEvent](),
	"session_recovered":            reflect.TypeFor[SessionRecoveredEvent](),
	"session_summary":              reflect.TypeFor[SessionSummaryEvent](),
	"session_title":                reflect.TypeFor[SessionTitleEvent](),
	"shell":                        reflect.TypeFor[ShellOutputEvent](),
	"stream_started":               reflect.TypeFor[StreamStartedEvent](),
	"stream_stopped":               reflect.TypeFor[StreamStoppedEvent](),
	"sub_session_completed":        reflect.TypeFor[SubSessionCompletedEvent](),
	"team_info":                    reflect.TypeFor[TeamInfoEvent](),
	"token_usage":                  reflect.TypeFor[TokenUsageEvent](),
	"tool_call":                    reflect.TypeFor[ToolCallEvent](),
	"tool_call_confirmation":       reflect.TypeFor[ToolCallConfirmationEvent](),
	"tool_call_output":             reflect.TypeFor[ToolCallOutputEvent](),
	"tool_call_response":           reflect.TypeFor[ToolCallResponseEvent](),
	"tool_calls_retracted":         reflect.TypeFor[ToolCallsRetractedEvent](),
	"toolset_info":                 reflect.TypeFor[ToolsetInfoEvent](),
	"user_message":                 reflect.TypeFor[UserMessageEvent](),
	"warning":                      reflect.TypeFor[WarningEvent](),
}

// EventTypes returns a copy of the event registry, keyed by the wire "type"
// discriminator.
func EventTypes() map[string]reflect.Type {
	return maps.Clone(eventTypes)
}

// NewEvent returns a zero event for a wire "type" discriminator, ready to be
// decoded into.
func NewEvent(eventType string) (Event, bool) {
	t, ok := eventTypes[eventType]
	if !ok {
		return nil, false
	}
	return reflect.New(t).Interface().(Event), true
}
