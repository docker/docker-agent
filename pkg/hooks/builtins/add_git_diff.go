package builtins

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddGitDiff is the registered name of the add_git_diff builtin.
const AddGitDiff = "add_git_diff"

// addGitDiff emits the working-tree diff as turn_start additional
// context, refreshing every turn.
//
// By default it runs `git diff --stat` for a compact summary. Pass the
// single arg "full" to emit the full unified diff (`git diff`) instead.
// In either mode the captured output is capped by [maxGitOutputBytes],
// so a runaway change set can't silently blow up the prompt.
//
// No-op when:
//   - Input.Cwd is empty;
//   - the directory isn't a git repo (git exits non-zero);
//   - git isn't installed;
//   - the diff is empty (clean tree).
func addGitDiff(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" {
		return nil, nil
	}

	gitArgs := []string{"diff", "--stat"}
	header := "Current working-tree diff (stat):\n\n"
	if len(args) > 0 && args[0] == "full" {
		gitArgs = []string{"diff"}
		header = "Current working-tree diff:\n\n"
	}

	out, err := gitOutput(ctx, in.Cwd, gitArgs...)
	if err != nil {
		slog.DebugContext(ctx, "add_git_diff: git diff failed; skipping", "cwd", in.Cwd, "error", err)
		return nil, nil
	}
	if out == "" {
		return nil, nil
	}
	return hooks.NewAdditionalContextOutput(hooks.EventTurnStart, header+out), nil
}
