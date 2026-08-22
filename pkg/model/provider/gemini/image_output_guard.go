package gemini

import (
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// checkImageOutputRequestCompatibility rejects, before any provider dispatch,
// an incompatible request when image output is enabled by configuration or the
// models.dev catalogue.
func (c *Client) checkImageOutputRequestCompatibility(imageOutputEnabled bool, config *genai.GenerateContentConfig, builtInTools []*genai.Tool, requestTools int) error {
	if !imageOutputEnabled {
		return nil
	}

	var incompatibilities []base.OutputIncompatibility
	if requestTools > 0 {
		incompatibilities = append(incompatibilities, base.OutputIncompatibleTools)
	}
	if len(builtInTools) > 0 {
		incompatibilities = append(incompatibilities, base.OutputIncompatibleBuiltInTools)
	}
	if config.ResponseMIMEType != "" || config.ResponseJsonSchema != nil {
		incompatibilities = append(incompatibilities, base.OutputIncompatibleStructuredOutput)
	}
	if len(incompatibilities) == 0 {
		return nil
	}
	return &base.OutputRequestIncompatibleError{Incompatibilities: incompatibilities}
}
