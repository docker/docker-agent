// Package permissions provides tool permission checking based on configurable
// Allow/Ask/Deny patterns.
package permissions

import (
	"fmt"
	"path/filepath"
	"strings"

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
// - Simple tool names: "shell", "read_*"
// - Argument matching: "shell:cmd=ls*" matches shell tool with cmd argument starting with "ls"
// - Multiple arguments: "shell:cmd=ls*:cwd=/home/*" matches both conditions
// - Glob patterns in both tool names and argument values
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
	if matchAny(c.allowPatterns, toolName, args) {
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

// parsePattern parses a permission pattern into tool name pattern and argument conditions.
// Pattern format: "toolname" or "toolname:arg1=val1:arg2=val2"
// Returns the tool pattern and a map of argument patterns.
//
// The parser looks for the first `:key=value` segment to split tool name from arguments.
// This allows tool names with colons (like "mcp:github:create_issue") to work correctly.
func parsePattern(pattern string) (toolPattern string, argPatterns map[string]string) {
	argPatterns = make(map[string]string)

	// Find the first occurrence of :key=value pattern
	// We look for ":" followed by an identifier and "="
	parts := strings.Split(pattern, ":")
	toolParts := []string{parts[0]} // First part is always part of the tool name

	for _, part := range parts[1:] {
		// Check if this part looks like an argument pattern (contains =)
		if key, value, found := strings.Cut(part, "="); found && key != "" {
			// This is an argument pattern - this and all remaining parts are args
			argPatterns[key] = value
		} else if len(argPatterns) == 0 {
			// No = found and we haven't started args yet, so it's part of tool name
			toolParts = append(toolParts, part)
		}
		// If we've started collecting args but this part has no =, skip it
	}

	toolPattern = strings.Join(toolParts, ":")
	return toolPattern, argPatterns
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
// Supports glob-style patterns using filepath.Match semantics:
// - "*" matches any sequence of characters within a path segment
// - "?" matches any single character
// - "[...]" matches character classes
//
// Matching is case-insensitive.
//
// Note: filepath.Match's "*" stops at path separators, but for argument
// matching we want "*" to match any characters including spaces.
// We handle trailing wildcards specially to support patterns like "sudo*"
// matching "sudo rm -rf /".
func matchGlob(pattern, value string) bool {
	// Normalize both to lowercase for case-insensitive matching
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)

	// Handle trailing wildcard for prefix matching
	// This allows "sudo*" to match "sudo rm -rf /"
	if strings.HasSuffix(pattern, "*") {
		prefix := pattern[:len(pattern)-1]
		// If prefix contains no other glob characters, do simple prefix match.
		// Including \ catches escaped asterisks (e.g. "foo\*").
		if !strings.ContainsAny(prefix, `*?[\`) {
			return strings.HasPrefix(value, prefix)
		}
	}

	// Try glob pattern match (also handles exact matches)
	matched, err := filepath.Match(pattern, value)
	return err == nil && matched
}
