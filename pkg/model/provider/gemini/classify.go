package gemini

import "strings"

// RequestRejectionCategory buckets a Gemini request rejection into one of a
// small, fixed set of causes so diagnostics and error messages can stay
// specific without echoing the provider's raw message or response body.
// Categories are derived only from Google's own public, documented API
// field/parameter names appearing in [genai.APIError.Message] — never from
// request content, tool schemas, prompts, or anything else local to this
// process.
type RequestRejectionCategory string

const (
	// RejectionMissingResponseModalities: the request didn't declare a
	// response modality (e.g. IMAGE) that the model needed for this reply.
	RejectionMissingResponseModalities RequestRejectionCategory = "missing_response_modalities"
	// RejectionIncompatibleFunctionOrBuiltinTools: the combination of
	// custom function tools and/or built-in tools (Search, Maps, Code
	// Execution) enabled for the request isn't supported together.
	RejectionIncompatibleFunctionOrBuiltinTools RequestRejectionCategory = "incompatible_function_or_builtin_tools"
	// RejectionIncompatibleToolConfig: the ToolConfig/function-calling mode
	// isn't supported for this request or model.
	RejectionIncompatibleToolConfig RequestRejectionCategory = "incompatible_tool_config"
	// RejectionStructuredOutputConflict: a structured-output option
	// (response MIME type/schema) conflicts with another request option.
	RejectionStructuredOutputConflict RequestRejectionCategory = "structured_output_conflict"
	// RejectionModelOrAPICapabilityMismatch: the request used a feature
	// (e.g. thinking configuration) the target model or API surface
	// doesn't support at all.
	RejectionModelOrAPICapabilityMismatch RequestRejectionCategory = "model_or_api_capability_mismatch"
	// RejectionOther covers any other provider rejection, including ones
	// with no message to classify against (Gemini sometimes returns a 400
	// with an empty body).
	RejectionOther RequestRejectionCategory = "other_provider_rejection"
)

// rejectionKeywords maps each category to lowercase substrings of Gemini's
// own documented request field/parameter names that indicate it. Checked
// top-to-bottom; the first match wins.
var rejectionKeywords = []struct {
	category RequestRejectionCategory
	keywords []string
}{
	{RejectionMissingResponseModalities, []string{"response_modalities", "responsemodalities"}},
	{RejectionStructuredOutputConflict, []string{
		"response_schema", "response_json_schema", "response_mime_type",
		"responseschema", "responsejsonschema", "responsemimetype",
	}},
	{RejectionIncompatibleToolConfig, []string{
		"tool_config", "toolconfig", "function_calling_config", "functioncallingconfig",
	}},
	{RejectionIncompatibleFunctionOrBuiltinTools, []string{
		"function_declarations", "functiondeclarations",
		"google_search", "google_maps", "code_execution",
		"built-in tool", "builtin tool",
	}},
	{RejectionModelOrAPICapabilityMismatch, []string{
		"thinking_config", "thinkingconfig",
		"not supported for", "not supported by", "does not support", "is not enabled for",
	}},
}

// classifyByMessage returns the category matching a bounded set of known
// Gemini field/parameter keywords found in message, and true when a match
// was found. It returns (RejectionOther, false) when message is empty or
// matches nothing — a common shape for Gemini 400s with no JSON body.
func classifyByMessage(message string) (RequestRejectionCategory, bool) {
	if message == "" {
		return RejectionOther, false
	}
	lower := strings.ToLower(message)
	for _, entry := range rejectionKeywords {
		for _, kw := range entry.keywords {
			if strings.Contains(lower, kw) {
				return entry.category, true
			}
		}
	}
	return RejectionOther, false
}
