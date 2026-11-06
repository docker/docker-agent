package modelinfo

import "strings"

// Claude5Model holds the request-policy differences between the Claude 5.x
// models. Every model in the table also uses adaptive thinking by default,
// rejects token budgets and sampling parameters, and accepts effort
// low/medium/high/xhigh/max.
//
// Source: the per-model matrix in https://github.com/docker/docker-agent/issues/4583.
type Claude5Model struct {
	// CanDisableThinking reports whether `thinking: disabled` is accepted.
	CanDisableThinking bool
	// ChecksThinkingPrefix reports whether the server verifies that replayed
	// thinking blocks still match their prefix.
	ChecksThinkingPrefix bool
	// BedrockMantleOnly reports that AWS serves the model only through
	// Bedrock Mantle, not the runtime/Converse API docker-agent uses.
	BedrockMantleOnly bool
	// NoServerSideFallback reports that the Claude API has no server-side
	// fallback for the model: `fallbacks: "default"` has no effect and a
	// list is a 400.
	NoServerSideFallback bool
}

var claude5Models = map[string]Claude5Model{
	"claude-haiku-5-5":  {CanDisableThinking: true, ChecksThinkingPrefix: true, NoServerSideFallback: true},
	"claude-opus-5":     {CanDisableThinking: true},
	"claude-opus-5-5":   {ChecksThinkingPrefix: true},
	"claude-sonnet-5":   {CanDisableThinking: true},
	"claude-sonnet-5-5": {ChecksThinkingPrefix: true},
	"claude-fable-5":    {},
	"claude-fable-5-1":  {ChecksThinkingPrefix: true},
	"claude-mythos-5":   {BedrockMantleOnly: true},
	"claude-mythos-5-1": {},
}

// LookupClaude5 returns the policy of a Claude 5.x model. It accepts bare,
// gateway-qualified ("vendor/claude-..."), Bedrock ("us.anthropic.claude-...-v1:0")
// and Vertex ("claude-...@date") spellings. Other IDs, including OpenAI-qualified
// ones, report false so callers keep their existing behavior.
func LookupClaude5(modelID string) (Claude5Model, bool) {
	m := normalize(modelID)
	if bare, ok := bedrockClaudeModelName(m); ok {
		m = bare
	}
	m, _, _ = strings.Cut(m, "@")
	m, _, _ = strings.Cut(m, ":")
	if i := strings.LastIndex(m, "-v"); i >= 0 {
		if _, w := leadingInt(m[i+2:]); w > 0 && i+2+w == len(m) {
			m = m[:i]
		}
	}
	model, ok := claude5Models[m]
	return model, ok
}
