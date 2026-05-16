package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
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
	ToolsApproved bool
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

	// SwitchCurrentAgent, when true, swaps r.currentAgent to AgentName
	// for the lifetime of the call and emits AgentSwitching/AgentInfo
	// events on entry and exit. Used by transfer_task. Mutually
	// exclusive in spirit with PinAgent: pinning is for concurrent
	// sub-sessions that must NOT share the runtime's mutable
	// currentAgent, while switching is for sequential delegations where
	// the parent loop is blocked anyway.
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

	opts := []session.Opt{
		session.WithSystemMessage(sysMsg),
		session.WithImplicitUserMessage(userMsg),
		session.WithMaxIterations(childAgent.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(childAgent.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(childAgent.MaxOldToolCallTokens()),
		session.WithTitle(cfg.Title),
		session.WithToolsApproved(cfg.ToolsApproved),
		session.WithNonInteractive(cfg.NonInteractive),
		session.WithSendUserMessage(false),
		session.WithParentID(parent.ID),
		session.WithAttachedFiles(attachedFiles),
	}
	if cfg.PinAgent {
		opts = append(opts, session.WithAgentName(cfg.AgentName))
	}
	// Merge parent's excluded tools with config's excluded tools so that
	// nested sub-sessions (e.g. skill → transfer_task → child) inherit
	// exclusions from all ancestors and don't re-introduce filtered tools.
	excludedTools := mergeExcludedTools(parent.ExcludedTools, cfg.ExcludedTools)
	if len(excludedTools) > 0 {
		opts = append(opts, session.WithExcludedTools(excludedTools))
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
	evts.Emit(AgentInfo(to.Name(), getAgentModelID(to).String(), to.Description(), to.WelcomeMessage()))
	return func() {
		r.setCurrentAgent(from.Name())
		evts.Emit(AgentSwitching(false, to.Name(), from.Name()))
		r.executeOnAgentSwitchHooks(ctx, from, sessionID, to.Name(), from.Name(), agentSwitchKindTransferTaskReturn)
		evts.Emit(AgentInfo(from.Name(), getAgentModelID(from).String(), from.Description(), from.WelcomeMessage()))
	}
}

// runForwarding runs a child session synchronously, forwarding all of its
// events to evts and propagating tool-approval state back to the parent
// on completion. This is the "interactive" path used by transfer_task and
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
// swapping the current agent (if requested), resolving the child agent,
// building the sub-session, driving RunStream, and recording the
// sub-session on the parent.
func (r *LocalRuntime) runForwarding(ctx context.Context, parent *session.Session, evts EventSink, req delegationRequest) (*tools.ToolCallResult, error) {
	span := trace.SpanFromContext(ctx)

	callerAgent, err := r.team.Agent(r.CurrentAgentName())
	if err != nil {
		return nil, fmt.Errorf("current agent not found: %w", err)
	}
	child, err := r.team.Agent(req.AgentName)
	if err != nil {
		return nil, err
	}

	if req.SwitchCurrentAgent {
		defer r.swapCurrentAgent(ctx, parent.ID, callerAgent, child, evts)()
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
	for event := range childEvents {
		evts.Emit(event)
		if errEvent, ok := event.(*ErrorEvent); ok {
			// Drain remaining events (including StreamStoppedEvent) so the
			// TUI's streamDepth counter stays balanced.
			for remaining := range childEvents {
				evts.Emit(remaining)
			}
			err := fmt.Errorf("%s", errEvent.Error)
			span.RecordError(err)
			span.SetStatus(codes.Error, "sub-session error")
			return nil, err
		}
	}

	parent.ToolsApproved = s.ToolsApproved
	parent.AddSubSession(s)
	evts.Emit(SubSessionCompleted(parent.ID, s, callerAgent.Name()))

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
func (r *LocalRuntime) runCollecting(ctx context.Context, parent *session.Session, cfg SubSessionConfig, onContent func(string)) *agenttool.RunResult {
	child, err := r.team.Agent(cfg.AgentName)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", cfg.AgentName, err)}
	}

	s := newSubSession(parent, cfg, child)

	// subagent_stop fires after the background sub-session has fully
	// drained — success or failure. The parent agent at the time of
	// dispatch (whoever called run_background_agent) owns the executor;
	// we resolve it via CurrentAgent because the background path doesn't
	// carry the parent agent name. dispatchHook silently no-ops when
	// CurrentAgent is nil. The deferred call ensures the hook fires even
	// when an ErrorEvent or ctx cancellation breaks us out of the loop.
	defer func() {
		r.executeSubagentStopHooks(ctx, parent, s, r.CurrentAgent(), cfg.AgentName, s.GetLastAssistantMessageContent())
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
		if errEvt, ok := event.(*ErrorEvent); ok {
			errMsg = errEvt.Error
			break
		}
	}
	// Drain remaining events so the RunStream goroutine can complete and
	// close the channel without blocking on a full buffer.
	for range events {
	}

	if errMsg != "" {
		return &agenttool.RunResult{ErrMsg: errMsg}
	}

	result := s.GetLastAssistantMessageContent()
	parent.AddSubSession(s)

	return &agenttool.RunResult{Result: result}
}

// CurrentAgentSubAgentNames implements agenttool.Runner.
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
// Background tasks run with tools pre-approved because there is no user
// present to respond to interactive approval prompts during async
// execution. This is a deliberate design trade-off: the user implicitly
// authorises all tool calls made by the sub-agent when they approve
// run_background_agent. Callers should be aware that prompt injection in
// the sub-agent's context could exploit this gate-bypass.
//
// TODO: propagate the parent session's per-tool permission rules once the
// runtime supports per-session permission scoping rather than a single
// shared ToolsApproved flag.
func (r *LocalRuntime) RunAgent(ctx context.Context, params agenttool.RunParams) *agenttool.RunResult {
	return r.runCollecting(ctx, params.ParentSession, SubSessionConfig{
		Task:           params.Task,
		ExpectedOutput: params.ExpectedOutput,
		AgentName:      params.AgentName,
		Title:          "Background agent task",
		ToolsApproved:  true,
		NonInteractive: true,
		PinAgent:       true,
	}, params.OnContent)
}

func (r *LocalRuntime) handleTaskTransfer(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts EventSink) (*tools.ToolCallResult, error) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	a := r.CurrentAgent()
	if errResult := validateAgentInList(a.Name(), params.Agent, "transfer task to", "sub-agents list", a.SubAgents()); errResult != nil {
		return errResult, nil
	}

	slog.DebugContext(ctx, "Transferring task to agent", "from_agent", a.Name(), "to_agent", params.Agent, "task", params.Task)

	ctx, span := r.startSpan(ctx, "runtime.task_transfer", trace.WithAttributes(
		attribute.String("from.agent", a.Name()),
		attribute.String("to.agent", params.Agent),
		attribute.String("session.id", sess.ID),
	))
	defer span.End()

	return r.runForwarding(ctx, sess, evts, delegationRequest{
		SubSessionConfig: SubSessionConfig{
			Task:           params.Task,
			ExpectedOutput: params.ExpectedOutput,
			AgentName:      params.Agent,
			Title:          "Transferred task",
			ToolsApproved:  sess.ToolsApproved,
			NonInteractive: sess.NonInteractive,
		},
		SwitchCurrentAgent: true,
	})
}

func (r *LocalRuntime) handleHandoff(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var params handoff.Args
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	ca := r.CurrentAgentName()
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
