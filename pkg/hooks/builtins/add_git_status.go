package builtins

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddGitStatus is the registered name of the add_git_status builtin.
const AddGitStatus = "add_git_status"

// addGitStatus emits `git status --short --branch` as turn_start
// additional context, refreshing the model's view of uncommitted
// changes every turn.
//
// No-op when:
//   - Input.Cwd is empty;
//   - the directory isn't a git repo (git exits non-zero);
//   - git isn't installed.
//
// Failures are logged at debug and surfaced as a nil Output so an
// agent configured to receive git status doesn't get the run aborted
// because the user happened to `cd` outside a repo.
func addGitStatus(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" {
		return nil, nil
	}
	out, err := gitOutput(ctx, in.Cwd, "status", "--short", "--branch")
	if err != nil {
		slog.DebugContext(ctx, "add_git_status: git status failed; skipping", "cwd", in.Cwd, "error", err)
		return nil, nil
	}
	if out == "" {
		return nil, nil
	}
	return hooks.NewAdditionalContextOutput(hooks.EventTurnStart, "Current git status:\n\n"+out), nil
}
