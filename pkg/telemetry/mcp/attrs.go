package mcp

// MCP attribute keys defined by the OTel semantic conventions
// (https://opentelemetry.io/docs/specs/semconv/registry/attributes/mcp/).
// All are Development stability.
const (
	AttrMethodName      = "mcp.method.name"
	AttrProtocolVersion = "mcp.protocol.version"
	AttrResourceURI     = "mcp.resource.uri"
	AttrSessionID       = "mcp.session.id"
)

// JSON-RPC attribute keys used alongside MCP spans for request id and
// response status when applicable.
const (
	AttrJSONRPCRequestID       = "jsonrpc.request.id"
	AttrJSONRPCProtocolVersion = "jsonrpc.protocol.version"
	AttrRPCResponseStatusCode  = "rpc.response.status_code"
)

// gen_ai.* attribute keys that the MCP semconv overlays on MCP spans when
// applicable. These are duplicated here as constants so the MCP package
// doesn't depend on the genai package — keeping the two telemetry helpers
// compositional.
const (
	AttrGenAIOperationName = "gen_ai.operation.name"
	AttrGenAIToolName      = "gen_ai.tool.name"
	AttrGenAIPromptName    = "gen_ai.prompt.name"
)

// Well-known MCP method names (https://modelcontextprotocol.io/specification).
// These match the values listed in the OTel semconv registry.
const (
	MethodInitialize         = "initialize"
	MethodPing               = "ping"
	MethodCompletionComplete = "completion/complete"
	MethodPromptsList        = "prompts/list"
	MethodPromptsGet         = "prompts/get"
	MethodResourcesList      = "resources/list"
	MethodResourcesRead      = "resources/read"
	MethodResourcesSubscribe = "resources/subscribe"
	MethodResourcesUnsub     = "resources/unsubscribe"
	MethodResourcesTemplates = "resources/templates/list"
	MethodRootsList          = "roots/list"
	MethodSamplingCreate     = "sampling/createMessage"
	MethodToolsList          = "tools/list"
	MethodToolsCall          = "tools/call"
	MethodLoggingSetLevel    = "logging/setLevel"
	MethodElicitationCreate  = "elicitation/create"
)

// OperationExecuteTool is the gen_ai.operation.name value used on MCP
// tools/call spans per the spec.
const OperationExecuteTool = "execute_tool"

// instrumentationName identifies this package as the OTel instrumentation
// scope for spans, metrics, and log records it produces.
const instrumentationName = "github.com/docker/docker-agent/pkg/telemetry/mcp"
