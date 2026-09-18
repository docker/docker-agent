package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/hooks/events"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
)

// Executor dispatches configured hooks. Hook types are resolved against
// a [Registry] of [HandlerFactory]s; embedders can register new kinds
// (in-process Go callbacks, HTTP webhooks, ...) without touching the
// executor itself.
type Executor struct {
	workingDir string
	env        []string
	registry   *Registry
	// events maps each event to its compiled matcher list. Flat events
	// (everything except pre/post_tool_use) are stored as a single
	// matcher with a nil pattern, unifying the dispatch path.
	events map[EventType][]matcher
}

// matcher is the compiled form of a [MatcherConfig]: a tool-name regex
// plus the hooks to fire when it matches. A nil pattern matches every
// tool — both "" and "*" matchers compile to nil, as do flat events
// where the tool-name dimension doesn't apply.
type matcher struct {
	pattern *regexp.Regexp
	hooks   []Hook
}

func (m *matcher) matches(toolName string) bool {
	return m.pattern == nil || m.pattern.MatchString(toolName)
}

// NewExecutor creates a new hook executor backed by [DefaultRegistry].
func NewExecutor(config *Config, workingDir string, env []string) *Executor {
	return NewExecutorWithRegistry(config, workingDir, env, DefaultRegistry)
}

// NewExecutorWithRegistry creates a new hook executor that resolves hook
// types against the supplied registry.
func NewExecutorWithRegistry(config *Config, workingDir string, env []string, registry *Registry) *Executor {
	if config == nil {
		config = &Config{}
	}
	if registry == nil {
		registry = DefaultRegistry
	}
	return &Executor{
		workingDir: workingDir,
		env:        env,
		registry:   registry,
		events:     compileEvents(config),
	}
}

// compileEvents compiles the persisted event lists once per executor.
func compileEvents(c *Config) map[EventType][]matcher {
	compiled := make(map[EventType][]matcher)
	for event, matchers := range c.Events() {
		if EventType(event) == EventPreToolUse {
			compiled[EventPreToolUse], compiled[EventPreToolUsePreYolo] = splitPreToolUseByPreemptYolo(matchers)
		} else {
			compiled[EventType(event)] = compileMatchers(matchers)
		}
	}
	return compiled
}

// splitPreToolUseByPreemptYolo buckets pre_tool_use matcher entries
// into the default lane (post-Decide()) and the preempt-yolo lane
// (pre-Decide()). The lane is selected per entry via
// [MatcherConfig.PreemptYolo]; nil/false → default, true → preempt.
//
// The split is done once at compileEvents time so the dispatcher's
// hot path is a single map lookup per lane. Entries inside one
// matcher entry share a lane — if an author wants two hooks at
// different lanes, they declare two YAML entries.
func splitPreToolUseByPreemptYolo(configs []MatcherConfig) (defaults, preempt []matcher) {
	var defaultCfgs, preemptCfgs []MatcherConfig
	for _, mc := range configs {
		if mc.PreemptYolo != nil && *mc.PreemptYolo {
			preemptCfgs = append(preemptCfgs, mc)
		} else {
			defaultCfgs = append(defaultCfgs, mc)
		}
	}
	return compileMatchers(defaultCfgs), compileMatchers(preemptCfgs)
}

func compileMatchers(configs []MatcherConfig) []matcher {
	if len(configs) == 0 {
		return nil
	}
	out := make([]matcher, 0, len(configs))
	for _, mc := range configs {
		m := matcher{hooks: mc.Hooks}
		if mc.Matcher != "" && mc.Matcher != "*" {
			p, err := regexp.Compile("^(?:" + mc.Matcher + ")$")
			if err != nil {
				slog.Warn("Invalid hook matcher pattern", "pattern", mc.Matcher, "error", err)
				continue
			}
			m.pattern = p
		}
		out = append(out, m)
	}
	return out
}

// Has reports whether any hooks are configured for event.
func (e *Executor) Has(event EventType) bool {
	return len(e.events[event]) > 0
}

// Dispatch runs the hooks registered for event and aggregates their
// verdicts into a single [Result]. Sets input.HookEventName so handlers
// don't have to remember. Defaults [Input.Cwd] to the executor's
// working directory when the caller didn't supply one. Rewrite-capable
// events run sequentially; all other events run concurrently.
//
// EventPreToolUsePreYolo is an internal sentinel for the preempt-yolo
// lane of pre_tool_use. Hooks on this lane see input.HookEventName =
// EventPreToolUse (the executor normalises it before invoking
// handlers) and aggregation reuses the EventPreToolUse branches.
// Tracing keeps the lane visible via the span name.
func (e *Executor) Dispatch(ctx context.Context, event EventType, input *Input) (*Result, error) {
	if EventContract(event).Name == "" {
		return nil, fmt.Errorf("unknown hook event %q", event)
	}
	if input == nil {
		return nil, errors.New("hook input must not be nil")
	}
	hooks := e.hooksFor(event, input.ToolName)
	if len(hooks) == 0 {
		return &Result{Allowed: true}, nil
	}

	// Hooks on the preempt-yolo lane see the public pre_tool_use event
	// name so a single handler implementation works on either lane.
	publicEvent := event
	if event == EventPreToolUsePreYolo {
		publicEvent = EventPreToolUse
	}

	// Single span per Dispatch call covers every hook the event matched.
	// Custom name `hook.{event}` because there is no GenAI semconv for
	// arbitrary user-defined lifecycle hooks; we surface the event type,
	// matched hook count, and session/agent identifiers so dashboards can
	// split by event class without parsing span events.
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/hooks").Start(
		ctx,
		"hook."+string(event),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("cagent.hook.event", string(event)),
			attribute.Int("cagent.hook.count", len(hooks)),
			attribute.String("cagent.agent.name", input.AgentName),
			attribute.String("gen_ai.conversation.id", input.SessionID),
		),
	)
	if input.ToolName != "" {
		span.SetAttributes(attribute.String("gen_ai.tool.name", input.ToolName))
	}
	defer span.End()

	input.HookEventName = publicEvent
	if input.Cwd == "" {
		input.Cwd = e.workingDir
	}

	slog.DebugContext(ctx, "Executing hooks", "event", event, "session_id", input.SessionID, "count", len(hooks))

	inputJSON, err := input.ToJSON()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		err = fmt.Errorf("failed to serialize hook input: %w", err)
		results := make([]hookResult, len(hooks))
		for i, hook := range hooks {
			results[i] = hookResult{hook: hook, err: err}
		}
		final := aggregate(results, event)
		if EventContract(event).CanBlock {
			final.Allowed = false
			final.ExitCode = -1
			final.Message = err.Error()
		}
		annotateHookSpan(span, event, final)
		return final, nil
	}

	var final *Result
	if EventContract(event).Sequential() {
		final = e.runPipeline(ctx, event, hooks, *input, inputJSON)
	} else {
		results := concurrent.MapSlice(hooks, func(hook Hook) hookResult {
			return e.runHook(ctx, event, hook, inputJSON)
		})
		final = aggregate(results, event)
	}
	annotateHookSpan(span, event, final)
	return final, nil
}

// annotateHookSpan stamps the aggregated verdict onto the hook.{event}
// span so dashboards can answer "did the hook block this?" and "why?"
// without re-running the hook. Prior to this the span only carried the
// event type and hook count — a denied call looked identical to an
// allowed one. The verdict booleans and short reason are unconditional
// (they're decisions, not content); free-text fields that may contain
// PII or LLM output (Message, AdditionalContext, SystemMessage,
// Summary) are gated on the GenAI content-capture opt-in.
func annotateHookSpan(span trace.Span, event EventType, r *Result) {
	if span == nil || r == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.Bool("cagent.hook.allowed", r.Allowed),
		attribute.Int("cagent.hook.exit_code", r.ExitCode),
	}
	if r.Decision != "" {
		attrs = append(attrs, attribute.String("cagent.hook.decision", string(r.Decision)))
	}
	if r.DecisionReason != "" {
		attrs = append(attrs, attribute.String("cagent.hook.decision_reason", r.DecisionReason))
	}
	if EventContract(event).PermissionApproval {
		attrs = append(attrs, attribute.Bool("cagent.hook.permission_allowed", r.PermissionAllowed))
	}
	if r.ModifiedInput != nil {
		attrs = append(attrs, attribute.Bool("cagent.hook.modified_input", true))
	}
	if r.Summary != "" {
		attrs = append(attrs, attribute.Bool("cagent.hook.summary_provided", true))
	}
	if genai.IsContentCaptureEnabled() {
		if r.Message != "" {
			attrs = append(attrs, attribute.String("cagent.hook.message", r.Message))
		}
		if r.AdditionalContext != "" {
			attrs = append(attrs, attribute.String("cagent.hook.additional_context", r.AdditionalContext))
		}
		if r.SystemMessage != "" {
			attrs = append(attrs, attribute.String("cagent.hook.system_message", r.SystemMessage))
		}
		if r.Summary != "" {
			attrs = append(attrs, attribute.String("cagent.hook.summary", r.Summary))
		}
	}
	span.SetAttributes(attrs...)
}

// hooksFor keeps the first matching occurrence of each complete definition.
// Identical user-authored and auto-injected hooks run only once per dispatch.
func (e *Executor) hooksFor(event EventType, toolName string) []Hook {
	var hooks []Hook
	for _, m := range e.events[event] {
		if !m.matches(toolName) {
			continue
		}
		for _, h := range m.hooks {
			if slices.ContainsFunc(hooks, func(existing Hook) bool { return sameHook(existing, h) }) {
				continue
			}
			hooks = append(hooks, h)
		}
	}
	return hooks
}

// sameHook compares configured values without expanding env or working_dir.
func sameHook(a, b Hook) bool {
	return a.Name == b.Name &&
		a.Type == b.Type &&
		a.Command == b.Command &&
		slices.Equal(a.Args, b.Args) &&
		a.Timeout == b.Timeout &&
		maps.Equal(a.Env, b.Env) &&
		a.WorkingDir == b.WorkingDir &&
		a.OnError == b.OnError &&
		a.StrictOutput == b.StrictOutput &&
		a.Model == b.Model &&
		a.Prompt == b.Prompt &&
		a.Schema == b.Schema
}

// hookResult is the outcome of a single hook invocation: the raw
// [HandlerResult] reported by the handler plus a post-execution err
// (factory failure, timeout, exec error). When err is non-nil the
// handler-reported fields are reset to a uniform "did not run"
// representation so [aggregate] can rely on the err alone.
type hookResult struct {
	HandlerResult

	hook Hook
	err  error
}

// runHook resolves the hook's [HookType] in the registry, applies its
// timeout, and returns the structured outcome. JSON-on-stdout is parsed
// into [Output] when the handler didn't already provide one.
func (e *Executor) runHook(ctx context.Context, event EventType, hook Hook, inputJSON []byte) hookResult {
	factory, ok := e.registry.Lookup(hook.Type)
	if !ok {
		return hookResult{hook: hook, err: fmt.Errorf("unsupported hook type: %s", hook.Type)}
	}
	handler, err := factory(HandlerEnv{WorkingDir: e.workingDir, Env: e.env}, hook)
	if err != nil {
		return hookResult{hook: hook, err: err}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, hook.GetTimeout())
	defer cancel()

	res, err := handler.Run(timeoutCtx, inputJSON)
	r := hookResult{HandlerResult: res, hook: hook}

	// markFailed turns r into a "did not complete" outcome: the
	// handler's diagnostic stdout/stderr survive (aggregate surfaces
	// stderr in the PreToolUse fail-closed message), ExitCode is
	// pinned to -1 to match the documented [Result.ExitCode]
	// convention, any partial Output is dropped (it can't have been
	// authoritative if the run didn't complete), and rerr lands in
	// hookResult.err for the err-!= nil branch in [aggregate].
	markFailed := func(rerr error) hookResult {
		r.ExitCode = -1
		r.Output = nil
		r.err = rerr
		return r
	}

	// Normalize timeout/cancellation: handler error types vary, so we
	// rewrite to a uniform error so PreToolUse fails closed cleanly.
	if ctxErr := timeoutCtx.Err(); ctxErr != nil {
		reason := "cancelled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("timed out after %s", hook.GetTimeout())
		}
		return markFailed(fmt.Errorf("hook %q %s: %w", hook.Command, reason, ctxErr))
	}
	if err != nil {
		return markFailed(err)
	}

	// Fall back to the legacy "parse JSON from stdout" protocol.
	if r.Output == nil && r.ExitCode == 0 {
		r.Output, err = parseStdoutJSON(r.Stdout, hook.StrictOutput)
		if err != nil {
			return markFailed(err)
		}
	}
	if r.ExitCode != 0 && r.ExitCode != 2 {
		return markFailed(fmt.Errorf("exited with status %d", r.ExitCode))
	}
	if r.Output != nil && r.ExitCode == 0 {
		if err := validateOutput(event, r.Output, hook.StrictOutput); err != nil {
			return markFailed(err)
		}
	}
	return r
}

// aggregate combines per-hook results into a single [Result].
func aggregate(results []hookResult, event EventType) *Result {
	contract := EventContract(event)
	final := &Result{Allowed: true}
	var messages, contexts, sysMsgs []string

	for _, r := range results {
		if r.err == nil && r.ExitCode != 0 && r.ExitCode != 2 {
			r.err = fmt.Errorf("exited with status %d", r.ExitCode)
		}
		switch {
		case r.err != nil:
			policy := ErrorPolicy(r.hook.OnError)
			if policy == "" {
				policy = ErrorPolicyWarn
			}
			if contract.FailClosed || (contract.CanBlock && policy == ErrorPolicyBlock) {
				slog.Warn("Hook failed; blocking event", "hook", r.hook.DisplayName(), "error", r.err)
				final.Allowed = false
				final.ExitCode = -1
				final.Stderr = r.Stderr
				messages = append(messages, hookFailureMessage(event, fmt.Errorf("hook %q: %w", r.hook.DisplayName(), r.err)))
			} else if policy != ErrorPolicyIgnore {
				slog.Warn("Hook execution error", "hook", r.hook.DisplayName(), "error", r.err)
				sysMsgs = append(sysMsgs, hookFailureMessage(event, fmt.Errorf("hook %q: %w", r.hook.DisplayName(), r.err)))
			}
			continue

		case r.ExitCode == 2:
			if !contract.CanBlock {
				sysMsgs = append(sysMsgs, fmt.Sprintf("%s hook %q returned exit 2, but this event cannot block", contract.Name, r.hook.DisplayName()))
				continue
			}
			final.Allowed = false
			final.ExitCode = 2
			if r.Stderr != "" {
				final.Stderr = r.Stderr
				messages = append(messages, strings.TrimSpace(r.Stderr))
			}
			continue

		case r.Output == nil:
			// Plain stdout becomes AdditionalContext only for events
			// whose runtime consumes it.
			if r.Stdout != "" && contract.Context {
				contexts = append(contexts, strings.TrimSpace(r.Stdout))
			}
			continue
		}

		out := r.Output
		if !contract.CanBlock && (!out.ShouldContinue() || out.IsBlocked()) {
			sysMsgs = append(sysMsgs, fmt.Sprintf("%s hook %q returned a block, but this event cannot block", contract.Name, r.hook.DisplayName()))
		}
		if contract.CanBlock && !out.ShouldContinue() {
			final.Allowed = false
			if out.StopReason != "" {
				messages = append(messages, out.StopReason)
			}
		}
		if contract.CanBlock && out.IsBlocked() {
			final.Allowed = false
			if out.Reason != "" {
				messages = append(messages, out.Reason)
			}
		}
		if out.SystemMessage != "" {
			sysMsgs = append(sysMsgs, out.SystemMessage)
		}
		if hso := out.HookSpecificOutput; hso != nil {
			if !contract.Permission() && hso.PermissionDecision != "" {
				sysMsgs = append(sysMsgs, fmt.Sprintf("%s hook %q returned permission_decision, but this event does not support approval decisions", contract.Name, r.hook.DisplayName()))
			}
			if contract.Decision && hso.PermissionDecision != "" {
				final.Decision, final.DecisionReason = strongerDecision(
					final.Decision, final.DecisionReason,
					hso.PermissionDecision, hso.PermissionDecisionReason,
				)
			}
			if contract.Decision || contract.PermissionApproval {
				switch hso.PermissionDecision {
				case DecisionDeny:
					final.Allowed = false
					if hso.PermissionDecisionReason != "" {
						messages = append(messages, hso.PermissionDecisionReason)
					}
				case DecisionAllow:
					if contract.PermissionApproval {
						final.PermissionAllowed = true
						if final.DecisionReason == "" {
							final.DecisionReason = hso.PermissionDecisionReason
						}
					}
				}
			}
			if contract.Rewrite == events.RewriteToolInput && hso.UpdatedInput != nil {
				if final.ModifiedInput == nil {
					final.ModifiedInput = make(map[string]any)
				}
				maps.Copy(final.ModifiedInput, hso.UpdatedInput)
			}
			if contract.Summary && hso.Summary != "" && final.Summary == "" {
				// First non-empty summary in CONFIG ORDER wins. Hooks run
				// concurrently (see runHook above), but we iterate
				// `results` in the order they were configured — the index
				// of each hook's slot in `results` is fixed at registration
				// time, not by completion order — so this verdict is
				// deterministic regardless of which hook finishes first.
				// Concatenating multiple summaries would produce nonsense,
				// and merging them would require a second LLM call,
				// defeating the point of the hook-supplied summary (which
				// is to skip the LLM entirely).
				final.Summary = hso.Summary
			}
			if contract.Rewrite == events.RewriteMessages && len(hso.UpdatedMessages) > 0 {
				final.UpdatedMessages = hso.UpdatedMessages
			}
			if contract.Rewrite == events.RewriteToolResponse && hso.UpdatedToolResponse != nil {
				final.UpdatedToolResponse = hso.UpdatedToolResponse
			}
			if contract.Metadata && len(hso.Metadata) > 0 {
				// Metadata from every matching hook is merged so multiple
				// hooks can each contribute keys. On a key clash the last
				// hook in config order wins (results is iterated in
				// registration order).
				if final.Metadata == nil {
					final.Metadata = make(map[string]string)
				}
				maps.Copy(final.Metadata, hso.Metadata)
			}
			if contract.Context && hso.AdditionalContext != "" {
				contexts = append(contexts, hso.AdditionalContext)
			}
			if contract.Instructions {
				final.InstructionContext = append(final.InstructionContext, hso.InstructionContext...)
			}
		}
	}

	final.Message = strings.Join(messages, "\n")
	final.AdditionalContext = strings.Join(contexts, "\n")
	final.SystemMessage = strings.Join(sysMsgs, "\n")
	return final
}

func hookFailureMessage(event EventType, err error) string {
	return fmt.Sprintf("%s hook failed to execute: %v", EventContract(event).Name, err)
}

// decisionWeight ranks PermissionDecision verdicts so [strongerDecision]
// can pick the most-restrictive across a chain of pre_tool_use hooks.
// Deny > Ask > Allow > "" (no decision).
func decisionWeight(d Decision) int {
	switch d {
	case DecisionDeny:
		return 3
	case DecisionAsk:
		return 2
	case DecisionAllow:
		return 1
	default:
		return 0
	}
}

// strongerDecision returns the more-restrictive of two (decision,
// reason) pairs, preserving the reason of the winner. A non-empty
// decision always beats the empty string. Ties keep the existing
// (current) decision so the iteration order in [aggregate] is
// stable and the first-seen reason wins.
func strongerDecision(curD Decision, curR string, newD Decision, newR string) (Decision, string) {
	if decisionWeight(newD) > decisionWeight(curD) {
		return newD, newR
	}
	return curD, curR
}
