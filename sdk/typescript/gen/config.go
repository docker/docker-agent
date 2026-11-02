package main

import (
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// config lists what the SDK exposes and how Go types that reflection alone
// gets wrong are described.
type config struct {
	// roots are the types served or accepted by the API; everything they
	// reference is generated too.
	roots []reflect.Type
	// events maps each event's "type" discriminator to its struct.
	events map[string]reflect.Type
	// modules maps a Go package to the generated file its types go to.
	modules map[string]string
	// names renames types whose Go names collide.
	names map[reflect.Type]string
	// enums are string types rendered as the union of their constants plus
	// the given extra values.
	enums map[reflect.Type][]string
	// overrides describe types with custom JSON encoding.
	overrides map[reflect.Type]func(*builder) (*jsonschema.Schema, error)
	// reflected are json.Marshaler implementations whose output matches
	// their fields.
	reflected map[reflect.Type]bool
}

const unionName = "AgentStreamingEvent"

func defaultConfig() *config {
	return &config{
		roots: []reflect.Type{
			reflect.TypeFor[api.AddMessageRequest](),
			reflect.TypeFor[api.AddSummaryRequest](),
			reflect.TypeFor[api.Agent](),
			reflect.TypeFor[api.BatchDeleteSessionsRequest](),
			reflect.TypeFor[api.BatchDeleteSessionsResponse](),
			reflect.TypeFor[api.BatchExportSessionsRequest](),
			reflect.TypeFor[api.BatchExportSessionsResponse](),
			reflect.TypeFor[api.CreateAgentConfigRequest](),
			reflect.TypeFor[api.CreateAgentConfigResponse](),
			reflect.TypeFor[api.CreateAgentRequest](),
			reflect.TypeFor[api.CreateAgentResponse](),
			reflect.TypeFor[api.DeleteAgentRequest](),
			reflect.TypeFor[api.DeleteAgentResponse](),
			reflect.TypeFor[api.DesktopTokenResponse](),
			reflect.TypeFor[api.EditAgentConfigRequest](),
			reflect.TypeFor[api.EditAgentConfigResponse](),
			reflect.TypeFor[api.ErrorResponse](),
			reflect.TypeFor[api.ExportAgentsResponse](),
			reflect.TypeFor[api.FollowUpResponse](),
			reflect.TypeFor[api.ForkSessionRequest](),
			reflect.TypeFor[api.HealthResponse](),
			reflect.TypeFor[api.ImportAgentRequest](),
			reflect.TypeFor[api.ImportAgentResponse](),
			reflect.TypeFor[api.Message](),
			reflect.TypeFor[api.PullAgentRequest](),
			reflect.TypeFor[api.PullAgentResponse](),
			reflect.TypeFor[api.PushAgentRequest](),
			reflect.TypeFor[api.PushAgentResponse](),
			reflect.TypeFor[api.QueueDepthResponse](),
			reflect.TypeFor[api.ReadyResponse](),
			reflect.TypeFor[api.ResumeElicitationRequest](),
			reflect.TypeFor[api.ResumeSessionRequest](),
			reflect.TypeFor[api.RunAgentRequest](),
			reflect.TypeFor[api.SessionResponse](),
			reflect.TypeFor[api.SessionSnapshotResponse](),
			reflect.TypeFor[api.SessionStatusResponse](),
			reflect.TypeFor[api.SessionsResponse](),
			reflect.TypeFor[api.SetSessionStarredRequest](),
			reflect.TypeFor[api.SteerSessionRequest](),
			reflect.TypeFor[api.UpdateMessageRequest](),
			reflect.TypeFor[api.UpdateSessionPermissionsRequest](),
			reflect.TypeFor[api.UpdateSessionSafetyPolicyRequest](),
			reflect.TypeFor[api.UpdateSessionTitleRequest](),
			reflect.TypeFor[api.UpdateSessionTitleResponse](),
			reflect.TypeFor[api.UpdateSessionTokensRequest](),
			// GET /api/agents/:id
			reflect.TypeFor[latest.Config](),
			// POST /api/sessions
			reflect.TypeFor[session.Session](),
			// GET /api/sessions/:id/models
			reflect.TypeFor[modelcatalog.SessionModelsResponse](),
		},
		events: runtime.EventTypes(),
		modules: map[string]string{
			"github.com/docker/docker-agent/pkg/api":                  "api",
			"github.com/docker/docker-agent/pkg/chat":                 "chat",
			"github.com/docker/docker-agent/pkg/config/latest":        "config",
			"github.com/docker/docker-agent/pkg/config/types":         "config",
			"github.com/docker/docker-agent/pkg/hooks":                "runtime",
			"github.com/docker/docker-agent/pkg/runtime":              "runtime",
			"github.com/docker/docker-agent/pkg/runtime/inspection":   "runtime",
			"github.com/docker/docker-agent/pkg/runtime/modelcatalog": "runtime",
			"github.com/docker/docker-agent/pkg/session":              "session",
			"github.com/docker/docker-agent/pkg/tools":                "tools",
			"github.com/modelcontextprotocol/go-sdk/mcp":              "tools",
		},
		names: map[reflect.Type]string{
			reflect.TypeFor[api.Message]():              "APIMessage",
			reflect.TypeFor[chat.Message]():             "ChatMessage",
			reflect.TypeFor[latest.PermissionsConfig](): "ConfigPermissions",
			reflect.TypeFor[runtime.Usage]():            "TokenUsage",
			reflect.TypeFor[hooks.EventType]():          "HookEventType",
			reflect.TypeFor[session.Error]():            "SessionError",
			reflect.TypeFor[session.Message]():          "SessionMessage",
		},
		enums: map[reflect.Type][]string{
			reflect.TypeFor[chat.ArtifactRootKind](): nil,
			reflect.TypeFor[chat.FinishReason]():     nil,
			reflect.TypeFor[chat.ImageURLDetail]():   nil,
			reflect.TypeFor[chat.MessagePartType]():  nil,
			reflect.TypeFor[chat.MessageRole]():      nil,
			reflect.TypeFor[hooks.EventType]():       nil,
			reflect.TypeFor[latest.SafetyMode]():     nil,
			// "" keeps a session on the legacy default and is a valid request.
			reflect.TypeFor[session.SafetyPolicy]():    {""},
			reflect.TypeFor[tools.ElicitationAction](): nil,
		},
		overrides: map[reflect.Type]func(*builder) (*jsonschema.Schema, error){
			reflect.TypeFor[latest.Duration]():          durationSchema,
			reflect.TypeFor[latest.InstructionFiles]():  instructionFilesSchema,
			reflect.TypeFor[latest.SkillsConfig]():      skillsConfigSchema,
			reflect.TypeFor[latest.ThinkingBudget]():    thinkingBudgetSchema,
			reflect.TypeFor[latest.TaskBudget]():        taskBudgetSchema,
			reflect.TypeFor[latest.RAGStrategyConfig](): ragStrategyConfigSchema,
		},
		reflected: map[reflect.Type]bool{
			reflect.TypeFor[session.Session](): true,
			reflect.TypeFor[tools.Tool]():      true,
		},
	}
}
