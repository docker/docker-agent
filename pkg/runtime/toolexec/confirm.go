package toolexec

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// Gate approves and runs an action that is not a tool call of its own — today
// a shell command embedded in a skill body, which the skills toolset has to
// run on the user's machine to expand the skill. Skill bodies are untrusted
// data, so such a command must never run unannounced.
//
// The action is presented as a tool call: it goes through the same safety
// classification, hooks, permission rules and confirmation prompt as a real
// call, and shows up in the transcript with its output. Nothing is recorded in
// the conversation though — the requesting tool call owns that slot, so a
// denial is reported to the caller instead of to the model.
type Gate struct {
	// Sess and Agent identify who the action is attributed to. Required.
	Sess  *session.Session
	Agent *agent.Agent

	// Emitter publishes the confirmation and the resulting tool-call item.
	// Required.
	Emitter Emitter

	// Resume receives the user's confirmation response. Required for
	// interactive sessions.
	Resume <-chan ResumeRequest

	// Hooks receives the "waiting for the user" and approval-decision
	// notifications. May be nil.
	Hooks HookDispatcher

	// Permissions returns the permission checkers to evaluate, in the same
	// order the dispatcher uses (session tier first). Called for every
	// decision so a grant made while this gate waits for [Gate.Mu] is seen.
	// May be nil.
	Permissions func(*session.Session) []NamedChecker

	// Mu serialises prompts with the ones the dispatcher raises for real
	// tool calls: [ResumeRequest] carries no tool-call ID, so only one
	// prompt may be outstanding at a time. May be nil when the caller has
	// no dispatcher to share a mutex with.
	Mu *sync.Mutex
}

// ConfirmAndRun implements the [tools.Runtime] side of the gate.
func (g *Gate) ConfirmAndRun(ctx context.Context, run tools.ConfirmedRun, exec func(context.Context, tools.ConfirmedRun) (string, error)) (string, error) {
	tc, tool, err := syntheticCall(run)
	if err != nil {
		return "", err
	}
	d := &Dispatcher{
		Hooks:          g.Hooks,
		Resume:         g.Resume,
		Permissions:    g.Permissions,
		confirmationMu: g.Mu,
	}
	if d.confirmationMu == nil {
		d.confirmationMu = &sync.Mutex{}
	}
	c := &call{
		d:         d,
		sess:      g.Sess,
		em:        g.Emitter,
		a:         g.Agent,
		tc:        tc,
		tool:      tool,
		available: true,
		outOfBand: true,
	}

	approved := false
	outcome := c.approveAndRun(ctx, func() CallOutcome {
		approved = true
		return CallOutcome{}
	})
	if !approved {
		if outcome.Canceled && ctx.Err() != nil {
			return "", ctx.Err()
		}
		if c.lastError != "" {
			return "", fmt.Errorf("%w: %s", tools.ErrConfirmationDenied, c.lastError)
		}
		return "", tools.ErrConfirmationDenied
	}

	// Input hooks may rewrite the action. For skill command expansion,
	// execute the approved command rather than the original closure's command.
	approvedRun := tools.ConfirmedRun{
		ToolName: run.ToolName,
		Args:     ParseToolInput(c.tc.Function.Arguments),
		Metadata: run.Metadata,
	}

	c.startOutOfBand()
	output, execErr := exec(ctx, approvedRun)
	if execErr != nil {
		output = execErr.Error()
	}
	res := &tools.ToolCallResult{Output: output, IsError: execErr != nil}
	res.Output = c.applyToolResponseTransform(ctx, res.Output, res.IsError)
	g.emitResponse(c.tc, tool, res)
	if stop, message := c.postHook(ctx, res); stop {
		return "", &StopRunError{Message: message}
	}
	if execErr != nil {
		return "", &actionError{message: res.Output, cause: execErr}
	}
	return res.Output, nil
}

type actionError struct {
	message string
	cause   error
}

func (e *actionError) Error() string { return e.message }
func (e *actionError) Unwrap() error { return e.cause }

func (g *Gate) emitResponse(tc tools.ToolCall, tool tools.Tool, res *tools.ToolCallResult) {
	g.Emitter.EmitToolCallResponse(tc.ID, tool, res, res.Output, g.Agent.Name())
}

// StopRunError reports that a nested action's post_tool_use hook requested
// termination of the parent tool call.
type StopRunError struct {
	Message string
}

func (e *StopRunError) Error() string {
	return "post_tool_use hook stopped the skill command: " + e.Message
}

func (e *StopRunError) AbortExpansion() {}

// syntheticCall renders an out-of-band action as the tool call it is presented
// as. The generated ID ties the confirmation, the running item and the result
// together in the transcript; it is never sent to the model.
func syntheticCall(run tools.ConfirmedRun) (tools.ToolCall, tools.Tool, error) {
	args, err := json.Marshal(run.Args)
	if err != nil {
		return tools.ToolCall{}, tools.Tool{}, fmt.Errorf("marshal confirmed-run arguments: %w", err)
	}
	tc := tools.ToolCall{
		ID:       "confirm_" + uuid.NewString(),
		Type:     "function",
		Function: tools.FunctionCall{Name: run.ToolName, Arguments: string(args)},
	}
	tool := tools.Tool{Name: run.ToolName, Metadata: run.Metadata}
	if run.ToolName == safety.ShellToolName {
		tool.Category = "shell"
	}
	return tc, tool, nil
}
