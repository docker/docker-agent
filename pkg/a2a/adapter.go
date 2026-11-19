package a2a

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
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

		// If a run is parked waiting for tool approval on this context, the
		// incoming message carries the client's decision (a FunctionResponse
		// validated by ADK's HandleInputRequired). Resume the parked runtime
		// instead of starting a new one; do not add a user message.
		if paused, ok := reg.take(sessionID); ok {
			resumePausedRun(ctx, yield, reg, sessionID, agentName, paused)
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
		// mode the stream derives from the server-lifetime context so a run that
		// parks on a confirmation survives the request that started it; otherwise
		// it derives from the invocation context and is torn down with the request.
		streamCtx, cancel := context.WithCancel(reg.runContext(ctx))
		eventsChan := rt.RunStream(streamCtx, sess)
		handedOff := false
		defer func() {
			// A run handed off to the registry (parked on a tool confirmation)
			// must stay alive: the registry now owns its cancel and drain.
			if handedOff {
				return
			}
			cancel()
			for range eventsChan {
			}
		}()

		var contentBuilder strings.Builder
		handedOff = pumpEvents(ctx, yield, reg, sessionID, agentName, rt, eventsChan, cancel, &contentBuilder)
	}
}

// resumePausedRun delivers the client's approve/reject decision to a runtime
// parked on a tool confirmation and continues consuming its event stream. The
// runtime was taken out of the registry by the caller, so this goroutine owns
// it: it must cancel and drain unless the run parks again (handed back off).
func resumePausedRun(ctx agent.InvocationContext, yield func(*adksession.Event, error) bool, reg *approvalRegistry, sessionID, agentName string, paused *pausedRun) {
	paused.rt.Resume(ctx, resumeDecision(ctx.UserContent(), paused))

	var contentBuilder strings.Builder
	handedOff := pumpEvents(ctx, yield, reg, sessionID, agentName, paused.rt, paused.events, paused.cancel, &contentBuilder)
	if !handedOff {
		paused.cancelAndDrain()
	}
}

// pumpEvents converts docker-agent runtime events to ADK events and yields
// them. It is shared by the initial run and by resume continuations. When a
// tool call needs approval it parks the run in the registry, emits an
// INPUT_REQUIRED event, and returns true so the caller leaves the runtime
// alive. It returns false on normal completion, error, or early stop.
func pumpEvents(ctx agent.InvocationContext, yield func(*adksession.Event, error) bool, reg *approvalRegistry, sessionID, agentName string, rt runtime.Runtime, eventsChan <-chan runtime.Event, cancel context.CancelFunc, contentBuilder *strings.Builder) (handedOff bool) {
	// finalEvent builds the turn-complete ADK event from whatever content was
	// accumulated so far. Shared by the StreamStoppedEvent case and the
	// post-loop fallback below, so both paths build an identical event.
	finalEvent := func() *adksession.Event {
		return &adksession.Event{
			Author: agentName,
			LLMResponse: model.LLMResponse{
				Content:      genai.NewContentFromParts([]*genai.Part{{Text: contentBuilder.String()}}, genai.RoleModel),
				Partial:      false,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
		}
	}

	for event := range eventsChan {
		if ctx.Ended() {
			slog.DebugContext(ctx, "Invocation ended, stopping agent", "agent", agentName)
			return false
		}

		switch e := event.(type) {
		case *runtime.AgentChoiceEvent:
			// Accumulate content chunks
			contentBuilder.WriteString(e.Content)

			// Create a partial response event
			adkEvent := &adksession.Event{
				Author: agentName,
				LLMResponse: model.LLMResponse{
					Content:      genai.NewContentFromParts([]*genai.Part{{Text: e.Content}}, genai.RoleModel),
					Partial:      true,
					TurnComplete: false,
				},
			}

			if !yield(adkEvent, nil) {
				return false
			}

		case *runtime.ToolCallConfirmationEvent:
			// The runtime is parked inside askUser waiting for approval. Report
			// the pending call to the client as a long-running function call
			// (driving the A2A task to INPUT_REQUIRED) and hand the runtime to
			// the registry so the next request can resume it. Do not cancel.
			reg.store(sessionID, &pausedRun{
				rt:       rt,
				events:   eventsChan,
				cancel:   cancel,
				toolName: e.ToolCall.Function.Name,
				callID:   e.ToolCall.ID,
				created:  time.Now(),
			})
			yield(inputRequiredEvent(agentName, e.ToolCall), nil)
			return true

		case *runtime.ErrorEvent:
			// Yield error and stop

			yield(nil, fmt.Errorf("%s", e.Error))
			return false

		case *runtime.StreamStoppedEvent:
			// Send final complete event with all accumulated content
			if contentBuilder.Len() > 0 {
				yield(finalEvent(), nil)
				return false
			}
		}
	}

	// The channel closed without a StreamStoppedEvent: the runtime bounds
	// how long it waits to deliver that event (#4136), but a consumer that
	// abandoned the channel or an unexpected close should still complete
	// the ADK turn rather than leave it hanging.
	if contentBuilder.Len() > 0 {
		yield(finalEvent(), nil)
	}
	return false
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
