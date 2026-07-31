package gemini

import "google.golang.org/genai"

// imageOutputMediaFileInstruction is appended as a system instruction on the
// explicit image-output gateway route (see wantsImageResponseModalities) so
// generated images arrive with a machine-readable filename: the runtime
// strips these exact marker lines from the reply and uses the paths to name
// the materialized workspace files (pkg/runtime/generated_media_markers.go).
// The single-image steering keeps one request yielding one predictably named
// file; every blob the model actually returns is still persisted.
const imageOutputMediaFileInstruction = `When you generate images, name each one with a marker line, placed alone on its own line, in this exact format:
[media-file: relative/path.ext]
Rules:
- Emit exactly one marker line per generated image, in the same order as the images.
- If the user asked for a specific file name or path, echo it in the marker exactly as requested.
- Otherwise choose a short, meaningful, kebab-case file name.
- Generate a single image unless the user explicitly asks for multiple images or variations.
- Never emit a marker line for an image you did not generate.`

// applyImageOutputMediaFileInstruction appends the marker-protocol
// instruction to the request's system instruction, preserving any parts
// already present. Callers gate it on wantsImageResponseModalities so only
// the explicit image-output gateway chat route ever carries it.
func applyImageOutputMediaFileInstruction(config *genai.GenerateContentConfig) {
	if config.SystemInstruction == nil {
		config.SystemInstruction = &genai.Content{}
	}
	config.SystemInstruction.Parts = append(config.SystemInstruction.Parts,
		genai.NewPartFromText(imageOutputMediaFileInstruction))
}
