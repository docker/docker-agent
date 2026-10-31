// Package permissions provides tool permission checking based on configurable
// Allow/Ask/Deny patterns.
package permissions

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// Decision represents the permission decision for a tool call
type Decision int

const (
	// Ask means the tool requires user approval (default behavior)
	Ask Decision = iota
	// Allow means the tool is auto-approved without user confirmation
	Allow
	// Deny means the tool is rejected and should not be executed
	Deny
	// ForceAsk means an explicit ask pattern matched; the tool must be
	// confirmed even if it would normally be auto-approved (e.g. read-only).
	ForceAsk
)

// String returns a human-readable representation of the decision
func (d Decision) String() string {
	switch d {
	case Ask:
		return "ask"
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	case ForceAsk:
		return "force_ask"
	default:
		return "unknown"
	}
}

// Checker evaluates tool permissions based on configured patterns
type Checker struct {
	allowPatterns []string
	askPatterns   []string
	denyPatterns  []string
}

// NewChecker creates a new permission checker from config
func NewChecker(cfg *latest.PermissionsConfig) *Checker {
	if cfg == nil {
		return &Checker{}
	}
	return &Checker{
		allowPatterns: cfg.Allow,
		askPatterns:   cfg.Ask,
		denyPatterns:  cfg.Deny,
	}
}

// NewCheckerFromRules builds a Checker directly from allow/ask/deny
// pattern slices, for callers that hold the rules outside a
// [latest.PermissionsConfig] (e.g. session-scoped permissions whose
// config type lives in the session package). Semantics are identical
// to [NewChecker].
func NewCheckerFromRules(allow, ask, deny []string) *Checker {
	return &Checker{
		allowPatterns: allow,
		askPatterns:   ask,
		denyPatterns:  deny,
	}
}

// Check evaluates the permission for a given tool name without arguments.
// This is a convenience method that calls CheckWithArgs with nil arguments.
// Evaluation order: Deny (checked first), then Allow, then Ask (default)
func (c *Checker) Check(toolName string) Decision {
	return c.CheckWithArgs(toolName, nil)
}

// CheckWithArgs evaluates the permission for a given tool name and its arguments.
// Evaluation order: Deny (checked first), then Allow, then Ask (explicit), then Ask (default).
//
// The toolName can be a simple name like "shell" or a qualified name like
// "mcp:github:create_issue".
//
// Patterns support:
//   - Simple tool names: "shell", "read_*"
//   - Argument matching: "shell:cmd=ls*" matches shell tool with cmd argument starting with "ls"
//     For command-tool allow rules, literal trailing-* prefixes require a simple command and word boundary.
//   - Multiple arguments: "shell:cmd=ls*:cwd=/home/*" matches both conditions
//   - Glob patterns in both tool names and argument values
//
// Returns ForceAsk when an explicit ask pattern matches. ForceAsk means the
// tool must always be confirmed, even when it would normally be auto-approved
// (e.g. read-only tools). Note that --yolo mode takes precedence over ForceAsk.
func (c *Checker) CheckWithArgs(toolName string, args map[string]any) Decision {
	// Deny patterns are checked first - they take priority
	if matchAny(c.denyPatterns, toolName, args) {
		return Deny
	}

	// Allow patterns are checked second
	if matchAnyAllow(c.allowPatterns, toolName, args) {
		return Allow
	}

	// Explicit ask patterns override auto-approval (e.g. read-only hints)
	if matchAny(c.askPatterns, toolName, args) {
		return ForceAsk
	}

	// Default is Ask
	return Ask
}

// matchAny reports whether any pattern in the list matches the tool name and args.
func matchAny(patterns []string, toolName string, args map[string]any) bool {
	for _, pattern := range patterns {
		if matchToolPattern(pattern, toolName, args) {
			return true
		}
	}
	return false
}

// Merge returns a new Checker that combines the patterns from all provided
// checkers. Nil or empty checkers are skipped. The merged checker evaluates
// all deny patterns first, then all allow patterns, then all ask patterns.
func Merge(checkers ...*Checker) *Checker {
	var allow, ask, deny []string
	for _, c := range checkers {
		if c == nil || c.IsEmpty() {
			continue
		}
		allow = append(allow, c.allowPatterns...)
		ask = append(ask, c.askPatterns...)
		deny = append(deny, c.denyPatterns...)
	}
	return &Checker{allowPatterns: allow, askPatterns: ask, denyPatterns: deny}
}

// IsEmpty returns true if no permissions are configured
func (c *Checker) IsEmpty() bool {
	return len(c.allowPatterns) == 0 && len(c.askPatterns) == 0 && len(c.denyPatterns) == 0
}

// AllowPatterns returns the list of allow patterns.
func (c *Checker) AllowPatterns() []string {
	return c.allowPatterns
}

// AskPatterns returns the list of ask patterns.
func (c *Checker) AskPatterns() []string {
	return c.askPatterns
}

// DenyPatterns returns the list of deny patterns.
func (c *Checker) DenyPatterns() []string {
	return c.denyPatterns
}

// argCondition matches the ":key=" boundary that starts an argument condition.
// Tool names may contain colons ("mcp:github:create_issue") and so may values
// (URLs, Windows drive paths), so a bare ":" is not a separator: only a ":"
// immediately followed by "key=" is.
var argCondition = regexp.MustCompile(`:(\w+)=`)

// parsePattern parses a permission pattern into tool name pattern and argument conditions.
// Pattern format: "toolname" or "toolname:arg1=val1:arg2=val2"
// The tool name runs up to the first ":key=" boundary and each value up to the
// next one, so colons inside tool names and values are preserved.
func parsePattern(pattern string) (toolPattern string, argPatterns map[string]string) {
	argPatterns = make(map[string]string)

	// Split gives the tool name then one value per condition; the keys come
	// from the same non-overlapping matches, so they pair up one-to-one.
	values := argCondition.Split(pattern, -1)
	for i, m := range argCondition.FindAllStringSubmatch(pattern, -1) {
		argPatterns[m[1]] = values[i+1]
	}
	return values[0], argPatterns
}

// matchToolPattern checks if a tool name and its arguments match a pattern.
// The pattern can be:
// - Simple: "shell" - matches tool name only
// - With args: "shell:cmd=ls*" - matches tool name AND argument value
func matchToolPattern(pattern, toolName string, args map[string]any) bool {
	toolPattern, argPatterns := parsePattern(pattern)

	// First check if the tool name matches
	if !matchGlob(toolPattern, toolName) {
		return false
	}

	// If no argument patterns, we're done - tool name matched
	if len(argPatterns) == 0 {
		return true
	}

	// All argument patterns must match (indexing a nil args map is safe in Go)
	for argName, argPattern := range argPatterns {
		argValue, exists := args[argName]
		if !exists {
			return false
		}

		// Convert argument value to string for matching
		argStr := argToString(argValue)
		if !matchGlob(argPattern, argStr) {
			return false
		}
	}

	return true
}

// argToString converts an argument value to a string for pattern matching.
func argToString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		// JSON numbers are float64 - use %g for shortest representation
		return fmt.Sprintf("%g", val)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// matchGlob checks if a value matches a glob pattern.
// Supports glob-style patterns:
// - "*" matches any sequence of characters, path separators and newlines included
// - "?" matches any single character, path separator or newline included
// - "[...]" matches character classes
// - "\x" matches the literal character x
//
// Matching is case-insensitive.
//
// Note: filepath.Match's "*"/"?" stop at path separators ("/", or "\" on
// Windows), but argument values are shell commands, file paths and URLs: they
// routinely contain "/" and are often multi-line (heredocs, "&&" chains), so
// matching must not depend on either. We therefore translate the glob to an
// anchored regexp rather than using filepath.Match. Trailing wildcards keep a
// plain prefix-match fast path (e.g. "sudo*").
//
// One deliberate divergence from filepath.Match: "\" escapes the next character
// on every platform (filepath.Match treats it as a literal on Windows), and it
// does so both inside and outside character classes.
func matchGlob(pattern, value string) bool {
	// Normalize both to lowercase for case-insensitive matching
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)

	// Handle trailing wildcard for prefix matching
	// This allows "sudo*" to match "sudo rm -rf /"
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		// If prefix contains no other glob characters, do simple prefix match.
		// Including \ catches escaped asterisks (e.g. "foo\*").
		if !strings.ContainsAny(prefix, `*?[\`) {
			return strings.HasPrefix(value, prefix)
		}
	}

	// Full glob match (also handles exact matches).
	re, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// globCache memoizes compiled glob patterns. Patterns come from configuration
// and are stable for the lifetime of a Checker, so the cache is bounded by the
// number of configured rules.
var globCache sync.Map // map[string]*regexp.Regexp

// compileGlob returns the compiled form of pattern, translating it on first use.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if cached, ok := globCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return nil, err
	}
	globCache.Store(pattern, re)
	return re, nil
}

// globToRegexp translates a glob into an anchored regular expression whose "*"
// and "?" match any character, path separators and newlines included.
//
// Literal runs are quoted as whole substrings rather than byte by byte, so
// multi-byte UTF-8 runes survive the translation. Scanning bytes is safe
// regardless: every glob metacharacter is ASCII, and UTF-8 continuation bytes
// are always >= 0x80.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder

	// (?s) makes "." match "\n" as well. Without it the wildcards would stop at
	// a line break the same way filepath.Match's stopped at a path separator.
	b.WriteString(`(?s)\A`)

	lit := 0 // start of the literal run pending emission
	flush := func(end int) {
		if lit < end {
			b.WriteString(regexp.QuoteMeta(pattern[lit:end]))
		}
	}

	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			flush(i)
			b.WriteString(`.*`)
			i++
			lit = i
		case '?':
			flush(i)
			b.WriteByte('.')
			i++
			lit = i
		case '\\':
			if i+1 == len(pattern) {
				i++ // trailing "\": nothing to escape, keep it in the literal run
				continue
			}
			flush(i)
			// Drop the backslash and start a literal run at the escaped
			// character. Any UTF-8 continuation bytes fall through to default
			// and join that run.
			i += 2
			lit = i - 1
		case '[':
			end := classEnd(pattern, i)
			if end < 0 {
				i++ // unterminated class: "[" stays in the literal run
				continue
			}
			flush(i)
			writeClass(&b, pattern[i:end+1])
			i = end + 1
			lit = i
		default:
			i++
		}
	}
	flush(len(pattern))

	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

// writeClass translates a glob character class (brackets included) into a
// regexp character class. Glob "\x" escapes are resolved to the literal x and
// the remaining members are escaped, so regexp's Perl and POSIX classes cannot
// leak into glob syntax: "[\d]" stays the literal "d", as it was under
// filepath.Match. "-" is passed through so that ranges keep working.
func writeClass(b *strings.Builder, class string) {
	body := class[1 : len(class)-1]

	b.WriteByte('[')
	if strings.HasPrefix(body, "^") {
		b.WriteByte('^')
		body = body[1:]
	}
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\' && i+1 < len(body):
			i++
			writeClassByte(b, body[i])
		case c == '-':
			b.WriteByte('-') // range separator
		default:
			writeClassByte(b, c)
		}
	}
	b.WriteByte(']')
}

// writeClassByte writes one literal byte of a character class, escaping the
// characters regexp treats as special inside "[...]".
func writeClassByte(b *strings.Builder, c byte) {
	if strings.IndexByte(`\]^-[`, c) >= 0 {
		b.WriteByte('\\')
	}
	b.WriteByte(c)
}

// classEnd returns the index of the "]" closing the character class opened at
// start, or -1 if the class is unterminated. As with filepath.Match, a "]" in
// the first position closes an empty class rather than being a literal member;
// the class then fails to compile and, like ErrBadPattern, matches nothing.
func classEnd(pattern string, start int) int {
	for i := start + 1; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++ // skip the escaped character
		case ']':
			return i
		}
	}
	return -1
}

func matchAnyAllow(patterns []string, toolName string, args map[string]any) bool {
	for _, pattern := range patterns {
		if !matchToolPattern(pattern, toolName, args) {
			continue
		}
		if !commandAllowMatches(toolName, pattern, args) {
			continue
		}
		return true
	}
	return false
}
