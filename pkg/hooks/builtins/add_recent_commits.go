package builtins

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddRecentCommits is the registered name of the add_recent_commits builtin.
const AddRecentCommits = "add_recent_commits"

// defaultRecentCommitsLimit is the number of commits emitted when no
// explicit limit is supplied via args. Ten lines is enough to convey
// "what's been happening here recently" without flooding the prompt.
const defaultRecentCommitsLimit = 10

// addRecentCommits emits `git log --oneline -n N` as session_start
// additional context, giving the model a one-shot view of recent
// project activity.
//
// The first arg, when present and parseable as a positive integer,
// overrides the default limit ([defaultRecentCommitsLimit]). Invalid
// or non-positive values fall back to the default with a debug log
// rather than failing the hook.
//
// No-op when:
//   - Input.Cwd is empty;
//   - the directory isn't a git repo (git exits non-zero);
//   - git isn't installed;
//   - the repo has no commits yet.
func addRecentCommits(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" {
		return nil, nil
	}

	limit := defaultRecentCommitsLimit
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			limit = n
		} else {
			slog.DebugContext(ctx, "add_recent_commits: ignoring invalid limit arg", "arg", args[0], "error", err)
		}
	}

	out, err := gitOutput(ctx, in.Cwd, "log", "--oneline", "-n", strconv.Itoa(limit))
	if err != nil {
		slog.DebugContext(ctx, "add_recent_commits: git log failed; skipping", "cwd", in.Cwd, "error", err)
		return nil, nil
	}
	if out == "" {
		return nil, nil
	}
	return hooks.NewAdditionalContextOutput(hooks.EventSessionStart, "Recent commits:\n\n"+out), nil
}
