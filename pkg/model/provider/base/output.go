package base

import (
	"fmt"
	"strings"
)

const MediaFileInstruction = `When you generate images, name each one with a marker line, placed alone on its own line, in this exact format:
[media-file: relative/path.ext]
Rules:
- Emit exactly one marker line per generated image, in the same order as the images.
- If the user asked for a specific file name or path, echo it in the marker exactly as requested.
- Otherwise choose a short, meaningful, kebab-case file name.
- Generate a single image unless the user explicitly asks for multiple images or variations.
- Never emit a marker line for an image you did not generate.`

type OutputIncompatibility string

const (
	OutputIncompatibleTools            OutputIncompatibility = "tools"
	OutputIncompatibleBuiltInTools     OutputIncompatibility = "built-in tools"
	OutputIncompatibleStructuredOutput OutputIncompatibility = "structured output"
)

// OutputRequestIncompatibleError carries only fixed categories, never request
// content or provider text, so callers can safely display it.
type OutputRequestIncompatibleError struct {
	Incompatibilities []OutputIncompatibility
}

func (e *OutputRequestIncompatibleError) Error() string {
	names := make([]string, len(e.Incompatibilities))
	for i, incompatibility := range e.Incompatibilities {
		names[i] = string(incompatibility)
	}
	return fmt.Sprintf(
		"this model is configured for image output (output_capabilities.image) and does not support %s in the same request; use a separate model or request for that combination",
		strings.Join(names, ", "),
	)
}
