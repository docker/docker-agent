package modelinfo

import "strings"

// VertexModelsDevProvider maps a Model Garden publisher to its models.dev
// provider. Anthropic uses bare Claude names; other publishers keep their
// publisher-prefixed model names under google-vertex. This is catalogue
// identity only, not the API dispatch provider (google).
func VertexModelsDevProvider(publisher string) string {
	switch strings.ToLower(strings.TrimSpace(publisher)) {
	case "":
		return ""
	case "anthropic":
		return "anthropic"
	default:
		return "google-vertex"
	}
}
