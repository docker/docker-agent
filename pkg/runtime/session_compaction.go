package runtime

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/compactor"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// Compaction reasons reported to BeforeCompaction / AfterCompaction hooks.
const (
	compactionReasonThreshold = "threshold"
	compactionReasonOverflow  = "overflow"
	compactionReasonManual    = "manual"
)

// sessionCompactionEnabled reports whether automatic compaction (proactive
// threshold trigger, post-overflow recovery) is active for the given agent:
// the runtime-wide option ANDed with the agent's own `session_compaction`
// config. Manual /compact is intentionally not gated by either flag.
func (r *LocalRuntime) sessionCompactionEnabled(a *agent.Agent) bool {
	return r.sessionCompaction && a.SessionCompaction()
}

// doCompact orchestrates a session compaction. It is intentionally thin:
// the heavy lifting (extracting the conversation, running the LLM, computing
// the kept-tail boundary) lives in [pkg/runtime/compactor]; this function
// owns only what's runtime-private: hook dispatch, session mutation, event
// emission, and persistence.
//
// reason is one of [compactionReasonThreshold] (proactive threshold trigger),
// [compactionReasonOverflow] (post-overflow recovery) or
// [compactionReasonManual] (user-invoked /compact). It is forwarded to
// BeforeCompaction / AfterCompaction hooks.
//
// Hook integration:
//   - BeforeCompaction fires first. If a hook denies (Decision: "block"),
//     the runtime returns immediately without emitting any compaction
//     events; the conversation is left untouched.
//   - If a BeforeCompaction hook supplies a non-empty Summary in
//     HookSpecificOutput, the runtime applies that summary verbatim and
//     skips the LLM-based summarization entirely. The kept-tail policy
//     stays consistent across both paths via [compactor.ComputeFirstKeptEntry].
//   - AfterCompaction fires after the summary has been applied; it is
//     observational.
//
// If no hooks are configured for any of these events, control flow is
// behaviourally identical to the original, hookless implementation.
//
// Note: the runtime does NOT re-fire session_start with Source="compact".
// session_start hook output is held as transient context that is threaded
// into every model call (see [LocalRuntime.executeSessionStartHooks]), so
// env / cwd / OS info is automatically present after a compaction without
// any extra dispatch.
func (r *LocalRuntime) doCompact(ctx context.Context, sess *session.Session, a *agent.Agent, additionalPrompt, reason string, events EventSink) {
	contextLimit := r.compactionContextLimit(ctx, a)

	// before_compaction: hooks can veto or supply a custom summary.
	pre := r.executeBeforeCompactionHooks(ctx, sess, a, reason, contextLimit, events)
	if pre != nil && !pre.Allowed {
		slog.InfoContext(ctx, "Session compaction skipped by before_compaction hook",
			"session_id", sess.ID, "agent", a.Name(), "reason", reason,
			"hook_message", pre.Message,
		)
		return
	}

	slog.DebugContext(ctx, "Generating summary for session", "session_id", sess.ID, "reason", reason)
	events.Emit(SessionCompaction(sess.ID, "started", a.Name()))
	// outcome qualifies the terminal "completed" event: "applied" when a
	// summary was written to the session, "skipped" for no-ops (nothing
	// to compact, empty model output), "failed" when an error was
	// emitted. UIs use it to avoid announcing success for a compaction
	// that did nothing.
	outcome := CompactionOutcomeApplied
	defer func() {
		events.Emit(SessionCompactionCompleted(sess.ID, outcome, a.Name()))
	}()

	// Choose the strategy: a hook-supplied summary if before_compaction
	// returned one, otherwise the default LLM strategy.
	result := summaryFromHook(sess, a, pre, contextLimit)
	if result == nil {
		if contextLimit <= 0 {
			slog.ErrorContext(ctx, "Failed to generate session summary",
				"error", "model definition unavailable")
			events.Emit(ErrorForSession(sess.ID, "Failed to get model definition"))
			outcome = CompactionOutcomeFailed
			return
		}

		var err error
		result, err = compactor.RunLLM(ctx, compactor.LLMArgs{
			Session:          sess,
			Agent:            a,
			AdditionalPrompt: additionalPrompt,
			ContextLimit:     contextLimit,
			RunAgent:         r.runCompactionAgent,
		})
		if err != nil {
			slog.ErrorContext(ctx, "Failed to generate session summary", "error", err)
			events.Emit(ErrorForSession(sess.ID, err.Error()))
			outcome = CompactionOutcomeFailed
			return
		}
		if result == nil {
			// Empty summary — bail without applying anything.
			outcome = CompactionOutcomeSkipped
			return
		}
	}

	// Capture the pre-compaction token counts so the after_compaction
	// hook can observe what was summarized ("compacted from X to Y").
	// We snapshot before applying the result because the apply step
	// resets sess.OutputTokens to 0 and replaces sess.InputTokens with
	// the new summary's estimated size.
	preInputTokens, preOutputTokens := sess.Usage()

	// Apply the summary to the session. This is intrinsically
	// runtime-private: it mutates session-internal state and persists
	// through the runtime's session store.
	sess.ApplyCompaction(result.InputTokens, 0, session.Item{
		Summary:        result.Summary,
		FirstKeptEntry: result.FirstKeptEntry,
		Cost:           result.Cost,
	})
	_ = r.sessionStore.UpdateSession(ctx, sess)

	slog.DebugContext(ctx, "Generated session summary", "session_id", sess.ID, "summary_length", len(result.Summary))
	events.Emit(SessionSummary(sess.ID, result.Summary, a.Name(), result.FirstKeptEntry))

	// after_compaction: observational. Fired only when a summary was
	// actually applied to the session. The hook receives the
	// pre-compaction token counts (what was summarized) so observability
	// handlers can compute "compacted from X to Y"; the new (lower)
	// counts live on sess.InputTokens / sess.OutputTokens after this
	// returns and are exposed via the next TokenUsageEvent.
	r.executeAfterCompactionHooks(ctx, sess, a, reason, contextLimit, preInputTokens, preOutputTokens, result.Summary, events)
}

// summaryFromHook lifts a before_compaction hook's Summary verdict into
// a [compactor.Result] that the runtime can apply with the same code
// path as the LLM strategy. Returns nil when no hook supplied a
// summary (the caller then falls through to [compactor.RunLLM]).
//
// The hook only contributes the summary text; the runtime fills in the
// kept-tail boundary (matching the LLM path's policy) and estimates the
// summary's token count for session bookkeeping. The Result.Cost is
// left at its zero value because no LLM call ran — the hook produced
// the summary itself, so there's nothing to bill.
func summaryFromHook(sess *session.Session, a *agent.Agent, pre *hooks.Result, contextLimit int64) *compactor.Result {
	if pre == nil || pre.Summary == "" {
		return nil
	}
	slog.Debug("Using compaction summary from before_compaction hook",
		"session_id", sess.ID, "agent", a.Name(), "summary_length", len(pre.Summary))
	return &compactor.Result{
		Summary:        pre.Summary,
		FirstKeptEntry: compactor.ComputeFirstKeptEntry(sess, contextLimit),
		// Estimate the summary's token count for session bookkeeping;
		// no LLM was called so Cost stays at the zero value.
		InputTokens: compaction.EstimateMessageTokens(&chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: pre.Summary,
		}),
	}
}

// compactionContextLimit returns the context-window limit of the model that
// generates the summary: the dedicated compaction model when one is
// configured, otherwise the agent's own model. Returns 0 when it can't be
// resolved. Failure is non-fatal: a before_compaction hook may supply its own
// summary and never need the model definition. The LLM strategy itself
// enforces ContextLimit > 0.
//
// See [LocalRuntime.resolveContextLimit] for the resolution order; we
// pass the cloned summary-call provider so its provider_opts (which
// match the underlying model) are considered.
func (r *LocalRuntime) compactionContextLimit(ctx context.Context, a *agent.Agent) int64 {
	if a == nil {
		return 0
	}
	model := a.CompactionModel()
	if model == nil {
		model = a.Model(ctx)
	}
	if model == nil {
		return 0
	}
	summaryModel := provider.CloneWithOptions(ctx, model,
		options.WithStructuredOutput(nil),
		options.WithMaxTokens(compactor.MaxSummaryTokens),
	)
	return r.resolveContextLimit(ctx, summaryModel, summaryModel.ID())
}

// effectiveContextLimit returns the context budget the running session
// operates within: the primary model's window, capped by the dedicated
// compaction model's (smaller) window when one is configured. It drives both
// the proactive compaction trigger and the UI context gauge, so the gauge
// fills to the compaction threshold (~90% by default) right as compaction
// fires and the summary call can always ingest the conversation it must
// compact. This is the maintainers' resolution
// for issue #3241 for the case where the dedicated compaction model has the
// smaller context window.
//
// With no dedicated compaction model it is simply primaryLimit (behaviour
// unchanged). A non-positive compaction limit (an unresolvable compaction-model
// definition) falls back to primaryLimit so a misconfigured compaction model
// never suppresses compaction; conversely, when only the compaction model's
// window is resolvable (e.g. a local primary absent from the catalogue), that
// window is used so compaction still runs.
func (r *LocalRuntime) effectiveContextLimit(ctx context.Context, a *agent.Agent, primaryLimit int64) int64 {
	if a == nil || a.CompactionModel() == nil {
		return primaryLimit
	}
	compactionLimit := r.compactionContextLimit(ctx, a)
	if compactionLimit <= 0 {
		return primaryLimit
	}
	if primaryLimit <= 0 {
		return compactionLimit
	}
	return min(primaryLimit, compactionLimit)
}

// resolveContextLimit resolves the effective context window size for a
// model. Resolution order:
//
//  1. The user-supplied [provider_opts.context_size], when set, is
//     authoritative. Some providers (notably Docker Model Runner) use
//     it to size the actual inference context, so we plan against the
//     same number the engine will enforce. This also makes compaction
//     work for local models that aren't catalogued in models.dev (e.g.
//     a HuggingFace GGUF).
//  2. Otherwise, the models.dev catalogue limit looked up by id.
//  3. Otherwise, 0 (caller treats this as "can't compact").
func (r *LocalRuntime) resolveContextLimit(ctx context.Context, p provider.Provider, id modelsdev.ID) int64 {
	if n := providerContextLimit(p); n > 0 {
		return n
	}
	m, err := r.modelsStore.GetModel(ctx, id)
	if err == nil && m != nil && m.Limit.Context > 0 {
		return int64(m.Limit.Context)
	}
	return 0
}

// providerContextLimit reads [provider_opts.context_size] from a
// provider's resolved [latest.ModelConfig], returning 0 when unset or
// not parseable as an integer. This is the fallback used when the
// models.dev catalogue does not have an entry for the configured
// model (typically Docker Model Runner with a HuggingFace GGUF model).
//
// The parsing is shared with the provider clients (which clamp max_tokens
// against the same window) via [latest.ContextSizeFromProviderOpts], so the
// two never disagree on how context_size is interpreted.
func providerContextLimit(p provider.Provider) int64 {
	if p == nil {
		return 0
	}
	return latest.ContextSizeFromProviderOpts(p.BaseConfig().ModelConfig.ProviderOpts)
}

// runCompactionAgent runs an agent against a sub-session for compaction.
// It is the runtime-side glue [pkg/runtime/compactor] invokes via callback,
// which avoids creating an import cycle on [pkg/runtime].
func (r *LocalRuntime) runCompactionAgent(ctx context.Context, a *agent.Agent, sess *session.Session) error {
	t := team.New(team.WithAgents(a))
	rt, err := New(ctx, t, WithSessionCompaction(false))
	if err != nil {
		return err
	}
	_, err = rt.Run(ctx, sess)
	return err
}
