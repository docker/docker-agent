package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
)

// agentNames returns the names of the given agents.
func agentNames(agents []*agent.Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name()
	}
	return names
}

// validateAgentInList checks that targetAgent appears in the given agent list.
// Returns a tool error result if not found, or nil if the target is valid.
// The action describes the attempted operation (e.g. "transfer task to"),
// and listDesc is a human-readable description of the list (e.g. "sub-agents list").
func validateAgentInList(currentAgent, targetAgent, action, listDesc string, agents []*agent.Agent) *tools.ToolCallResult {
	if slices.ContainsFunc(agents, func(a *agent.Agent) bool { return a.Name() == targetAgent }) {
		return nil
	}
	if names := agentNames(agents); len(names) > 0 {
		return tools.ResultError(fmt.Sprintf(
			"Agent %s cannot %s %s: target agent not in %s. Available agent IDs are: %s",
			currentAgent, action, targetAgent, listDesc, strings.Join(names, ", "),
		))
	}
	return tools.ResultError(fmt.Sprintf(
		"Agent %s cannot %s %s: target agent not in %s. No agents are configured in this list.",
		currentAgent, action, targetAgent, listDesc,
	))
}

// maxDelegationDepth caps the number of chained agent-delegation edges
// (transfer_task and run_background_agent) below a root session. The root
// agent delegating to its first child is depth 1; a delegation is allowed at
// exactly this depth and rejected beyond it. Handoffs and skill sub-sessions
// are not delegation edges and do not count. This is a fixed runtime guard
// against runaway recursion, not user configuration: legitimate teams stay
// well below it.
const maxDelegationDepth = 10

// validateDelegation guards one agent-delegation edge from caller to target
// against the parent session's recorded delegation lineage. On success it
// returns the child session's lineage (parent lineage plus caller, freshly
// allocated so concurrent fan-out from one parent never shares backing
// arrays). On a direct or indirect cycle, or when the chain would exceed
// maxDelegationDepth, it returns a non-empty actionable error message and
// the caller must not spawn the child session.
func validateDelegation(parent *session.Session, caller, target string) ([]string, string) {
	childLineage := append(parent.DelegationLineageSnapshot(), caller)
	if slices.Contains(childLineage, target) {
		path := strings.Join(append(slices.Clone(childLineage), target), " -> ")
		return nil, fmt.Sprintf(
			"delegation cycle detected: %s. Agent %s is already part of the active delegation chain; complete the task directly or delegate to a different agent.",
			path, target,
		)
	}
	if len(childLineage) > maxDelegationDepth {
		return nil, fmt.Sprintf(
			"delegation depth limit exceeded: agent %s is at delegation depth %d and delegating to %s would reach depth %d, exceeding the maximum of %d. Complete the task directly instead of delegating further.",
			caller, len(childLineage)-1, target, len(childLineage), maxDelegationDepth,
		)
	}
	return childLineage, ""
}

// buildTaskSystemMessage constructs the system message for a delegated task.
// attachedFiles, when non-empty, lists absolute paths of files the user
// attached to the parent conversation; they are surfaced to the sub-agent so
// it can use them directly without scanning the workspace or guessing from a
// bare filename.
func buildTaskSystemMessage(task, expectedOutput string, attachedFiles []string) string {
	var b strings.Builder
	b.WriteString("You are a member of a team of agents. Your goal is to complete the following task:")
	fmt.Fprintf(&b, "\n\n<task>\n%s\n</task>", task)
	if expectedOutput != "" {
		fmt.Fprintf(&b, "\n\n<expected_output>\n%s\n</expected_output>", expectedOutput)
	}
	if len(attachedFiles) > 0 {
		b.WriteString("\n\nThe user attached these files in the original conversation. They are available for you to read at these absolute paths; prefer them over any bare filenames mentioned in <task>:\n<attached_files>")
		for _, p := range attachedFiles {
			fmt.Fprintf(&b, "\n- %s", p)
		}
		b.WriteString("\n</attached_files>")
	}
	b.WriteString("\n\nIf the task references files, treat any absolute paths in <task> as authoritative and use them as-is. If a referenced file is given by name only (e.g. \"foo.go\"), do not guess: search the workspace or ask the calling agent for the absolute path before reading or modifying the file.")
	return b.String()
}

// SubSessionConfig describes the shape of a child session: system prompt,
// implicit user message, agent identity, tool approval, exclusions, etc.
// It is the data input to [newSubSession]; the orchestration around running
// such a session (telemetry, current-agent switching, event forwarding)
// lives in [LocalRuntime.runForwarding] and [LocalRuntime.runCollecting].
type SubSessionConfig struct {
	// Task is the user-facing task description.
	Task string
	// ExpectedOutput is an optional description of what the sub-agent should produce.
	ExpectedOutput string
	// SystemMessage, when non-empty, replaces the default task-based system
	// message. This is used by skill sub-agents whose system prompt is the
	// skill content itself rather than the team delegation boilerplate.
	SystemMessage string
	// AgentName is the name of the agent that will execute the sub-session.
	AgentName string
	// Title is a human-readable label for the sub-session (e.g. "Transferred task").
	Title string
	// ToolsApproved overrides whether tools are pre-approved in the child session.
	//
	// Deprecated: prefer SafetyPolicy; kept for callers that only know
	// the legacy blanket-approval flag.
	ToolsApproved bool
	// SafetyPolicy is the safety mode the child session inherits from
	// its parent, so a Balanced/Autonomous opt-in (or a Strict pin)
	// survives task transfers, skills, and background agents.
	SafetyPolicy session.SafetyPolicy
	// Permissions defines session-level tool permission overrides.
	Permissions *session.PermissionsConfig
	// NonInteractive marks the child session as running without a user present
	// (e.g. MCP server, A2A adapter, background agent). This causes the runtime
	// to auto-stop on max iterations instead of blocking for user input.
	NonInteractive bool
	// PinAgent, when true, pins the child session to AgentName via
	// session.WithAgentName. This is required for concurrent background
	// tasks that must not share the runtime's mutable currentAgent field.
	PinAgent bool
	// ImplicitUserMessage, when non-empty, overrides the default "Please proceed."
	// user message sent to the child session. This allows callers like skill
	// sub-agents to pass the task description as the user message.
	ImplicitUserMessage string
	// ExcludedTools lists tool names that should be filtered out of the agent's
	// tool list for the child session. This prevents recursive tool calls
	// (e.g. run_skill calling itself in a skill sub-session).
	ExcludedTools []string
	// AllowedTools, when non-empty, restricts the child session's inherited
	// agent tools to those whose names match an entry (glob or exact). Used by
	// fork-mode skills that declare an allowed-tools list. ExtraToolSets are
	// exempt from this filter.
	AllowedTools []string
	// ExtraToolSets are additional toolsets exposed in the child session on
	// top of the agent's own toolsets. Used by fork-mode skills that declare
	// assistive toolsets.
	ExtraToolSets []tools.ToolSet
	// DisableStructuredOutput exempts the child session from tool-mode
	// structured-output enforcement. Set only by fork-mode skills, whose
	// answer is free-form text for the calling agent; ordinary delegations
	// and background agents stay enforced.
	DisableStructuredOutput bool
	// DelegationLineage is the chain of agents that delegated to produce this
	// child session (parent lineage plus the delegating caller), as computed
	// by validateDelegation. Set only for true delegation edges
	// (transfer_task, run_background_agent); when nil the child inherits the
	// parent session's lineage unchanged, so non-delegation sub-sessions
	// (e.g. skills) preserve ancestry without adding an edge.
	DelegationLineage []string
}

// delegationRequest bundles a [SubSessionConfig] with the single
// orchestration knob [LocalRuntime.runForwarding] needs: whether to
// swap the runtime's current agent for the lifetime of the call.
//
// Adding a new "spawn a sub-agent" feature is a matter of building one
// of these and calling runForwarding (or runCollecting for the
// non-interactive variant); the boilerplate around AgentInfo events,
// agent restoration, and event forwarding stays in runForwarding.
//
// The OpenTelemetry span is owned by the caller (each public-facing
// handler opens its own span before calling runForwarding) so that
// pre-delegation work — most importantly the model override applied
// by [LocalRuntime.handleRunSkill] before forwarding — is recorded
// under the caller's span.
type delegationRequest struct {
	SubSessionConfig

	// CallerAgent is the agent that issued the delegating tool call,
	// snapshotted by the caller before the dispatcher's parallel fan-out.
	// runForwarding falls back to session resolution when it is nil.
	CallerAgent *agent.Agent

	// SwitchCurrentAgent, when true, swaps r.currentAgent to AgentName
	// for the lifetime of the call and emits AgentSwitching/AgentInfo
	// events on entry and exit. Used by transfer_task. Mutually
	// exclusive in spirit with PinAgent: pinning is for concurrent
	// sub-sessions that must NOT share the runtime's mutable
	// currentAgent, while switching is for the one delegation that owns it.
	//
	// runForwarding downgrades the switch to pinning the child to AgentName
	// in two cases. When the parent session is itself pinned (a background
	// agent's session), the shared current agent belongs to the concurrent
	// foreground loop and must not be mutated from a background task (#3886).
	// When a sibling delegation from the same parallel tool batch already
	// holds the switch, mutating it would misroute this child's turns (#4156).
	SwitchCurrentAgent bool
}

// newSubSession builds a *session.Session from a SubSessionConfig and a parent
// session. It consolidates the session options that were previously duplicated
// across handleTaskTransfer and RunAgent.
func newSubSession(parent *session.Session, cfg SubSessionConfig, childAgent *agent.Agent) *session.Session {
	// Sub-agents start in a fresh session, so they don't see the user's
	// original messages or attached files. Snapshot the parent's attached
	// files once and propagate them both to the system prompt (so the agent
	// is told about them) and to the child session (so further nested
	// transfers keep inheriting them).
	attachedFiles := parent.AttachedFilesSnapshot()

	sysMsg := cfg.SystemMessage
	if sysMsg == "" {
		sysMsg = buildTaskSystemMessage(cfg.Task, cfg.ExpectedOutput, attachedFiles)
	}

	userMsg := cfg.ImplicitUserMessage
	if userMsg == "" {
		userMsg = "Please proceed."
	}

	lineage := cfg.DelegationLineage
	if lineage == nil {
		lineage = parent.DelegationLineageSnapshot()
	}

	opts := []session.Opt{
		session.WithSystemMessage(sysMsg),
		session.WithImplicitUserMessage(userMsg),
		session.WithMaxIterations(childAgent.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(childAgent.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(childAgent.MaxOldToolCallTokens()),
		session.WithMaxToolResultTokens(childAgent.MaxToolResultTokens()),
		session.WithTitle(cfg.Title),
		session.WithToolsApproved(cfg.ToolsApproved),
		session.WithSafetyPolicy(cfg.SafetyPolicy),
		session.WithNonInteractive(cfg.NonInteractive),
		session.WithSendUserMessage(false),
		session.WithStructuredOutputDisabled(cfg.DisableStructuredOutput),
		session.WithParentID(parent.ID),
		session.WithAttachedFiles(attachedFiles),
		session.WithAttributes(parent.AttributesSnapshot()),
	}
	if cfg.PinAgent {
		opts = append(opts, session.WithAgentName(cfg.AgentName))
	}
	if len(lineage) > 0 {
		opts = append(opts, session.WithDelegationLineage(lineage))
	}
	opts = append(opts, session.WithPermissions(cfg.Permissions))
	// Merge parent's excluded tools with config's excluded tools so that
	// nested sub-sessions (e.g. skill → transfer_task → child) inherit
	// exclusions from all ancestors and don't re-introduce filtered tools.
	excludedTools := mergeExcludedTools(parent.ExcludedTools, cfg.ExcludedTools)
	if len(excludedTools) > 0 {
		opts = append(opts, session.WithExcludedTools(excludedTools))
	}
	if len(cfg.AllowedTools) > 0 {
		opts = append(opts, session.WithAllowedTools(cfg.AllowedTools))
	}
	if len(cfg.ExtraToolSets) > 0 {
		opts = append(opts, session.WithExtraToolSets(cfg.ExtraToolSets))
	}
	return session.New(opts...)
}

// mergeExcludedTools combines two excluded-tool lists, deduplicating entries.
// It returns nil when both inputs are empty.
func mergeExcludedTools(parent, child []string) []string {
	if len(parent) == 0 {
		return child
	}
	if len(child) == 0 {
		return parent
	}
	set := make(map[string]struct{}, len(parent)+len(child))
	for _, t := range parent {
		set[t] = struct{}{}
	}
	for _, t := range child {
		set[t] = struct{}{}
	}
	merged := make([]string, 0, len(set))
	for t := range set {
		merged = append(merged, t)
	}
	return merged
}

// swapCurrentAgent swaps the runtime's current agent from `from` to `to`,
// emitting the AgentSwitching/AgentInfo events and invoking the on_agent_switch
// hooks on entry, and returns a closure that reverses everything (restores
// `from`, emits the counterpart events and the matching return-side hooks)
// when invoked.
//
// Use as `defer r.swapCurrentAgent(ctx, sessionID, from, to, evts)()` so the
// swap takes effect immediately and the restore runs at function exit.
func (r *LocalRuntime) swapCurrentAgent(ctx context.Context, sessionID string, from, to *agent.Agent, evts EventSink) func() {
	evts.Emit(AgentSwitching(true, from.Name(), to.Name()))
	r.executeOnAgentSwitchHooks(ctx, from, sessionID, from.Name(), to.Name(), agentSwitchKindTransferTask)
	r.setCurrentAgent(to.Name())
	evts.Emit(AgentInfo(to.Name(), agentModelLabel(ctx, to), to.Description(), to.WelcomeMessage()))
	return func() {
		r.setCurrentAgent(from.Name())
		evts.Emit(AgentSwitching(false, to.Name(), from.Name()))
		r.executeOnAgentSwitchHooks(ctx, from, sessionID, to.Name(), from.Name(), agentSwitchKindTransferTaskReturn)
		evts.Emit(AgentInfo(from.Name(), agentModelLabel(ctx, from), from.Description(), from.WelcomeMessage()))
	}
}

// runForwarding manages the lifecycle of a blocking sub-session, forwarding
// events to evts. The child's approval state stays scoped to the sub-session
// and never flows back to the parent. This is the "interactive" path used by transfer_task and
// run_skill: the parent loop is blocked while the child executes, and
// the user sees the child's events live.
//
// On success it returns a tool result whose output is the child's last
// assistant message. On error it has already forwarded the ErrorEvent to
// evts and returns a wrapped error.
//
// The caller is expected to have opened a tracing span before calling
// runForwarding; the function records sub-session status (Ok / Error)
// on whatever span is attached to ctx — a no-op if none.
//
// runForwarding handles every concern the callers used to duplicate:
// resolving the caller from the parent session, swapping the current agent
// (if requested; downgraded to pinning the child when the parent session is
// itself pinned), resolving the child agent, building the sub-session,
// driving RunStream, and recording the sub-session on the parent.
func (r *LocalRuntime) runForwarding(ctx context.Context, parent *session.Session, evts EventSink, req delegationRequest) (*tools.ToolCallResult, error) {
	span := trace.SpanFromContext(ctx)

	// The caller never resolves from the shared current agent here: a nested
	// transfer from a pinned background session must attribute events, hooks,
	// and completion to the pinned agent (#3886), and a sibling call in the
	// same parallel batch may already have swapped it (#4156).
	callerAgent := req.CallerAgent
	if callerAgent == nil {
		callerAgent = r.resolveSessionAgent(parent)
	}
	if callerAgent == nil {
		return nil, errors.New("no agent resolved for the parent session")
	}
	child, err := r.team.Agent(req.AgentName)
	if err != nil {
		return nil, err
	}

	if req.SwitchCurrentAgent {
		switch {
		case parent.AgentName != "":
			// Pinned parent (background delegation): the shared current
			// agent belongs to the concurrent foreground loop and must not
			// be mutated. Pin the child to the target instead — RunStream
			// resolves pinned sessions directly, so the child still
			// executes as the target agent, without switch events/hooks.
			req.PinAgent = true
		case r.agentSwitchInFlight.CompareAndSwap(false, true):
			defer r.agentSwitchInFlight.Store(false)
			defer r.swapCurrentAgent(ctx, parent.ID, callerAgent, child, evts)()
		default:
			// A sibling delegation from the same parallel tool batch owns the
			// shared current agent. Stomping it would make this child's turns
			// resolve to the wrong agent, so pin the child instead (#4156).
			req.PinAgent = true
		}
	}

	s := newSubSession(parent, req.SubSessionConfig, child)

	// subagent_stop fires after the child's stream has fully drained,
	// using the *parent* agent's executor so handlers configured on the
	// orchestrator see every child completion in one place — success or
	// failure. The deferred call ensures we don't lose the event when an
	// ErrorEvent triggers an early return below; handlers can detect a
	// failed run by an empty stop_response (or by correlating with the
	// session-level error event the parent already received).
	defer func() {
		r.executeSubagentStopHooks(ctx, parent, s, callerAgent, req.AgentName, s.GetLastAssistantMessageContent())
	}()

	childEvents := r.RunStream(ctx, s)
	var subSessionErr error
	for event := range childEvents {
		evts.Emit(event)
		if errEvent, ok := event.(*ErrorEvent); ok && subSessionErr == nil {
			// Capture the first ErrorEvent but keep draining the channel so
			// the sub-session's full transcript still streams through. The
			// child's run loop may emit additional events (e.g. notifications,
			// hook output) after the error before its channel closes; dropping
			// them here would leave the TUI's streamDepth counter unbalanced
			// and the user without context for what actually went wrong.
			subSessionErr = fmt.Errorf("%s", errEvent.Error)
		}
	}

	// Persist the sub-session unconditionally — even on error, the partial
	// transcript is the most valuable artifact for debugging. The persistence
	// pipeline relies on SubSessionCompleted to write the sub-session's
	// messages to the store; without this emission they are silently dropped.
	parent.AddLiveSubSession(s)
	evts.Emit(SubSessionCompleted(parent.ID, s, callerAgent.Name()))

	if subSessionErr != nil {
		span.RecordError(subSessionErr)
		span.SetStatus(codes.Error, "sub-session error")
		return nil, subSessionErr
	}

	span.SetStatus(codes.Ok, "sub-session completed")
	return tools.ResultSuccess(s.GetLastAssistantMessageContent()), nil
}

// runCollecting runs a child session and collects its output via an
// optional content callback instead of forwarding events. This is the
// non-interactive path used by background agents: there's no live UI, so
// events are dropped and only the final assistant message (or the first
// error) matters.
//
// Unlike runForwarding it does not emit AgentSwitching/AgentInfo events:
// callers like background agents PinAgent the child session so the
// runtime never mutates the shared currentAgent state.
func (r *LocalRuntime) runCollecting(ctx context.Context, parent *session.Session, caller *agent.Agent, cfg SubSessionConfig, onContent func(string)) *agenttool.RunResult {
	// Resolved by the caller before dispatch, never from the shared current
	// agent: a nested background dispatch from a pinned session must attribute
	// the child's completion to the pinned agent (#3886), and the shared field
	// may be swapped mid-run by a concurrent delegation (#4156).
	callerAgent := caller
	if callerAgent == nil {
		callerAgent = r.resolveSessionAgent(parent)
	}
	if callerAgent == nil {
		return &agenttool.RunResult{ErrMsg: "no agent resolved for the parent session"}
	}
	child, err := r.team.Agent(cfg.AgentName)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", cfg.AgentName, err)}
	}

	s := newSubSession(parent, cfg, child)

	// subagent_stop fires after the background sub-session has fully
	// drained — success or failure. The caller agent (whoever dispatched
	// run_background_agent) owns the executor. The deferred call ensures
	// the hook fires even when an ErrorEvent or ctx cancellation breaks
	// us out of the loop.
	defer func() {
		r.executeSubagentStopHooks(ctx, parent, s, callerAgent, cfg.AgentName, s.GetLastAssistantMessageContent())
	}()

	var errMsg string
	events := r.RunStream(ctx, s)
	for event := range events {
		if ctx.Err() != nil {
			break
		}
		if choice, ok := event.(*AgentChoiceEvent); ok && choice.Content != "" {
			if onContent != nil {
				onContent(choice.Content)
			}
		}
		// Token usage is the one event a background sub-session surfaces
		// out-of-band: it carries the sub-session id and agent name, so the
		// UI can keep per-agent context accounting for background agents.
		if usage, ok := event.(*TokenUsageEvent); ok {
			r.emitBackgroundEvent(usage)
		}
		// Elicitation requests are NOT re-forwarded here: elicitationHandler
		// already delivered this event to the OnElicitationRequest sink
		// directly, synchronously, and exactly once (#3584). Forwarding it
		// again here — as the bridge's best-effort copy happens to flow
		// through this same channel when this background sub-session
		// currently owns the bridge slot — used to cause a second sink
		// delivery for the same request, which only a stateful App-side
		// dedupe (since removed) papered over. This branch is intentionally
		// absent; ElicitationRequestEvents seen here are simply ignored.
		if errEvt, ok := event.(*ErrorEvent); ok {
			errMsg = errEvt.Error
			break
		}
	}
	// Drain remaining events so the RunStream goroutine can complete and
	// close the channel without blocking on a full buffer.
	for range events {
	}

	// The loop above stops forwarding on ctx cancellation / first ErrorEvent,
	// so the drain can discard TokenUsageEvents carrying the child's latest
	// recorded usage. Emit one authoritative final snapshot before the child
	// is attached: AddLiveSubSession marks it live-attached, so the parent's
	// own events will never fold this cost back in. UI snapshots replace by
	// session ID, which makes the duplicate on the clean path harmless.
	// A child that failed before recording any usage or cost gets no
	// snapshot at all: a zero-usage event would only add an empty
	// sub-session row to the UI and clobber per-agent context accounting.
	// Check that before resolving the context limit — the model lookup is
	// pure overhead for a snapshot that is never emitted.
	finalUsage := SessionUsage(s, 0, child.CompactionThreshold())
	usageCtx := context.WithoutCancel(ctx)
	if finalUsage.ContextLength > 0 || finalUsage.Cost > 0 {
		// usageCtx: the context-limit lookup must still resolve for a
		// cancelled task.
		finalUsage.ContextLimit = r.contextLimitForAgentModel(usageCtx, child, r.getEffectiveModelID(usageCtx, child))
		r.emitBackgroundEvent(NewTokenUsageEvent(s.ID, cfg.AgentName, finalUsage))
	}

	// Persist the sub-session unconditionally — the partial transcript is
	// the most valuable artifact for debugging a failed background agent.
	// AddLiveSubSession records it in-memory on both the success and error
	// paths.
	parent.AddLiveSubSession(s)

	// Mirror runForwarding's persistence, but write to the store directly
	// instead of emitting SubSessionCompleted: runCollecting runs on a
	// detached background goroutine, so routing through the shared observer
	// chain would race the parent's live RunStream (the PersistenceObserver
	// keeps unsynchronised streaming state). Without this the background
	// sub-session never reaches the store — its tokens and cost are recorded
	// as $0 and escape any spend accounting that reads the store. usageCtx is
	// already detached, so a cancelled/stopped task still persists its
	// transcript.
	r.persistBackgroundSubSession(usageCtx, parent.ID, s)

	if errMsg != "" {
		return &agenttool.RunResult{ErrMsg: errMsg}
	}

	result := s.GetLastAssistantMessageContent()
	// A remote MCP toolset that needs first-time interactive OAuth fails fast
	// in a background (non-interactive) session instead of hanging on an
	// unanswerable elicitation (issue #3200). The resulting "needs auth" state
	// never reaches the orchestrating model on its own — runCollecting drops
	// warning events — so the sub-agent would silently lack a capability with
	// no explanation. Prepend an actionable note so the model (and, through it,
	// the user) learns the server must be authorized interactively first.
	if note := backgroundAuthRequiredNote(child); note != "" {
		result = prependNote(result, note)
	}
	// Mid-call elicitations that were auto-declined because this background
	// session had no UI to answer them (see elicitationHandler) are recorded
	// against this sub-session's ID; surface them the same way (#3584).
	for _, note := range r.elicitationDeclines.drain(s.ID) {
		result = prependNote(result, note)
	}
	return &agenttool.RunResult{Result: result}
}

// prependNote prepends note to result, separated by a blank line, handling
// the case where either side is empty. Used to surface model-readable
// context (OAuth-required, elicitation auto-declined) ahead of a background
// sub-session's actual response.
func prependNote(result, note string) string {
	if note == "" {
		return result
	}
	if result == "" {
		return note
	}
	return note + "\n\n" + result
}

// backgroundAuthRequiredNote returns a model-readable note naming the child
// agent's MCP toolsets that could not start because they require first-time
// interactive OAuth authorization, which a background agent cannot complete
// (issue #3200). It returns "" when no toolset is in that state. Detection is
// typed (tools.IsAuthorizationRequired against the toolset's recorded
// LastError) rather than string-matched, so it stays correct if the user-facing
// error text changes.
func backgroundAuthRequiredNote(child *agent.Agent) string {
	var needAuth []string
	for _, ts := range child.ToolSets() {
		status := toolsetStatusFor(ts)
		if tools.IsAuthorizationRequired(status.LastError) {
			needAuth = append(needAuth, status.Name)
		}
	}
	if len(needAuth) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Note: the following MCP tool server(s) could not be used because they "+
			"require first-time interactive OAuth authorization, which cannot be "+
			"completed by a background agent: %s. Ask the user to authorize them "+
			"once from an interactive session (run the agent in the foreground and "+
			"approve the OAuth prompt), then retry this task.",
		strings.Join(needAuth, ", "),
	)
}

// persistBackgroundSubSession writes a completed background sub-session to the
// session store, linking it under parentID. It is the runCollecting analogue
// of the SubSessionCompletedEvent that runForwarding emits: background tasks
// have no live EventSink, so the persistence observer never sees them. Errors
// are logged rather than surfaced — a failed persist must not change the tool
// result the caller returns to the model.
func (r *LocalRuntime) persistBackgroundSubSession(ctx context.Context, parentID string, sub *session.Session) {
	if r.sessionStore == nil {
		return
	}
	if err := r.sessionStore.AddSubSession(ctx, parentID, sub); err != nil {
		slog.WarnContext(ctx, "Failed to persist background sub-session",
			"parent_id", parentID, "sub_session_id", sub.ID, "error", err)
	}
}

// SubAgentNames implements agenttool.SessionSubAgentResolver, which
// HandleRun prefers over the legacy CurrentAgentSubAgentNames. The
// sub-agent list resolves from the calling session — a pinned background
// session yields its pinned agent — so a nested run_background_agent
// dispatched from a detached background task is validated against the
// actual caller, not whatever the concurrent foreground loop's shared
// current agent points at (#3886).
func (r *LocalRuntime) SubAgentNames(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	a := r.resolveSessionAgent(sess)
	if a == nil {
		return nil
	}
	return agentNames(a.SubAgents())
}

// CurrentAgentSubAgentNames implements agenttool.Runner. It is the legacy
// shared current-agent resolver, kept so the Runner contract stays
// source-compatible; HandleRun never takes this path for LocalRuntime
// because the session-aware SubAgentNames above is preferred.
func (r *LocalRuntime) CurrentAgentSubAgentNames() []string {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	return agentNames(a.SubAgents())
}

// RunAgent implements agenttool.Runner. It starts a sub-agent synchronously
// and blocks until completion or cancellation.
//
// Background tasks inherit the parent session's permissions because there is no user
// present to respond to interactive approval prompts during async execution.
// Tool calls that result in an "Ask" outcome will be auto-denied by the dispatcher
// due to the non-interactive context.
func (r *LocalRuntime) RunAgent(ctx context.Context, params agenttool.RunParams) *agenttool.RunResult {
	// Caller identity must come from the parent session, not the shared
	// current agent: nested background delegation runs on pinned sessions
	// whose agent can differ from whatever the foreground loop points at.
	caller := r.resolveSessionAgent(params.ParentSession)
	if caller == nil {
		return &agenttool.RunResult{ErrMsg: "no agent resolved for the parent session"}
	}
	childLineage, guardErr := validateDelegation(params.ParentSession, caller.Name(), params.AgentName)
	if guardErr != "" {
		return &agenttool.RunResult{ErrMsg: guardErr}
	}
	return r.runCollecting(ctx, params.ParentSession, caller, SubSessionConfig{
		Task:              params.Task,
		ExpectedOutput:    params.ExpectedOutput,
		AgentName:         params.AgentName,
		Title:             "Background agent task",
		ToolsApproved:     params.ParentSession.IsToolsApproved(),
		SafetyPolicy:      params.ParentSession.GetSafetyPolicy(),
		Permissions:       params.ParentSession.ClonePermissions(),
		NonInteractive:    true,
		PinAgent:          true,
		DelegationLineage: childLineage,
	}, params.OnContent)
}

func (r *LocalRuntime) handleTaskTransfer(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts EventSink, rt tools.Runtime) (*tools.ToolCallResult, error) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Resolve the caller from the dispatcher's batch snapshot: a nested
	// transfer_task from a pinned background session must attribute the call to
	// the pinned agent (#3886), and a sibling transfer_task in the same parallel
	// batch may already have swapped the shared current agent (#4156).
	a := r.callerAgent(rt, sess)
	if a == nil {
		return nil, errors.New("no agent resolved for the calling session")
	}
	if errResult := validateAgentInList(a.Name(), params.Agent, "transfer task to", "sub-agents list", a.SubAgents()); errResult != nil {
		return errResult, nil
	}

	childLineage, guardErr := validateDelegation(sess, a.Name(), params.Agent)
	if guardErr != "" {
		return tools.ResultError(guardErr), nil
	}

	slog.DebugContext(ctx, "Transferring task to agent", "from_agent", a.Name(), "to_agent", params.Agent, "task", params.Task)

	delegationAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationInvokeAgent),
		// gen_ai.agent.name identifies the target agent of the invoke_agent
		// operation per the OTel GenAI semconv (Required). cagent.agent.name
		// is the same value but in our internal namespace; we emit both so
		// spec-aware backends and existing cagent dashboards both see it.
		attribute.String(genai.AttrAgentName, params.Agent),
		attribute.String("cagent.delegation.from_agent", a.Name()),
		attribute.String("cagent.delegation.to_agent", params.Agent),
		attribute.String("cagent.delegation.kind", "transfer_task"),
		attribute.String(genai.AttrConversationID, sess.ID),
		attribute.String(genai.AttrAgentNameRuntime, params.Agent),
	}
	if params.Task != "" {
		// Task length is bounded enough to be useful as a span
		// attribute for debugging "agent X transferred which task
		// to Y". The full task body lands on the sub-session's
		// runtime.session span when content capture is opt-in.
		delegationAttrs = append(delegationAttrs, attribute.Int("cagent.delegation.task_length", len(params.Task)))
	}
	if genai.EmitLegacyAttributes() {
		delegationAttrs = append(delegationAttrs,
			attribute.String("from.agent", a.Name()),
			attribute.String("to.agent", params.Agent),
			attribute.String("session.id", sess.ID),
		)
	}
	ctx, span := r.startSpan(ctx, "runtime.task_transfer", trace.WithAttributes(delegationAttrs...))
	defer span.End()

	return r.runForwarding(ctx, sess, evts, delegationRequest{
		SubSessionConfig: SubSessionConfig{
			Task:              params.Task,
			ExpectedOutput:    params.ExpectedOutput,
			AgentName:         params.Agent,
			Title:             "Transferred task",
			ToolsApproved:     sess.IsToolsApproved(),
			SafetyPolicy:      sess.GetSafetyPolicy(),
			Permissions:       sess.ClonePermissions(),
			NonInteractive:    sess.NonInteractive,
			DelegationLineage: childLineage,
		},
		CallerAgent:        a,
		SwitchCurrentAgent: true,
	})
}

func (r *LocalRuntime) handleHandoff(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var params handoff.Args
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	ca := r.currentAgentName()
	currentAgent, err := r.team.Agent(ca)
	if err != nil {
		return nil, fmt.Errorf("current agent not found: %w", err)
	}

	if errResult := validateAgentInList(ca, params.Agent, "hand off to", "handoffs list", currentAgent.Handoffs()); errResult != nil {
		return errResult, nil
	}

	next, err := r.team.Agent(params.Agent)
	if err != nil {
		return nil, err
	}

	// Handoff is in-place agent swap (same session, different agent
	// from the next turn). Span name keeps the runtime.* family;
	// attributes mirror the transfer_task span shape so dashboards
	// can union both delegation kinds. Take the returned ctx so
	// `executeOnAgentSwitchHooks` and any of its children parent
	// onto this span instead of bypassing it.
	ctx, span := r.startSpan(ctx, "runtime.handoff", trace.WithAttributes(
		attribute.String(genai.AttrOperationName, genai.OperationInvokeAgent),
		// gen_ai.agent.name — Required by OTel GenAI semconv on invoke_agent
		// spans; identifies the agent being handed off to. See task_transfer
		// for the rationale of dual-emitting alongside cagent.agent.name.
		attribute.String(genai.AttrAgentName, next.Name()),
		attribute.String("cagent.delegation.from_agent", ca),
		attribute.String("cagent.delegation.to_agent", next.Name()),
		attribute.String("cagent.delegation.kind", "handoff"),
		attribute.String(genai.AttrConversationID, sess.ID),
		attribute.String(genai.AttrAgentNameRuntime, next.Name()),
	))
	defer span.End()

	r.executeOnAgentSwitchHooks(ctx, currentAgent, sess.ID, ca, next.Name(), agentSwitchKindHandoff)
	r.setCurrentAgent(next.Name())
	handoffMessage := "The agent " + ca + " handed off the conversation to you. " +
		"Your available handoff agents and tools are specified in the system messages that follow. " +
		"Only use those capabilities - do not attempt to use tools or hand off to agents that you see " +
		"in the conversation history from previous agents, as those were available to different agents " +
		"with different capabilities. Look at the conversation history for context, but only use the " +
		"handoff agents and tools that are listed in your system messages below. " +
		"Complete your part of the task and hand off to the next appropriate agent in your workflow " +
		"(if any are available to you), or respond directly to the user if you are the final agent."
	return tools.ResultSuccess(handoffMessage), nil
}

// applyForceHandoff routes the conversation to the agent's configured
// force_handoff target after a natural stop, bypassing the LLM's
// tool-calling entirely. The conversation context carries over because
// the same session keeps running; an implicit user message tells the
// target agent what happened so the next model call doesn't start on a
// dangling assistant message. The caller (runTurn) is responsible for
// continuing the run loop, where the next iteration re-resolves the
// current agent and emits the AgentInfo event.
func (r *LocalRuntime) applyForceHandoff(ctx context.Context, sess *session.Session, from, to *agent.Agent) {
	slog.InfoContext(ctx, "Forced handoff", "from_agent", from.Name(), "to_agent", to.Name(), "session_id", sess.ID)

	r.executeOnAgentSwitchHooks(ctx, from, sess.ID, from.Name(), to.Name(), agentSwitchKindForceHandoff)
	r.setCurrentAgent(to.Name())

	sess.AddMessage(session.ImplicitUserMessage(
		"The agent " + from.Name() + " finished its response and the conversation was automatically " +
			"handed off to you. Your available handoff agents and tools are specified in the system " +
			"messages that follow. Only use those capabilities - do not attempt to use tools or hand " +
			"off to agents that you see in the conversation history from previous agents, as those were " +
			"available to different agents with different capabilities. Look at the conversation history " +
			"for context, continue the work from where the previous agent stopped, and complete your " +
			"part of the task.",
	))
}
