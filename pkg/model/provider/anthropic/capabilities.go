package anthropic

import (
	"strings"

	"github.com/docker/docker-agent/pkg/modelinfo"
)

// claudeModel is the family and version parsed from a Claude model id.
type claudeModel struct {
	family       string
	major, minor int
}

func (m claudeModel) atLeast(family string, major, minor int) bool {
	return m.family == family && (m.major > major || m.major == major && m.minor >= minor)
}

// parseClaudeModel reads "claude-<family>-<major>[-<minor>][-<date>]" out of
// bare, gateway-qualified ("vendor/claude-..."), Bedrock ("us.anthropic.claude-...")
// and Vertex ("claude-...@date") identifiers. Date stamps are wider than two
// digits and never read as a minor version.
func parseClaudeModel(id string) (claudeModel, bool) {
	m := strings.ToLower(strings.TrimSpace(id))
	m = m[strings.LastIndexByte(m, '/')+1:]
	if i := strings.Index(m, "anthropic."); i >= 0 {
		m = m[i+len("anthropic."):]
	}
	m, _, _ = strings.Cut(m, "@")
	rest, ok := strings.CutPrefix(m, "claude-")
	if !ok {
		return claudeModel{}, false
	}
	family, rest, _ := strings.Cut(rest, "-")
	major, w := leadingDigits(rest)
	if w == 0 || w > 2 {
		return claudeModel{}, false
	}
	parsed := claudeModel{family: family, major: major}
	if rest = rest[w:]; rest != "" && (rest[0] == '-' || rest[0] == '.') {
		if n, mw := leadingDigits(rest[1:]); mw > 0 && mw <= 2 {
			parsed.minor = n
		}
	}
	return parsed, true
}

func leadingDigits(s string) (n, width int) {
	for width < len(s) && s[width] >= '0' && s[width] <= '9' {
		n = n*10 + int(s[width]-'0')
		width++
	}
	return n, width
}

func usesDefaultThinking(model string) bool {
	if modelinfo.IsClaudeHaiku55(model) {
		return true
	}
	m, ok := parseClaudeModel(model)
	return ok && (m.atLeast("opus", 5, 0) || m.atLeast("sonnet", 5, 0) || m.family == "fable" || m.family == "mythos")
}

func requiresThinking(model string) bool {
	m, ok := parseClaudeModel(model)
	return ok && (m.family == "fable" || m.family == "mythos")
}

func checksThinkingPrefix(model string) bool {
	if modelinfo.IsClaudeHaiku55(model) {
		return true
	}
	m, ok := parseClaudeModel(model)
	return ok && (m.atLeast("fable", 5, 1) || m.atLeast("mythos", 5, 1))
}

func rejectsSampling(model string) bool {
	if modelinfo.IsClaudeHaiku55(model) {
		return true
	}
	m, ok := parseClaudeModel(model)
	return ok && (m.atLeast("opus", 4, 7) || m.atLeast("sonnet", 5, 0) || m.family == "fable" || m.family == "mythos")
}

func supportsProgressUpdates(model string) bool {
	m, ok := parseClaudeModel(model)
	return ok && (m.atLeast("fable", 5, 0) || m.atLeast("mythos", 5, 1))
}
