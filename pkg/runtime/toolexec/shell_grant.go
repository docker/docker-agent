package toolexec

// This file hardens the session-permissions override of preempt-yolo
// safety verdicts (see [call.sessionPermissionsAllow]) for the command
// tools — shell and run_background_job (see [safety.IsCommandTool]).
//
// The generic permissions matcher treats a trailing-* pattern as a
// plain prefix match over the whole command string, so the interactive
// "T = always allow" grant for `mkdir foo` — stored as
// "shell:cmd=mkdir*" — would also cover "mkdir x && rm -rf ~",
// "mkdir; rm -rf ~" and "mkdiranything". That laxity is acceptable
// when the pattern merely skips a confirmation the user opted out of,
// but not when it silences an explicit safety Ask from the preempt-yolo
// lane (possibly a high-blast-radius destructive verdict). Overriding
// a safety verdict therefore demands the strict reading implemented
// here:
//
//   - the command must be a single simple invocation — no shell
//     metacharacters that chain (;, &, |, newlines), substitute
//     ($(...), `...`, zsh =(...) or fish (...)), or redirect (>, <);
//   - a "<tool>:cmd=<literal>*" grant must match at a word boundary:
//     "mkdir*" covers "mkdir" and "mkdir -p x" but not "mkdiranything".
//
// Only grant shapes whose word-level intent is unambiguous are honored;
// any other shape falls back to the confirmation prompt. A rejected
// override is never destructive — the user is simply asked again.

import (
	"strings"

	"github.com/docker/docker-agent/pkg/safety"
)

// commandGrantCoversCall reports whether one of the session-level
// allow patterns covers the command tool call's command under the
// strict safety-override reading described in the file comment.
// toolName is the command tool being invoked; args is the parsed tool
// input (see ParseToolInput).
func commandGrantCoversCall(toolName string, allowPatterns []string, args map[string]any) bool {
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
