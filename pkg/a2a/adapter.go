package a2a

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	cgenai "github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/version"
)

// newDockerAgentAdapter creates a new ADK agent adapter from a docker agent team and agent name.
// When agentName is empty, the team's default agent (one explicitly named "root" if it
// exists, otherwise the first agent declared) is used.
func newDockerAgentAdapter(t *team.Team, agentName string, sessStore session.Store, safety servesafety.Resolved, workingDir string, reg *approvalRegistry) (agent.Agent, error) {
	a, err := t.AgentOrDefault(agentName)
	if err != nil {
		return nil, fmt.Errorf("failed to get agent %s: %w", agentName, err)
	}
	agentName = a.Name()

	desc := cmp.Or(a.Description(), "Agent "+agentName)

	return agent.New(agent.Config{
		Name:        agentName,
		Description: desc,
		Run: func(ctx agent.InvocationContext) iter.Seq2[*adksession.Event, error] {
			return runDockerAgent(ctx, t, agentName, a, sessStore, safety, workingDir, reg)
		},
	})
}

// runDockerAgent executes a docker agent and returns ADK session events
func runDockerAgent(ctx agent.InvocationContext, t *team.Team, agentName string, a *dagent.Agent, sessStore session.Store, safety servesafety.Resolved, workingDir string, reg *approvalRegistry) iter.Seq2[*adksession.Event, error] {
	return func(yield func(*adksession.Event, error) bool) {
		// Decorate the inbound `a2a.message` SERVER span (created by
		// otelhttp.NewHandler in server.go) with the GenAI semconv
		// invoke_agent shape so dashboards can recognise A2A traffic as
		// agent invocations rather than generic JSON-RPC POSTs. The
		// runtime.session span we open below is the child that records
		// the actual work; this annotation makes the parent searchable
		// via gen_ai.operation.name="invoke_agent".
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(
				attribute.String(cgenai.AttrOperationName, cgenai.OperationInvokeAgent),
				attribute.String(cgenai.AttrAgentName, agentName),
				attribute.String(cgenai.AttrAgentNameRuntime, agentName),
			)
		}

		// Extract user message from the ADK context
		userContent := ctx.UserContent()
		message := contentToMessage(userContent)

		// Use the A2A context ID as the docker-agent session ID so only future
		// A2A invocations with that ID can resume the conversation.
		sessionID := ctx.Session().ID()

		// The executor took the run parked for this task because the message
		// answers its pending call: resume it instead of starting a new turn.
		if ticket, ok := resumeFrom(ctx); ok {
			ticket.used.Store(true)
			resumePausedRun(ctx, yield, reg, sessStore, agentName, ticket)
			return
		}

		var sess *session.Session
		existing, err := sessStore.GetSessionByOrigin(ctx, sessionID, "a2a")
		switch {
		case err == nil:
			sess = existing
			// Reapply agent configuration; tool/history limits are not persisted.
			session.WithMaxIterations(a.MaxIterations())(sess)
			session.WithMaxConsecutiveToolCalls(a.MaxConsecutiveToolCalls())(sess)
			session.WithMaxOldToolCallTokens(a.MaxOldToolCallTokens())(sess)
			session.WithMaxToolResultTokens(a.MaxToolResultTokens())(sess)
			sess.AddMessage(session.UserMessage(message))
			sess.SetSafetyPolicy(servesafety.ResumeCeiling(sess.GetSafetyPolicy(), safety.Policy))
			sess.NonInteractive = true
			sess.ToolApproval = reg.toolApproval()
		case !errors.Is(err, session.ErrNotFound):
			yield(nil, fmt.Errorf("look up A2A session: %w", err))
			return
		default:
			_, err := sessStore.GetSession(ctx, sessionID)
			switch {
			case err == nil:
				yield(nil, errors.New("context ID is not available"))
				return
			case !errors.Is(err, session.ErrNotFound):
				yield(nil, fmt.Errorf("check A2A context ID: %w", err))
				return
			default:
				sess = session.New(
					session.WithID(sessionID),
					session.WithOrigin("a2a"),
					session.WithUserMessage(message),
					session.WithMaxIterations(a.MaxIterations()),
					session.WithMaxConsecutiveToolCalls(a.MaxConsecutiveToolCalls()),
					session.WithMaxOldToolCallTokens(a.MaxOldToolCallTokens()),
					session.WithMaxToolResultTokens(a.MaxToolResultTokens()),
					session.WithSafetyPolicy(safety.Policy),
					session.WithNonInteractive(true),
					session.WithToolApproval(reg.toolApproval()),
					session.WithWorkingDir(workingDir),
				)
				sess.SetTitle("A2A Session " + sessionID)
			}
		}

		// Create runtime
		rt, err := runtime.New(ctx, t,
			runtime.WithCurrentAgent(agentName),
			runtime.WithModelsDevFallback(embedded.Snapshot),
			runtime.WithSessionStore(sessStore),
			// Match the tracer scope used by `cmd/root/run.go` so
			// MCP / A2A / API spans share the same instrumentation
			// scope as the CLI's runtime spans. Without this option
			// `LocalRuntime.startSpan` sees a nil tracer and silently
			// returns no-op spans for runtime.session, runtime.stream,
			// runtime.tool.call, runtime.fallback, runtime.run_skill,
			// hook events, and so on.
			runtime.WithTracer(otel.Tracer(version.AppName)),
		)
		if err != nil {
			yield(nil, fmt.Errorf("failed to create runtime: %w", err))
			return
		}

		// Early iterator exits must cancel and join the runtime. In tool-approval
		// mode the stream outlives the request so a run parked on a confirmation
		// survives it; while active it is still cancelled with the invocation.
		streamCtx, cancel := reg.runContext(ctx)
		stop := bindToInvocation(ctx, cancel)
		eventsChan := reg.track(rt.RunStream(streamCtx, sess))
		info := taskInfoFrom(ctx)
		run := &pausedRun{
			taskID: info.TaskID, contextID: info.ContextID, rt: rt, sess: sess, events: eventsChan, cancel: cancel,
			spanCtx: trace.SpanContextFromContext(ctx),
		}
		if callCtx, ok := a2asrv.CallContextFrom(ctx); ok {
			run.user = callCtx.User
		}
		if !pumpEvents(ctx, yield, reg, agentName, run, stop) {
			stop()
			run.cancelAndDrain()
		}
	}
}

// resumePausedRun delivers the client's decision to a runtime parked on a tool
// confirmation and continues consuming its event stream. The caller took the
// run out of the registry, so this goroutine owns it: it must cancel and drain
// unless the run parks again.
func resumePausedRun(ctx agent.InvocationContext, yield func(*adksession.Event, error) bool, reg *approvalRegistry, sessStore session.Store, agentName string, ticket *resumeTicket) {
	paused := ticket.run
	// The run keeps the context of the request that started it, so its spans
	// stay under that trace; link this request to it.
	if span := trace.SpanFromContext(ctx); span.IsRecording() && paused.spanCtx.IsValid() {
		span.AddLink(trace.Link{SpanContext: paused.spanCtx})
	}
	stop := bindToInvocation(ctx, paused.cancel)
	req := resumeDecision(ticket.response, paused.grant)
	if req.Type == runtime.ResumeTypeApproveTool {
		// The next turn reloads the session from the store; persist the grant
		// now, as serve api does, so it does not ask again.
		paused.sess.AppendPermissionAllow(paused.grant)
		if err := sessStore.UpdateSession(ctx, paused.sess); err != nil {
			slog.WarnContext(ctx, "Failed to persist tool approval", "session_id", paused.sess.ID, "error", err)
		}
	}
	paused.rt.Resume(ctx, req)

	if !pumpEvents(ctx, yield, reg, agentName, paused, stop) {
		stop()
		paused.cancelAndDrain()
	}
}

// pumpEvents converts docker-agent runtime events to ADK events and yields
// them. It is shared by the initial run and by resume continuations. When a
// tool call needs approval it detaches the run from the invocation (stop),
// parks it in the registry, emits an INPUT_REQUIRED event, and returns true so
// the caller leaves the runtime alive. It returns false on normal completion,
// error, early stop or invocation cancellation.
func pumpEvents(ctx agent.InvocationContext, yield func(*adksession.Event, error) bool, reg *approvalRegistry, agentName string, run *pausedRun, stop func() bool) (handedOff bool) {
	var contentBuilder strings.Builder
	textEvent := func(partial, turnComplete bool, text string) *adksession.Event {
		ev := &adksession.Event{
			Author: agentName,
			LLMResponse: model.LLMResponse{
				Content:      genai.NewContentFromParts([]*genai.Part{{Text: text}}, genai.RoleModel),
				Partial:      partial,
				TurnComplete: turnComplete,
			},
		}
		if turnComplete {
			ev.FinishReason = genai.FinishReasonStop
		}
		return ev
	}

	for {
		var event runtime.Event
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "Invocation canceled, stopping agent", "agent", agentName)
			return false
		case ev, ok := <-run.events:
			if !ok {
				// The channel closed without a StreamStoppedEvent: the runtime
				// bounds how long it waits to deliver that event (#4136), but a
				// consumer that abandoned the channel or an unexpected close
				// should still complete the ADK turn rather than leave it hanging.
				if contentBuilder.Len() > 0 {
					yield(textEvent(false, true, contentBuilder.String()), nil)
				}
				return false
			}
			event = ev
		}
		if ctx.Ended() {
			slog.DebugContext(ctx, "Invocation ended, stopping agent", "agent", agentName)
			return false
		}

		switch e := event.(type) {
		case *runtime.AgentChoiceEvent:
			contentBuilder.WriteString(e.Content)
			if !yield(textEvent(true, false, e.Content), nil) {
				return false
			}

		case *runtime.ToolCallConfirmationEvent:
			if !reg.toolApproval() || run.taskID == "" || !stop() {
				return false
			}
			// Partial output is discarded when this invocation ends; emit the
			// text so far as a final chunk so it stays in the task artifacts.
			if contentBuilder.Len() > 0 {
				if !yield(textEvent(false, false, contentBuilder.String()), nil) {
					return false
				}
			}
			parked := *run
			// Computed once: the grant announced to the client is the one stored.
			// Empty for a command call that cannot be scoped to its command.
			parked.grant = permissions.ScopedPermissionPattern(ctx, e.ToolCall)
			parked.callID = e.ToolCall.ID
			// Reset: run still carries the previous pause's value.
			parked.rawArgs = ""
			if _, ok := decodeToolArgs(e.ToolCall.Function.Arguments); !ok {
				parked.rawArgs = e.ToolCall.Function.Arguments
			}
			parked.created = time.Now()
			reg.runHook(hookBeforePark, run.taskID)
			reg.park(&parked)
			reg.runHook(hookAfterPark, run.taskID)
			yield(inputRequiredEvent(agentName, e.ToolCall), nil)
			return true

		case *runtime.ErrorEvent:
			yield(nil, fmt.Errorf("%s", e.Error))
			return false

		case *runtime.StreamStoppedEvent:
			if contentBuilder.Len() > 0 {
				yield(textEvent(false, true, contentBuilder.String()), nil)
				return false
			}
		}
	}
}

// contentToMessage converts a genai.Content to a string message
func contentToMessage(content *genai.Content) string {
	if content == nil {
		return ""
	}

	var message string
	for _, part := range content.Parts {
		if part.Text != "" {
			if message != "" {
				message += "\n"
			}
			message += part.Text
		}
	}
	return message
}
