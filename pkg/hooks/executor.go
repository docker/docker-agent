package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
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

// compileEvents builds the per-event matcher lookup. This is the only
// place in the runtime that enumerates events; the persisted side
// owns the struct itself, its IsEmpty, and validate, all on
// [latest.HooksConfig]. Adding a new event is a one-line change here.
func compileEvents(c *Config) map[EventType][]matcher {
	flat := func(hooks []Hook) []matcher {
		if len(hooks) == 0 {
			return nil
		}
		return []matcher{{hooks: hooks}}
	}
	return map[EventType][]matcher{
		EventPreToolUse:             compileMatchers(c.PreToolUse),
		EventPostToolUse:            compileMatchers(c.PostToolUse),
		EventPermissionRequest:      compileMatchers(c.PermissionRequest),
		EventSessionStart:           flat(c.SessionStart),
		EventUserPromptSubmit:       flat(c.UserPromptSubmit),
		EventTurnStart:              flat(c.TurnStart),
		EventTurnEnd:                flat(c.TurnEnd),
		EventBeforeLLMCall:          flat(c.BeforeLLMCall),
		EventAfterLLMCall:           flat(c.AfterLLMCall),
		EventSessionEnd:             flat(c.SessionEnd),
		EventPreCompact:             flat(c.PreCompact),
		EventSubagentStop:           flat(c.SubagentStop),
		EventOnUserInput:            flat(c.OnUserInput),
		EventStop:                   flat(c.Stop),
		EventNotification:           flat(c.Notification),
		EventOnError:                flat(c.OnError),
		EventOnMaxIterations:        flat(c.OnMaxIterations),
		EventOnAgentSwitch:          flat(c.OnAgentSwitch),
		EventOnSessionResume:        flat(c.OnSessionResume),
		EventOnToolApprovalDecision: flat(c.OnToolApprovalDecision),
		EventBeforeCompaction:       flat(c.BeforeCompaction),
		EventAfterCompaction:        flat(c.AfterCompaction),
		EventToolResponseTransform:  compileMatchers(c.ToolResponseTransform),
	}
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
// working directory when the caller didn't supply one.
func (e *Executor) Dispatch(ctx context.Context, event EventType, input *Input) (*Result, error) {
	hooks := e.hooksFor(event, input.ToolName)
	if len(hooks) == 0 {
		return &Result{Allowed: true}, nil
	}

	input.HookEventName = event
	if input.Cwd == "" {
		input.Cwd = e.workingDir
	}

	slog.DebugContext(ctx, "Executing hooks", "event", event, "session_id", input.SessionID, "count", len(hooks))

	inputJSON, err := input.ToJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize hook input: %w", err)
	}

	results := make([]hookResult, len(hooks))
	var wg sync.WaitGroup
	for i, hook := range hooks {
		wg.Go(func() { results[i] = e.runHook(ctx, hook, inputJSON) })
	}
	wg.Wait()

	return aggregate(results, event), nil
}

// hooksFor returns the deduplicated list of hooks that should run for
// (event, toolName). Dedup by (type, command, args) catches the common
// case of an explicit YAML hook overlapping a runtime auto-injected
// one (e.g. WithAddDate plus a user-authored add_date entry).
func (e *Executor) hooksFor(event EventType, toolName string) []Hook {
	seen := make(map[string]bool)
	var hooks []Hook
	for _, m := range e.events[event] {
		if !m.matches(toolName) {
			continue
		}
		for _, h := range m.hooks {
			key := dedupKey(h)
			if seen[key] {
				continue
			}
			seen[key] = true
			hooks = append(hooks, h)
		}
	}
	return hooks
}

// dedupKey returns a deterministic key identifying a hook by (type, command, args).
func dedupKey(h Hook) string {
	var b strings.Builder
	b.WriteString(h.Type)
	b.WriteByte(0)
	b.WriteString(h.Command)
	for _, a := range h.Args {
		b.WriteByte(0)
		b.WriteString(a)
	}
	return b.String()
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
func (e *Executor) runHook(ctx context.Context, hook Hook, inputJSON []byte) hookResult {
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
		r.Output = parseStdoutJSON(r.Stdout)
	}
	return r
}

// parseStdoutJSON returns a parsed [Output] when stdout begins with '{'
// and decodes cleanly, or nil otherwise. Used for the legacy "JSON on
// stdout" hook protocol where handlers don't pre-populate
// [HandlerResult.Output].
func parseStdoutJSON(stdout string) *Output {
	s := strings.TrimSpace(stdout)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	var parsed Output
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil
	}
	return &parsed
}

// failClosed reports whether a hook failure on event must deny the
// event. Only PreToolUse is a hard security boundary; every other
// event surfaces failures as warnings unless the hook opts into
// ErrorPolicyBlock.
func failClosed(event EventType) bool {
	return event == EventPreToolUse
}

// stdoutAsContext reports whether plain stdout (non-JSON, exit 0)
// from a hook should be routed into Result.AdditionalContext. It is
// the runtime's emit site that decides whether AdditionalContext is
// surfaced; events that don't consume it MUST drop plain stdout so
// hook authors don't think their output mattered when it would have
// been thrown away.
func stdoutAsContext(event EventType) bool {
	switch event {
	case EventPostToolUse,
		EventSessionStart,
		EventUserPromptSubmit,
		EventTurnStart,
		EventPreCompact,
		EventStop:
		return true
	}
	return false
}

// aggregate combines per-hook results into a single [Result].
func aggregate(results []hookResult, event EventType) *Result {
	final := &Result{Allowed: true}
	var messages, contexts, sysMsgs []string

	for _, r := range results {
		switch {
		case r.err != nil:
			policy := ErrorPolicy(r.hook.OnError)
			if policy == "" {
				policy = ErrorPolicyWarn
			}
			if failClosed(event) || policy == ErrorPolicyBlock {
				slog.Warn("Hook failed; blocking event", "hook", r.hook.DisplayName(), "error", r.err)
				final.Allowed = false
				final.ExitCode = -1
				final.Stderr = r.Stderr
				messages = append(messages, fmt.Sprintf("PreToolUse hook failed to execute: %v", r.err))
			} else if policy != ErrorPolicyIgnore {
				slog.Warn("Hook execution error", "hook", r.hook.DisplayName(), "error", r.err)
			}
			continue

		case r.ExitCode == 2:
			final.Allowed = false
			final.ExitCode = 2
			if r.Stderr != "" {
				final.Stderr = r.Stderr
				messages = append(messages, strings.TrimSpace(r.Stderr))
			}
			continue

		case r.ExitCode != 0:
			slog.Debug("Hook returned non-zero exit code", "exit_code", r.ExitCode, "stderr", r.Stderr)
			continue

		case r.Output == nil:
			// Plain stdout becomes AdditionalContext only for events
			// whose runtime consumes it.
			if r.Stdout != "" && stdoutAsContext(event) {
				contexts = append(contexts, strings.TrimSpace(r.Stdout))
			}
			continue
		}

		out := r.Output
		if !out.ShouldContinue() {
			final.Allowed = false
			if out.StopReason != "" {
				messages = append(messages, out.StopReason)
			}
		}
		if out.IsBlocked() {
			final.Allowed = false
			if out.Reason != "" {
				messages = append(messages, out.Reason)
			}
		}
		if out.SystemMessage != "" {
			sysMsgs = append(sysMsgs, out.SystemMessage)
		}
		if hso := out.HookSpecificOutput; hso != nil {
			if event == EventPreToolUse && hso.PermissionDecision != "" {
				final.Decision, final.DecisionReason = strongerDecision(
					final.Decision, final.DecisionReason,
					hso.PermissionDecision, hso.PermissionDecisionReason,
				)
			}
			if event == EventPreToolUse || event == EventPermissionRequest {
				switch hso.PermissionDecision {
				case DecisionDeny:
					final.Allowed = false
					if hso.PermissionDecisionReason != "" {
						messages = append(messages, hso.PermissionDecisionReason)
					}
				case DecisionAllow:
					if event == EventPermissionRequest {
						final.PermissionAllowed = true
					}
					if hso.PermissionDecisionReason != "" {
						contexts = append(contexts, hso.PermissionDecisionReason)
					}
				}
			}
			if event == EventPreToolUse && hso.UpdatedInput != nil {
				if final.ModifiedInput == nil {
					final.ModifiedInput = make(map[string]any)
				}
				maps.Copy(final.ModifiedInput, hso.UpdatedInput)
			}
			if event == EventBeforeCompaction && hso.Summary != "" && final.Summary == "" {
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
			if event == EventBeforeLLMCall && len(hso.UpdatedMessages) > 0 && final.UpdatedMessages == nil {
				// First non-empty rewrite in CONFIG ORDER wins, same
				// determinism guarantee as Summary above. Concurrent
				// hooks all see the SAME input snapshot, so chaining two
				// independent rewrites here would silently throw one
				// away — callers wanting composition must do it inside
				// a single hook.
				final.UpdatedMessages = hso.UpdatedMessages
			}
			if event == EventToolResponseTransform && hso.UpdatedToolResponse != nil && final.UpdatedToolResponse == nil {
				// First non-nil rewrite in CONFIG ORDER wins (same
				// determinism + composition trade-off as UpdatedMessages
				// above). Pointer-typed: an explicit empty-string rewrite
				// is honoured — the runtime applies *result =
				// *UpdatedToolResponse verbatim, so a hook that wants to
				// blank a leaky tool's output entirely can do so.
				final.UpdatedToolResponse = hso.UpdatedToolResponse
			}
			if hso.AdditionalContext != "" {
				contexts = append(contexts, hso.AdditionalContext)
			}
		}
	}

	final.Message = strings.Join(messages, "\n")
	final.AdditionalContext = strings.Join(contexts, "\n")
	final.SystemMessage = strings.Join(sysMsgs, "\n")
	return final
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
