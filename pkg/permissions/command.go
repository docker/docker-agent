package permissions

import (
	"strings"

	"github.com/docker/docker-agent/pkg/safety"
)

// CommandAllowCoversCall checks strict grants: simple commands and literal word boundaries.
func CommandAllowCoversCall(toolName string, allowPatterns []string, args map[string]any) bool {
	cmd, ok := safety.CommandArg(args)
	if !ok || !isSimpleShellCommand(cmd) {
		return false
	}
	for _, pattern := range allowPatterns {
		if commandGrantMatches(toolName, pattern, cmd) {
			return true
		}
	}
	return false
}

// isSimpleShellCommand uses the same substitution guard as the classifier.
func isSimpleShellCommand(cmd string) bool {
	return !safety.ContainsShellMetacharacter(cmd)
}

// commandGrantMatches reports whether a single session allow pattern
// covers cmd for toolName under safety-override semantics. Recognized
// shapes:
//
//	"<tool>"                — whole-tool grant: covers any (simple) command
//	"<tool>:cmd=<literal>"  — exact-command grant
//	"<tool>:cmd=<literal>*" — word-prefix grant (the shape
//	                          toolconfirm.BuildPermissionPattern stores
//	                          for the interactive T decision): the
//	                          literal must match whole words, so
//	                          "mkdir*" covers "mkdir -p x" but not
//	                          "mkdiranything"
//
// Any other shape — glob metacharacters inside the literal, extra
// argument conditions (":cwd=..."), tool-name globs — has ambiguous
// word-level intent and is not honored for safety override.
// Matching is case-insensitive, consistent with the generic matcher.
func commandGrantMatches(toolName, pattern, cmd string) bool {
	if pattern == toolName {
		return true
	}
	cond, ok := strings.CutPrefix(pattern, toolName+":cmd=")
	if !ok {
		return false
	}
	literal, hadStar := strings.CutSuffix(cond, "*")
	// A ':' would introduce a further argument condition; glob or
	// escape characters make the word-level intent ambiguous.
	if strings.ContainsAny(literal, `*?[\:`) {
		return false
	}
	c := strings.ToLower(cmd)
	p := strings.ToLower(literal)
	if !hadStar {
		return c == p
	}
	rest, ok := strings.CutPrefix(c, p)
	if !ok {
		return false
	}
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

func commandAllowMatches(toolName, pattern string, args map[string]any) bool {
	if !safety.IsCommandTool(toolName) {
		return true
	}
	_, argPatterns := parsePattern(pattern)
	cmdPattern, ok := argPatterns["cmd"]
	if !ok {
		return true
	}
	literal, hasStar := strings.CutSuffix(cmdPattern, "*")
	if !hasStar || literal == "" || strings.ContainsAny(literal, `*?[\`) {
		return true
	}
	cmd, ok := safety.CommandArg(args)
	return ok && isSimpleShellCommand(cmd) &&
		commandGrantMatches(toolName, toolName+":cmd="+cmdPattern, cmd)
}
