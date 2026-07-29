package gemini

import (
	"strings"

	"google.golang.org/genai"
)

// apiSurface* classifies which backend/transport a Client talks to. Used
// only for diagnostics; never derived from or containing request content.
const (
	apiSurfaceGeminiAPI = "gemini_api"
	apiSurfaceVertexAI  = "vertex_ai"
	apiSurfaceGateway   = "gateway"
)

// RequestShape is a safe, allowlisted summary of a Gemini
// GenerateContentConfig request. Every field is a boolean, count, or a value
// drawn from a small fixed enum (e.g. a modality name or tool kind) — never
// a prompt, tool description/schema, media payload, token, credential, or
// any other provider- or user-supplied free text. It is safe to log at
// Debug level and to include in diagnostics/bug reports.
type RequestShape struct {
	// ResponseModalitiesSet reports whether the request specified any
	// response modalities at all. ResponseModalities holds the normalized
	// (trimmed, upper-cased, de-duplicated) values, e.g. ["TEXT", "IMAGE"].
	ResponseModalitiesSet bool
	ResponseModalities    []string

	// BuiltInToolKinds lists which fixed-kind built-in tools (google_search,
	// google_maps, code_execution) were enabled. BuiltInToolCount is
	// len(BuiltInToolKinds).
	BuiltInToolKinds []string
	BuiltInToolCount int

	// FunctionToolCount is the number of caller-supplied (MCP/custom)
	// function tools converted for this request. Never their names,
	// descriptions, or parameter schemas.
	FunctionToolCount int

	// HasToolConfig, FunctionCallingMode, and ServerSideToolInvocation
	// describe genai.ToolConfig without exposing AllowedFunctionNames.
	HasToolConfig            bool
	FunctionCallingMode      string
	ServerSideToolInvocation bool

	// StructuredOutputPresent reports whether a structured-output response
	// (MIME type and/or schema) was requested, never the schema itself.
	StructuredOutputPresent bool

	// ThinkingConfigSet and NoThinkingRequested describe the thinking
	// configuration shape without exposing budgets or levels (already safe
	// enums/numbers, but kept out to keep this struct minimal).
	ThinkingConfigSet   bool
	NoThinkingRequested bool

	// APISurface classifies the backend/transport: "gemini_api", "vertex_ai",
	// or "gateway".
	APISurface string

	// OutputCapabilityKnown and OutputCapabilityEnabled report whether an
	// authoritative source for the model's image/media *output* capability
	// was consulted for this request. No such source exists yet — it is the
	// subject of a later step — so both fields are always false today,
	// deliberately reporting "unknown" rather than guessing from the model
	// ID string.
	OutputCapabilityKnown   bool
	OutputCapabilityEnabled bool
}

// newRequestShape captures a [RequestShape] from a fully-built
// genai.GenerateContentConfig (i.e. after tools/ToolConfig have been
// attached) and the client that built it.
func newRequestShape(c *Client, config *genai.GenerateContentConfig, functionToolCount int) RequestShape {
	modalities := normalizeResponseModalities(config.ResponseModalities)
	kinds := builtInToolKinds(config.Tools)

	shape := RequestShape{
		ResponseModalitiesSet:   len(modalities) > 0,
		ResponseModalities:      modalities,
		BuiltInToolKinds:        kinds,
		BuiltInToolCount:        len(kinds),
		FunctionToolCount:       functionToolCount,
		HasToolConfig:           config.ToolConfig != nil,
		StructuredOutputPresent: config.ResponseMIMEType != "" || config.ResponseSchema != nil || config.ResponseJsonSchema != nil,
		ThinkingConfigSet:       config.ThinkingConfig != nil,
		NoThinkingRequested:     c.ModelOptions.NoThinking(),
		APISurface:              c.apiSurface,
	}

	if config.ToolConfig != nil {
		if fc := config.ToolConfig.FunctionCallingConfig; fc != nil {
			shape.FunctionCallingMode = string(fc.Mode)
		}
		shape.ServerSideToolInvocation = config.ToolConfig.IncludeServerSideToolInvocations != nil &&
			*config.ToolConfig.IncludeServerSideToolInvocations
	}

	return shape
}

// normalizeResponseModalities trims, upper-cases, and de-duplicates raw
// modality strings, dropping empty entries.
func normalizeResponseModalities(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, m := range raw {
		norm := strings.ToUpper(strings.TrimSpace(m))
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	return out
}

// builtInToolKinds returns the fixed-kind names of built-in tools present in
// toolsList (e.g. "google_search"). The caller-supplied function-declarations
// tool (added separately by convertToolsToGemini) has none of these fields
// set, so it is naturally excluded.
func builtInToolKinds(toolsList []*genai.Tool) []string {
	var kinds []string
	for _, t := range toolsList {
		switch {
		case t.GoogleSearch != nil:
			kinds = append(kinds, "google_search")
		case t.GoogleMaps != nil:
			kinds = append(kinds, "google_maps")
		case t.CodeExecution != nil:
			kinds = append(kinds, "code_execution")
		}
	}
	return kinds
}

// LogAttrs renders the shape as a flat slog key/value list.
func (s RequestShape) LogAttrs() []any {
	return []any{
		"response_modalities_set", s.ResponseModalitiesSet,
		"response_modalities", s.ResponseModalities,
		"built_in_tool_kinds", s.BuiltInToolKinds,
		"built_in_tool_count", s.BuiltInToolCount,
		"function_tool_count", s.FunctionToolCount,
		"has_tool_config", s.HasToolConfig,
		"function_calling_mode", s.FunctionCallingMode,
		"server_side_tool_invocation", s.ServerSideToolInvocation,
		"structured_output_present", s.StructuredOutputPresent,
		"thinking_config_set", s.ThinkingConfigSet,
		"no_thinking_requested", s.NoThinkingRequested,
		"api_surface", s.APISurface,
		"output_capability_known", s.OutputCapabilityKnown,
		"output_capability_enabled", s.OutputCapabilityEnabled,
	}
}
