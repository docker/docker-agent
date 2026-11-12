package runtime

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/promptfiles"
	"github.com/docker/docker-agent/pkg/session"
)

var errPromptFileRejected = errors.New("prompt-file instructions rejected or unavailable; prompt_file_guard stopped the turn")

func (r *LocalRuntime) hasPromptFileGuard(a *agent.Agent) bool {
	exec := r.hooksExec(a)
	return exec != nil && exec.Has(hooks.EventPromptFileGuard)
}

func (r *LocalRuntime) checkPromptFiles(ctx context.Context, sess *session.Session, a *agent.Agent, sources []session.InstructionSource, stable bool) error {
	if !r.hasPromptFileGuard(a) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, source := range sources {
		if source.Group != promptfiles.InstructionGroup {
			continue
		}
		if !source.Available {
			return errPromptFileRejected
		}
		if source.SetMarker {
			continue
		}
		content := strings.Join([]string{source.Key, source.Label, source.Content, source.ChangedContent, source.RemovedContent}, "\n\n")
		if err := r.checkPromptFile(ctx, sess, a, "loaded", source.Path, content); err != nil {
			return err
		}
	}
	if stable {
		// The session stream owns instruction writes; concurrent compaction only appends a summary.
		return r.checkStoredPromptFiles(ctx, sess, a, sess.InstructionContextSnapshot())
	}
	return nil
}

func (r *LocalRuntime) checkStoredPromptFiles(ctx context.Context, sess *session.Session, a *agent.Agent, state *session.InstructionContextState) error {
	if state == nil || !r.hasPromptFileGuard(a) {
		return nil
	}
	for _, values := range []map[string]session.InstructionValue{state.Initial, state.Current} {
		for _, key := range slices.Sorted(maps.Keys(values)) {
			value := values[key]
			if value.Group != promptfiles.InstructionGroup {
				continue
			}
			content := strings.Join([]string{key, value.Label, value.Content, value.RemovedContent}, "\n\n")
			if err := r.checkPromptFile(ctx, sess, a, "stored", value.Path, content); err != nil {
				return err
			}
		}
	}
	// Older updates merge several sources and carry no file provenance.
	for _, update := range state.Updates {
		if err := r.checkPromptFile(ctx, sess, a, "update", "", update.Content); err != nil {
			return err
		}
	}
	return nil
}

func (r *LocalRuntime) checkPromptFile(ctx context.Context, sess *session.Session, a *agent.Agent, source, path, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result := r.dispatchHook(ctx, a, hooks.EventPromptFileGuard, &hooks.Input{
		SessionID:  sess.ID,
		Source:     source,
		PromptFile: &hooks.PromptFile{Path: path, Content: content},
	}, nil)
	if err := ctx.Err(); err != nil {
		return err
	}
	if result == nil || !result.Allowed {
		return errPromptFileRejected
	}
	return nil
}
