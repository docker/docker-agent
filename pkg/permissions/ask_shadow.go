package permissions

import (
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// Shadow reports an ask rule that a matching allow rule silences: the checker
// evaluates allow before ask, so the ask rule never prompts for any call the
// allow rule covers.
type Shadow struct {
	Ask   string
	Allow string
}

// ShadowedAskPatterns returns the ask rules whose every matching call is also
// matched by an allow rule, so the ask rule can never prompt. Detection is
// conservative: it only reports a shadow when coverage is provable with the
// glob vocabulary the checker uses —
//
//   - an unconditional allow pattern ("*") covers every ask rule;
//   - otherwise the allow rule must carry no argument conditions and its tool
//     glob must match the ask rule's tool name (an ask tool that is itself a
//     glob is skipped: its match set cannot be proven covered);
//   - argument-scoped allow rules are never reported: matchAnyAllow routes
//     command patterns through commandAllowMatches, which enforces a
//     command-word boundary for literal trailing-star prefixes that the ask
//     matcher does not, so such an ask can still prompt for calls the allow
//     does not cover (e.g. "shell:cmd=ls*" allows ls, not lsof).
func ShadowedAskPatterns(cfg *latest.PermissionsConfig) []Shadow {
	if cfg == nil || len(cfg.Ask) == 0 || len(cfg.Allow) == 0 {
		return nil
	}

	var shadows []Shadow
	for _, ask := range cfg.Ask {
		askTool, _ := parsePattern(ask)
		askIsGlob := strings.ContainsAny(askTool, `*?[\`)
		for _, allow := range cfg.Allow {
			allowTool, allowArgs := parsePattern(allow)
			if len(allowArgs) > 0 {
				continue
			}
			if allowTool == "*" || (!askIsGlob && matchGlob(allowTool, askTool)) {
				shadows = append(shadows, Shadow{Ask: ask, Allow: allow})
				break
			}
		}
	}
	return shadows
}
