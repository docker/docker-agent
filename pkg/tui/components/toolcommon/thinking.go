package toolcommon

import "strconv"

// ThinkingDescription returns a human-readable description of a model's
// thinking-effort wire label, shared by the sidebar focus card and the
// agent-details dialog so both speak the same vocabulary:
//
//   - an effort level passes through verbatim (e.g. "high", "minimal")
//   - "adaptive" stays "adaptive"
//   - a decimal token budget becomes "<count> tokens" (e.g. "8.2K tokens")
//   - "off" stays "off"
//   - an empty label yields "" (the model has no thinking configuration)
func ThinkingDescription(label string) string {
	if isAllDigits(label) {
		n, _ := strconv.ParseInt(label, 10, 64)
		return FormatTokenCount(n) + " tokens"
	}
	return label
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
