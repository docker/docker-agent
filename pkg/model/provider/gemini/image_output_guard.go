package gemini

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/modelinfo"
)

// imageOutputIncompatibility names a fixed, safe request-feature class
// rejected by the image-output request guard. Values are display-safe:
// never provider text, tool names, schema contents, or prompts.
type imageOutputIncompatibility string

const (
	imageOutputIncompatibleTools            imageOutputIncompatibility = "tools"
	imageOutputIncompatibleStructuredOutput imageOutputIncompatibility = "structured output"
)

// ImageOutputRequestIncompatibleError is returned before any provider
// dispatch when a request to an image-output-capable model
// (output_capabilities.image: true) combines custom function tools (with
// their required ToolConfig) or structured output. Gemini server-side built-in
// tools remain allowed. Title generation and compaction are always text-only
// and bypass this guard.
type ImageOutputRequestIncompatibleError struct {
	// Incompatibilities is always non-empty. Its values are the fixed enum
	// above — never provider text, tool names/schemas, or prompt content.
	Incompatibilities []imageOutputIncompatibility
	ToolCallSupport   modelinfo.ToolCallSupport
}

func (e *ImageOutputRequestIncompatibleError) Error() string {
	if len(e.Incompatibilities) == 1 && e.Incompatibilities[0] == imageOutputIncompatibleTools {
		switch e.ToolCallSupport {
		case modelinfo.ToolCallUnsupported:
			return "this model does not support tool calls; use a tool-capable model or remove tools from the request"
		case modelinfo.ToolCallSupported:
			return "this model supports tool calls, but not while image output is enabled; use a separate model or request for that combination"
		}
	}

	names := make([]string, len(e.Incompatibilities))
	for i, c := range e.Incompatibilities {
		names[i] = string(c)
	}
	return fmt.Sprintf(
		"image output is enabled for this model (by output_capabilities.image or models.dev) and is incompatible with %s in the same request; use a separate model or request for that combination",
		strings.Join(names, ", "),
	)
}

// checkImageOutputRequestCompatibility rejects, before any provider dispatch,
// an incompatible request when image output is enabled by configuration or the
// models.dev catalogue.
func (c *Client) checkImageOutputRequestCompatibility(ctx context.Context, imageOutputEnabled bool, config *genai.GenerateContentConfig, requestTools int) error {
	if !imageOutputEnabled || c.ModelOptions.GeneratingTitle() || c.ModelOptions.Compacting() {
		return nil
	}

	var incompatibilities []imageOutputIncompatibility
	if requestTools > 0 {
		incompatibilities = append(incompatibilities, imageOutputIncompatibleTools)
	}
	if config.ResponseMIMEType != "" || config.ResponseJsonSchema != nil {
		incompatibilities = append(incompatibilities, imageOutputIncompatibleStructuredOutput)
	}
	if len(incompatibilities) == 0 {
		return nil
	}
	incompatible := &ImageOutputRequestIncompatibleError{Incompatibilities: incompatibilities}
	if len(incompatibilities) == 1 && incompatibilities[0] == imageOutputIncompatibleTools {
		incompatible.ToolCallSupport = c.ToolCallSupport(ctx)
	}
	return incompatible
}
