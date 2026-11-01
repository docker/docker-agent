package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/worktree"
)

type backgroundWorktree struct {
	tree         *worktree.Worktree
	runtime      *LocalRuntime
	instructions string
}

// worktreePrompt is inheritable context, not ownership of an execution runtime.
type worktreePrompt struct {
	setupContext          string
	isolationInstructions string
}

func (w *backgroundWorktree) prompt() worktreePrompt {
	wt := w.tree
	return worktreePrompt{setupContext: w.instructions, isolationInstructions: fmt.Sprintf("You are working in an isolated git worktree at %s on branch %s, based on commit %s. Resolve repository-relative files inside this checkout. Translate repository paths from the original checkout at %s to this checkout; do not treat source-checkout paths in the task as edit targets. Attached files are read-only inputs at their original paths. Do not edit or merge into the original checkout. Commit your completed changes on this branch, following the project signing policy, and report the branch and commit IDs to the calling agent. The calling agent will review and merge your changes. Uncommitted changes from the original checkout are not present.", wt.Dir, wt.Branch, wt.BaseCommit, wt.SourceDir)}
}

func (r *LocalRuntime) prepareBackgroundWorktree(ctx context.Context, params agenttool.RunParams) (*backgroundWorktree, error) {
	ctx = tools.WithoutInteractivePrompts(ctx)
	target, err := r.team.Agent(params.AgentName)
	if err != nil {
		return nil, err
	}
	if target.HasHarness() {
		return nil, fmt.Errorf("external harness agent %q does not support isolated background worktrees", params.AgentName)
	}
	var lookup session.Lookup
	if r.sessionStore != nil {
		lookup = r.sessionStore.GetSession
	}
	dir, err := session.ResolveWorkingDir(ctx, params.ParentSession, lookup)
	if err != nil {
		return nil, fmt.Errorf("creating background worktree: %w", err)
	}
	wt, err := worktree.Create(ctx, dir, "agent-"+uuid.NewV4().String(), worktree.WithRoot(r.worktreeRoot))
	if err != nil {
		return nil, fmt.Errorf("creating background worktree: %w", err)
	}
	if params.OnWorktree != nil {
		params.OnWorktree(wt)
	}
	// Keep the checkout even on setup failure: hooks may already have written files.
	fail := func(err error) (*backgroundWorktree, error) {
		return nil, fmt.Errorf("background worktree retained at %s (branch %s): %w", wt.Dir, wt.Branch, err)
	}
	var instructions string
	if executor := r.hooksExec(target); executor != nil && executor.Has(hooks.EventWorktreeCreate) {
		result, err := executor.ForWorkingDir(wt.Dir).Dispatch(ctx, hooks.EventWorktreeCreate, &hooks.Input{
			AgentName: params.AgentName, Cwd: wt.Dir, ParentSessionID: params.ParentSession.ID,
			WorktreePath: wt.Dir, WorktreeBranch: wt.Branch, WorktreeSourceDir: wt.SourceDir,
		})
		if err != nil {
			return fail(err)
		}
		if !result.Allowed {
			return fail(fmt.Errorf("worktree_create hook blocked the task: %s", result.Message))
		}
		instructions = result.AdditionalContext
		if params.OnContent != nil && result.SystemMessage != "" {
			params.OnContent(result.SystemMessage)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	isolatedTeam, err := r.team.ForWorkingDir(ctx, wt.Dir)
	if err != nil {
		return fail(err)
	}
	for _, name := range isolatedTeam.AgentNames() {
		a, _ := isolatedTeam.Agent(name)
		// Response-cache keys do not include the delegated task or checkout.
		agent.WithCache(nil)(a)
	}
	r.ensureBudget()
	opts := []Opt{
		WithWorkingDir(wt.Dir), WithCurrentAgent(params.AgentName),
		WithSessionStore(r.sessionStore), WithModelStore(r.modelsStore), WithProviderRegistry(r.providerRegistry),
		WithEnv(r.env), WithClock(r.now), WithTelemetry(r.telemetry),
		WithNonInteractive(true),
		WithHooksRegistry(r.hooksRegistry.Clone()), withSharedBudget(r.currentBudget()),
		WithTracer(r.tracer), WithSessionCompaction(r.sessionCompaction),
		WithManagedOAuth(r.managedOAuth), WithUnmanagedOAuthRedirectURI(r.unmanagedOAuthRedirectURI),
		WithToolListTimeout(r.toolListTimeout), WithToolStartTimeout(r.toolStartTimeout),
		WithMaxOverflowCompactions(r.maxOverflowCompactions), WithModelSwitcherConfig(r.modelSwitcherCfg),
	}
	for _, injector := range r.autoInjectors {
		opts = append(opts, WithAutoInjector(injector))
	}
	if r.fallback.retryOnRateLimit {
		opts = append(opts, WithRetryOnRateLimit())
	}
	if r.commandEvaluator != nil {
		opts = append(opts, WithCommandEvaluatorFactory(*r.commandEvaluator))
	}
	if r.harnessFactory != nil {
		opts = append(opts, WithHarnessFactory(*r.harnessFactory))
	}
	for _, transform := range r.transforms {
		if transform.name != BuiltinStripGeneratedMedia && transform.name != BuiltinStripUnsupportedModalities {
			opts = append(opts, WithMessageTransform(transform.name, transform.fn))
		}
	}
	isolated, err := NewLocalRuntime(ctx, isolatedTeam, opts...)
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if stopErr := isolatedTeam.Dispose(stopCtx); stopErr != nil {
			slog.WarnContext(ctx, "Failed to stop isolated toolsets", "error", stopErr)
		}
		return fail(err)
	}
	isolated.observers = slices.Clone(r.observers)
	isolated.isolatedWorktree = true
	isolated.liveSessionParent = r
	isolated.OnBackgroundEventWithContext(r.emitBackgroundEvent)
	workspace := &backgroundWorktree{tree: wt, runtime: isolated, instructions: instructions}
	isolated.worktreePrompt = workspace.prompt()
	return workspace, nil
}

func closeWorktreeRuntime(ctx context.Context, rt *LocalRuntime) {
	if err := rt.Close(); err != nil {
		slog.WarnContext(ctx, "Failed to close worktree runtime", "error", err)
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := rt.team.Dispose(stopCtx); err != nil {
		slog.WarnContext(ctx, "Failed to stop worktree toolsets", "error", err)
	}
}
