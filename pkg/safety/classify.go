package safety

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

//go:embed safety_patterns.json
var safetyPatternsJSON []byte

// ClassifyCommand classifies a shell command against the embedded
// taxonomy of destructive and safe patterns.
//
//   - A destructive pattern matching anywhere in the command wins
//     (worst blast radius across matches).
//   - Otherwise a safe pattern must match the WHOLE command; compound
//     shell (a && b, a; b, a | b) never matches the safe list so
//     `ls && rm -rf ~` doesn't inherit `ls`'s safe verdict.
//   - Everything else — including an empty command or a taxonomy load
//     failure — is [ClassUnknown]. A classifier failure must degrade,
//     never block: gating is the safety mode's job.
func ClassifyCommand(command string) Label {
	patterns, err := loadSafetyPatterns()
	if err != nil {
		return Label{
			Class:       ClassUnknown,
			Origin:      OriginClassifier,
			BlastRadius: "unknown",
			Reason:      "Safety pattern load failed: " + err.Error(),
		}
	}

	if command != "" {
		if match := bestDestructiveMatch(command, patterns.destructive); match != nil {
			return Label{
				Class:       ClassDestructive,
				Origin:      OriginClassifier,
				BlastRadius: match.BlastRadius,
				Category:    match.Category,
				Reason:      "Command matches destructive operation: " + match.Pattern,
			}
		}
		if match := bestSafeMatch(command, patterns.safe); match != nil {
			return Label{
				Class:       ClassSafe,
				Origin:      OriginClassifier,
				BlastRadius: "safe",
				Category:    match.Category,
				Reason:      "Command matches safe read-only pattern: " + match.Pattern,
			}
		}
	}
	return Label{
		Class:       ClassUnknown,
		Origin:      OriginClassifier,
		BlastRadius: "unknown",
		Reason:      "Shell command is not positively recognised by the safety classifier.",
	}
}

type destructivePattern struct {
	Pattern     string
	BlastRadius string
	Category    string
	regexp      *regexp.Regexp
}

type safePattern struct {
	Pattern   string
	Category  string
	DenyFlags []string
	regexp    *regexp.Regexp
}

type destructivePatternEntry struct {
	Pattern     string `json:"pattern"`
	BlastRadius string `json:"blast_radius"`
	Category    string `json:"category"`
}

type safePatternEntry struct {
	Pattern   string   `json:"pattern"`
	Category  string   `json:"category"`
	DenyFlags []string `json:"deny_flags"`
}

type compiledPatterns struct {
	destructive []destructivePattern
	safe        []safePattern
}

var loadSafetyPatterns = sync.OnceValues(func() (compiledPatterns, error) {
	var root map[string]any
	if err := json.Unmarshal(safetyPatternsJSON, &root); err != nil {
		return compiledPatterns{}, fmt.Errorf("parse shell safety patterns: %w", err)
	}

	destructive, err := compileDestructive(root["destructive"])
	if err != nil {
		return compiledPatterns{}, err
	}
	safe, err := compileSafe(root["safe"])
	if err != nil {
		return compiledPatterns{}, err
	}
	return compiledPatterns{destructive: destructive, safe: safe}, nil
})

func compileDestructive(value any) ([]destructivePattern, error) {
	entries := collectDestructiveEntries(value)
	out := make([]destructivePattern, 0, len(entries))
	for _, entry := range entries {
		pattern := normalizeCommand(entry.Pattern)
		re, err := regexp.Compile(patternToRegexp(pattern))
		if err != nil {
			return nil, fmt.Errorf("compile destructive pattern %q: %w", entry.Pattern, err)
		}
		out = append(out, destructivePattern{
			Pattern:     entry.Pattern,
			BlastRadius: normalizeBlastRadius(entry.BlastRadius),
			Category:    entry.Category,
			regexp:      re,
		})
	}
	return out, nil
}

func compileSafe(value any) ([]safePattern, error) {
	entries := collectSafeEntries(value)
	out := make([]safePattern, 0, len(entries))
	for _, entry := range entries {
		pattern := normalizeCommand(entry.Pattern)
		re, err := regexp.Compile(patternToSafeRegexp(pattern))
		if err != nil {
			return nil, fmt.Errorf("compile safe pattern %q: %w", entry.Pattern, err)
		}
		out = append(out, safePattern{
			Pattern:   entry.Pattern,
			Category:  entry.Category,
			DenyFlags: entry.DenyFlags,
			regexp:    re,
		})
	}
	return out, nil
}

func bestDestructiveMatch(command string, patterns []destructivePattern) *destructivePattern {
	normalized := normalizeCommand(command)
	var best *destructivePattern
	bestSeverity := 0
	for i := range patterns {
		if !patterns[i].regexp.MatchString(normalized) {
			continue
		}
		severity := blastRadiusSeverity(patterns[i].BlastRadius)
		if severity <= bestSeverity {
			continue
		}
		bestSeverity = severity
		best = &patterns[i]
	}
	return best
}

// bestSafeMatch returns the first matching safe-list pattern, or nil.
// Refuses to vouch for any command carrying shell metacharacters so
// `ls && rm -rf ~` or `grep foo|rm -rf ~` never inherit a safe verdict
// from their first segment.
func bestSafeMatch(command string, patterns []safePattern) *safePattern {
	normalized := normalizeCommand(command)
	if containsShellMetacharacter(command) {
		return nil
	}
	for i := range patterns {
		if patterns[i].regexp.MatchString(normalized) && !carriesDenyFlag(normalized, patterns[i].DenyFlags) {
			return &patterns[i]
		}
	}
	return nil
}

// carriesDenyFlag reports whether the command uses one of the pattern's
// deny-listed flags — exec/write escape hatches inside otherwise
// read-only commands (`rg --pre <cmd>`, `git log --output=<file>`) that
// a trailing-wildcard pattern would otherwise vouch for. Tokens are
// quote-trimmed so `"--pre=cmd"` is caught too; a false positive only
// costs a confirmation prompt.
func carriesDenyFlag(normalized string, flags []string) bool {
	if len(flags) == 0 {
		return false
	}
	for tok := range strings.FieldsSeq(normalized) {
		tok = strings.Trim(tok, `"'`)
		for _, flag := range flags {
			if tok == flag || strings.HasPrefix(tok, flag+"=") {
				return true
			}
		}
	}
	return false
}

// containsShellMetacharacter returns true when the command contains a
// character that can chain (`;`, `&`), pipe (`|`), redirect (`<`, `>`),
// or substitute (backticks, `$(`) commands — with or without
// surrounding whitespace, so `grep foo|rm -rf /` is caught just like
// `grep foo | rm -rf /`. The safe list must never vouch for such a
// string: a safe-looking prefix says nothing about what the rest does,
// and trailing-wildcard patterns (`grep ...`) would otherwise cover the
// injected tail. Erring toward "not safe" only costs a confirmation
// prompt. Deliberately the same strictness as the runtime's
// session-grant check for shell commands.
func containsShellMetacharacter(command string) bool {
	return strings.ContainsAny(command, ";&|<>`\n") || strings.Contains(command, "$(")
}

// collectDestructiveEntries walks the JSON destructive section. The
// shape is map[category-name][]entry where each entry has pattern +
// blast_radius (+ optional category override).
func collectDestructiveEntries(value any) []destructivePatternEntry {
	switch v := value.(type) {
	case []any:
		var entries []destructivePatternEntry
		for _, item := range v {
			entries = append(entries, collectDestructiveEntries(item)...)
		}
		return entries
	case map[string]any:
		if pattern, ok := v["pattern"].(string); ok {
			if blastRadius, ok := v["blast_radius"].(string); ok {
				category, _ := v["category"].(string)
				return []destructivePatternEntry{{Pattern: pattern, BlastRadius: blastRadius, Category: category}}
			}
		}
		var entries []destructivePatternEntry
		for _, item := range v {
			entries = append(entries, collectDestructiveEntries(item)...)
		}
		return entries
	default:
		return nil
	}
}

// collectSafeEntries walks the JSON safe section. Shape is the same
// as destructive minus the blast_radius field — entries that look
// destructive (carry a blast_radius) are ignored here so a stray
// destructive entry in the safe section can't accidentally allow a
// dangerous command through.
func collectSafeEntries(value any) []safePatternEntry {
	switch v := value.(type) {
	case []any:
		var entries []safePatternEntry
		for _, item := range v {
			entries = append(entries, collectSafeEntries(item)...)
		}
		return entries
	case map[string]any:
		if pattern, ok := v["pattern"].(string); ok {
			if _, hasBlast := v["blast_radius"]; !hasBlast {
				category, _ := v["category"].(string)
				return []safePatternEntry{{Pattern: pattern, Category: category, DenyFlags: stringSlice(v["deny_flags"])}}
			}
			// A destructive entry (pattern + blast_radius) in the safe
			// section is rejected wholesale: do not recurse into its
			// values, or nested children could be harvested into the
			// safe list.
			return nil
		}
		var entries []safePatternEntry
		for _, item := range v {
			entries = append(entries, collectSafeEntries(item)...)
		}
		return entries
	default:
		return nil
	}
}

// stringSlice converts a JSON []any into []string, dropping non-strings.
func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// patternToRegexp converts a destructive pattern into a regex that
// matches anywhere in the normalised command. Destructive intent is
// the priority — a destructive pattern hidden inside a larger
// command (e.g. `cd /tmp && rm -rf foo`) should still match.
func patternToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString(`(?i)(?:^|.*\b)`)
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '<':
			if end := strings.IndexByte(pattern[i:], '>'); end >= 0 {
				b.WriteString(`\S+`)
				i += end + 1
				continue
			}
		case '.':
			if strings.HasPrefix(pattern[i:], "...") {
				b.WriteString(`.*`)
				i += len("...")
				continue
			}
		}
		b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		i++
	}
	b.WriteString(`(?:$|\b.*)`)
	return b.String()
}

// patternToSafeRegexp anchors the safe pattern to the start AND end
// of the command. Safe matching must be strict: `ls -la` should match
// the safe pattern `ls -<flags>`, but `ls -la && rm -rf /` must not
// (the compound shell check upstream already blocks this, but
// anchoring is a belt-and-braces second line of defence).
func patternToSafeRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString(`(?i)^`)
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '<':
			if end := strings.IndexByte(pattern[i:], '>'); end >= 0 {
				b.WriteString(`\S+`)
				i += end + 1
				continue
			}
		case '.':
			if strings.HasPrefix(pattern[i:], "...") {
				b.WriteString(`.*`)
				i += len("...")
				continue
			}
		}
		b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		i++
	}
	b.WriteString(`$`)
	return b.String()
}

// normalizeBlastRadius collapses the JSON taxonomy's hyphenated
// levels onto the four canonical strings the label schema carries.
func normalizeBlastRadius(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "LOW":
		return "low"
	case "MEDIUM", "LOW-MEDIUM":
		return "medium"
	case "HIGH", "MEDIUM-HIGH":
		return "high"
	default:
		return "unknown"
	}
}

// blastRadiusSeverity ranks the wire-format blast-radius strings so
// [bestDestructiveMatch] can pick the worst match across patterns.
// "unknown" outranks "medium" by design: when a pattern can't be
// classified precisely but is flagged for safety, that's more
// dangerous than a confidently-medium hit.
func blastRadiusSeverity(level string) int {
	switch level {
	case "high":
		return 4
	case "unknown":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func normalizeCommand(command string) string {
	return strings.Join(strings.Fields(strings.ToLower(command)), " ")
}
