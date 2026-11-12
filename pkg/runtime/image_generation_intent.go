package runtime

import "regexp"

const missingGeneratedImageWarning = "The model returned text but no image for this image-generation request. Try rephrasing the request."

var imageGenerationIntentRE = regexp.MustCompile(
	`(?i)\b(?:re)?(?:generate|create|make|draw|render|produce)\s+(?:an?\s+|the\s+)?` +
		`(?:image|picture|photo|banner|logo|icon|graphic|drawing|illustration|thumbnail|sticker|avatar|gif)\b`,
)

// This phrase heuristic does not resolve negation or conversational context;
// the caller does not track capability or submission-wide continuations.
func hasExplicitImageGenerationIntent(prompt string) bool {
	return imageGenerationIntentRE.MatchString(prompt)
}
