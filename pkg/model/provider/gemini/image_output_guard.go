package gemini

import (
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// imageOutputIncompatibility names a fixed, safe request-feature class
// rejected by the image-output request guard. Values are display-safe:
// never provider text, tool names, schema contents, or prompts.
type imageOutputIncompatibility string

const (
	imageOutputIncompatibleTools            imageOutputIncompatibility = "tools"
	imageOutputIncompatibleBuiltInTools     imageOutputIncompatibility = "built-in tools"
	imageOutputIncompatibleStructuredOutput imageOutputIncompatibility = "structured output"
)

// ImageOutputRequestIncompatibleError is returned before any provider
// dispatch when a request to an image-output-capable model
// (output_capabilities.image: true) combines custom function tools (with
// their required ToolConfig), a built-in tool, or structured output. The
// request shape is intentionally unsupported until live direct Gemini API and
// Vertex AI verification proves it is accepted there; gateway probing has
// already shown opaque, empty-body HTTP 400 responses. Rejecting locally keeps
// the verified minimal request byte-for-byte and gives the caller a specific,
// actionable error instead.
type ImageOutputRequestIncompatibleError struct {
	// Incompatibilities is always non-empty. Its values are the fixed enum
	// above — never provider text, tool names/schemas, or prompt content.
	Incompatibilities []imageOutputIncompatibility
}

func (e *ImageOutputRequestIncompatibleError) Error() string {
	names := make([]string, len(e.Incompatibilities))
	for i, c := range e.Incompatibilities {
		names[i] = string(c)
	}
	return fmt.Sprintf(
		"this model is configured for image output (output_capabilities.image) and does not support %s in the same request; use a separate model or request for that combination",
		strings.Join(names, ", "),
	)
}

// checkImageOutputRequestCompatibility rejects, before any provider dispatch,
// an incompatible request when image output is enabled by configuration or the
// models.dev catalogue.
func (c *Client) checkImageOutputRequestCompatibility(imageOutputEnabled bool, config *genai.GenerateContentConfig, builtInTools []*genai.Tool, requestTools int) error {
	if !imageOutputEnabled {
		return nil
	}

	var incompatibilities []imageOutputIncompatibility
	if requestTools > 0 {
		incompatibilities = append(incompatibilities, imageOutputIncompatibleTools)
	}
	if len(builtInTools) > 0 {
		incompatibilities = append(incompatibilities, imageOutputIncompatibleBuiltInTools)
	}
	if config.ResponseMIMEType != "" || config.ResponseJsonSchema != nil {
		incompatibilities = append(incompatibilities, imageOutputIncompatibleStructuredOutput)
	}
	if len(incompatibilities) == 0 {
		return nil
	}
	return &ImageOutputRequestIncompatibleError{Incompatibilities: incompatibilities}
}
