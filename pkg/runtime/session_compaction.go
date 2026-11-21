package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/compactor"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

var errCompactionBudgetExceeded = errors.New("compaction budget exhausted")

// ErrCompactionInProgress indicates that the session already has an active compaction.
var ErrCompactionInProgress = errors.New("compaction already in progress")

func (r *LocalRuntime) acquireCompaction(sessionID string) bool {
	_, loaded := r.activeCompactions.LoadOrStore(sessionID, struct{}{})
	return !loaded
}

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
// the kept-tail boundary) lives in [compactor]; this function
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
//     skips both native and LLM-based summarization. The kept-tail policy
//     stays consistent across the hook and LLM paths via
//     [compactor.ComputeFirstKeptEntry]; native compaction keeps no tail
//     (see [LocalRuntime.compactNatively]).
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
	result := r.generateCompaction(ctx, sess, a, additionalPrompt, reason, events)
	if result == nil {
		return
	}
	outcome := r.applyCompactionResult(ctx, sess, a, result, reason, events)
	events.Emit(SessionCompactionCompleted(sess.ID, outcome, a.Name()))
}

func (r *LocalRuntime) generateCompaction(ctx context.Context, sess *session.Session, a *agent.Agent, additionalPrompt, reason string, events EventSink) (result *compactor.Result) {
	contextLimit, limitErr := r.compactionContextLimitChecked(ctx, a)

	// before_compaction: hooks can veto or supply a custom summary.
	pre := r.executeBeforeCompactionHooks(ctx, sess, a, reason, contextLimit, events)
	if pre != nil && !pre.Allowed {
		slog.InfoContext(ctx, "Session compaction skipped by before_compaction hook",
			"session_id", sess.ID, "agent", a.Name(), "reason", reason,
			"hook_message", pre.Message,
		)
		return nil
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
		if result == nil {
			events.Emit(SessionCompactionCompleted(sess.ID, outcome, a.Name()))
		}
	}()

	// Choose the strategy: a hook-supplied summary if before_compaction
	// returned one, then provider-native compaction when the model opts in,
	// otherwise the default LLM strategy.
	result = summaryFromHook(sess, a, pre, contextLimit)
	if result == nil {
		native, err := nativeCompactor(ctx, a)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to generate session summary", "error", err)
			events.Emit(ErrorForSession(sess.ID, err.Error()))
			outcome = CompactionOutcomeFailed
			return nil
		}
		if native != nil {
			result, err = r.compactNatively(ctx, sess, a, native, additionalPrompt, events)
		} else {
			if limitErr != nil {
				events.Emit(ErrorForSession(sess.ID, limitErr.Error()))
				outcome = CompactionOutcomeFailed
				return nil
			}
			if contextLimit <= 0 {
				slog.WarnContext(ctx, "Session compaction cannot run: summary model context window unknown",
					"session_id", sess.ID, "agent", a.Name())
				events.Emit(WarningForSession(sess.ID, unknownContextWindowMessage(ctx, a), a.Name()))
				outcome = CompactionOutcomeFailed
				return nil
			}
			result, err = compactor.RunLLM(ctx, compactor.LLMArgs{
				Session:          sess,
				Agent:            a,
				AdditionalPrompt: additionalPrompt,
				ContextLimit:     contextLimit,
				RunAgent: func(ctx context.Context, summaryAgent *agent.Agent, summarySession *session.Session) error {
					return r.runCompactionAgent(ctx, summaryAgent, summarySession, sess, events)
				},
			})
		}
		if errors.Is(err, errCompactionBudgetExceeded) {
			outcome = CompactionOutcomeSkipped
			return nil
		}
		if err != nil {
			slog.ErrorContext(ctx, "Failed to generate session summary", "error", err)
			events.Emit(ErrorForSession(sess.ID, err.Error()))
			outcome = CompactionOutcomeFailed
			return nil
		}
		if result == nil {
			// Empty summary — bail without applying anything.
			outcome = CompactionOutcomeSkipped
			return nil
		}
	}

	return result
}

func (r *LocalRuntime) applyCompactionResult(ctx context.Context, sess *session.Session, a *agent.Agent, result *compactor.Result, reason string, events EventSink) string {
	contextLimit := r.compactionContextLimit(ctx, a)
	// Capture the pre-compaction token counts so the after_compaction
	// hook can observe what was summarized ("compacted from X to Y").
	// We snapshot before applying the result because the apply step
	// resets sess.OutputTokens to 0 and replaces sess.InputTokens with
	// the new summary's estimated size.
	preInputTokens, preOutputTokens := sess.Usage()

	// Apply and persist the summary as one intrinsic successful-compaction step.
	// Manual Summarize calls do not pass through the RunStream observer chain,
	// while normal observed runs do; marking the emitted event as persisted
	// keeps the observer from appending a duplicate row.
	item := session.Item{
		Summary:        result.Summary,
		FirstKeptEntry: result.FirstKeptEntry,
		Cost:           result.Cost,
		Model:          result.Model,
		Usage:          summaryUsage(result),
		Compaction:     result.Compaction,
	}
	// Atomically persist the metadata and summary before mutating the live
	// session. A failed write is a failed compaction: no success summary event
	// is emitted and the in-memory continuation remains unchanged.
	if err := flushObservers(ctx); err != nil {
		events.Emit(ErrorForSession(sess.ID, fmt.Sprintf("Failed to flush events before session compaction: %v", err)))
		return CompactionOutcomeFailed
	}
	if err := r.sessionStore.PersistCompaction(ctx, sess, result.InputTokens, 0, item); err != nil {
		slog.ErrorContext(ctx, "Failed to persist session compaction", "session_id", sess.ID, "error", err)
		events.Emit(ErrorForSession(sess.ID, fmt.Sprintf("Failed to persist session compaction: %v", err)))
		return CompactionOutcomeFailed
	}

	if result.Compaction != nil {
		r.toolDeferrals.Reset(sess.ID)
	}
	slog.DebugContext(ctx, "Generated session summary", "session_id", sess.ID, "summary_length", len(result.Summary))
	summaryEvent := SessionSummary(sess.ID, result.Summary, a.Name(), result.FirstKeptEntry, result.Cost, result.Model, summaryUsage(result))
	if e, ok := summaryEvent.(*SessionSummaryEvent); ok {
		e.persisted = true
	}
	events.Emit(summaryEvent)

	// after_compaction: observational. Fired only when a summary was
	// actually applied to the session. The hook receives the
	// pre-compaction token counts (what was summarized) so observability
	// handlers can compute "compacted from X to Y"; the new (lower)
	// counts live on sess.InputTokens / sess.OutputTokens after this
	// returns and are exposed via the next TokenUsageEvent.
	r.executeAfterCompactionHooks(ctx, sess, a, reason, contextLimit, preInputTokens, preOutputTokens, result.Summary, events)
	return CompactionOutcomeApplied
}

// summaryUsage lifts a result's usage into the *chat.Usage form session
// items and events carry, mapping the zero value (hook-supplied summaries,
// no LLM call) to nil so nothing pretends tokens were consumed.
func summaryUsage(result *compactor.Result) *chat.Usage {
	if result.Usage == (chat.Usage{}) {
		return nil
	}
	return new(result.Usage)
}

// summaryFromHook lifts a before_compaction hook's Summary verdict into
// a [compactor.Result] that the runtime can apply with the same code
// path as the LLM strategy. Returns nil when no hook supplied a
// summary (the caller then tries native compaction, else [compactor.RunLLM]).
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
	limit, err := r.compactionContextLimitChecked(ctx, a)
	if err != nil {
		slog.DebugContext(ctx, "Compaction context window unresolvable", "error", err)
	}
	return limit
}

func (r *LocalRuntime) compactionContextLimitChecked(ctx context.Context, a *agent.Agent) (int64, error) {
	if a == nil {
		return 0, nil
	}
	model := compactionSummaryModel(ctx, a)
	if model == nil {
		return 0, nil
	}
	summaryModel, err := provider.Clone(ctx, model,
		options.WithStructuredOutput(nil),
		options.WithMaxTokens(compactor.MaxSummaryTokens),
	)
	if err != nil {
		return 0, fmt.Errorf("resolving compaction context window: %w", err)
	}
	return r.resolveContextLimit(ctx, summaryModel, summaryModel.ID()), nil
}

// compactionSummaryModel returns the model that makes the summary call: the
// dedicated compaction model when one is configured, otherwise the agent's own.
func compactionSummaryModel(ctx context.Context, a *agent.Agent) provider.Provider {
	if m := a.CompactionModel(); m != nil {
		return m
	}
	return a.Model(ctx)
}

// unknownContextWindowMessage tells the user which model's context window is
// missing and how to declare it, without guessing a limit.
func unknownContextWindowMessage(ctx context.Context, a *agent.Agent) string {
	const fix = "Define that model in the models section with context_window set to its context window (inline provider/model references cannot set context_window), or point compaction_model at a model with a known context window."
	model := compactionSummaryModel(ctx, a)
	if model == nil {
		return "Session compaction failed: the summary model's context window is unknown. " + fix
	}
	if a.CompactionModel() != nil {
		return fmt.Sprintf("Session compaction failed: the compaction model %q has no known context window. %s", model.ID().String(), fix)
	}
	return fmt.Sprintf("Session compaction failed: model %q has no known context window, and it also runs the summary because no compaction_model is set. %s", model.ID().String(), fix)
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

// compactionCapAttribution resolves the identity of the agent's dedicated
// compaction model together with both models' context windows, for callers
// that need to attribute a possibly-capped effective context limit to the
// model that imposes it. Returns a zero compactionModel and zero limits when
// the agent has no dedicated compaction model configured.
func (r *LocalRuntime) compactionCapAttribution(ctx context.Context, a *agent.Agent, modelID modelsdev.ID) (compactionModel string, primaryLimit, compactionLimit int64) {
	if a == nil {
		return "", 0, 0
	}
	cm := a.CompactionModel()
	if cm == nil {
		return "", 0, 0
	}
	return cm.ID().String(), r.resolveContextLimit(ctx, a.Model(ctx), modelID), r.compactionContextLimit(ctx, a)
}

// compactionCaps reports whether a dedicated compaction model's window
// actually reduces the effective context limit below the primary model's
// own window — the condition under which UIs should attribute the cap to
// the compaction model.
func compactionCaps(primaryLimit, compactionLimit int64) bool {
	return compactionLimit > 0 && compactionLimit < primaryLimit
}

// resolveContextLimit resolves the effective context window size for a
// model. Resolution order:
//
//  1. The window the user declared, when set, is authoritative: the
//     model-level context_window, or provider_opts.context_size for DMR
//     (see [latest.ModelConfig.ContextWindow]). This makes compaction work
//     for models that aren't catalogued in models.dev (e.g. a HuggingFace
//     GGUF or a custom openai_responses endpoint).
//  2. Otherwise, the provider's discovered runtime context window.
//  3. Otherwise, the models.dev catalogue limit looked up by id.
//  4. Otherwise, 0 (caller treats this as "can't compact").
func (r *LocalRuntime) resolveContextLimit(ctx context.Context, p provider.Provider, id modelsdev.ID) int64 {
	if n := declaredContextLimit(p); n > 0 {
		return n
	}
	if n := r.discoveredContextLimit(ctx, p); n > 0 {
		return n
	}
	m, err := r.modelsStore.GetModel(ctx, id)
	if err == nil && m != nil && m.Limit.Context > 0 {
		return int64(m.Limit.Context)
	}
	return 0
}

// declaredContextLimit returns the context window declared for a provider's
// resolved [latest.ModelConfig], or 0 when none applies. resolveContextLimit
// checks it first; for models absent from the models.dev catalogue it and the
// provider's discovered window (see discoveredContextLimit) are the only
// sources.
func declaredContextLimit(p provider.Provider) int64 {
	if p == nil {
		return 0
	}
	cfg := p.BaseConfig()
	return cfg.ModelConfig.EffectiveContextWindow()
}

// runCompactionAgent runs an agent against a sub-session for compaction.
// It is the runtime-side glue [compactor] invokes via callback,
// which avoids creating an import cycle on pkg/runtime.
//
// The summary uses the parent's pricing and budgets, even when invoked manually.
func (r *LocalRuntime) runCompactionAgent(ctx context.Context, a *agent.Agent, sess, parent *session.Session, events EventSink) error {
	r.ensureBudget()
	budget := r.currentBudget()
	t := team.New(team.WithAgents(a))
	rt, err := New(ctx, t, WithSessionCompaction(false), WithModelStore(r.modelsStore),
		WithClock(r.now), withSharedBudget(budget))
	if err != nil {
		return err
	}

	warnUnpriced := !budget.unpricedSpend()
	var runErr error
	for event := range rt.RunStream(ctx, sess) {
		switch e := event.(type) {
		case *BudgetExceededEvent:
			// Never let the child's synthetic stop message replace the conversation.
			runErr = errCompactionBudgetExceeded
			events.Emit(Warning(fmt.Sprintf("Compaction skipped: %s limit reached (used %s of %s).", e.ConfigPath, e.Used, e.Max), a.Name()))
		case *ErrorEvent:
			if runErr == nil {
				runErr = errors.New(e.Error)
				// Keep token overflows typed so the compactor can retry smaller.
				if e.Code == ErrorCodeContextExceeded {
					runErr = childTokenOverflowError{msg: e.Error}
				}
			}
		}
	}
	if budget != nil {
		if warnUnpriced && budget.unpricedSpend() {
			events.Emit(Warning(unpricedSpendWarning, a.Name()))
		}
		events.Emit(BudgetUsage(parent.ID, a.Name(), budget.snapshot()))
	}
	return runErr
}

// childTokenOverflowError carries a compaction child run's context_exceeded error:
// the message stays verbatim while [modelerrors.OverflowKindOf] still sees a
// token overflow.
type childTokenOverflowError struct{ msg string }

func (e childTokenOverflowError) Error() string { return e.msg }

func (e childTokenOverflowError) Unwrap() error {
	return &modelerrors.ContextOverflowError{Underlying: errors.New(e.msg), Kind: modelerrors.OverflowKindTokens}
}
