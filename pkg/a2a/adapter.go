package a2a

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
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
func newDockerAgentAdapter(t *team.Team, agentName string, sessStore session.Store, safety servesafety.Resolved) (agent.Agent, error) {
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
			return runDockerAgent(ctx, t, agentName, a, sessStore, safety)
		},
	})
}

// runDockerAgent executes a docker agent and returns ADK session events
func runDockerAgent(ctx agent.InvocationContext, t *team.Team, agentName string, a *dagent.Agent, sessStore session.Store, safety servesafety.Resolved) iter.Seq2[*adksession.Event, error] {
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

		var sess *session.Session
		existing, err := sessStore.GetSessionByOrigin(ctx, sessionID, "a2a")
		switch {
		case err == nil:
			sess = existing
			sess.AddMessage(session.UserMessage(message))
			sess.SetSafetyPolicy(servesafety.ResumeCeiling(sess.GetSafetyPolicy(), safety.Policy))
			sess.NonInteractive = true
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
				workingDir, _ := os.Getwd()
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
					session.WithWorkingDir(workingDir),
				)
				sess.SetTitle("A2A Session " + sessionID)
			}
		}

		// Create runtime
		rt, err := runtime.New(ctx, t,
			runtime.WithCurrentAgent(agentName),
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

		// Run the agent and collect events
		eventsChan := rt.RunStream(ctx, sess)

		// Track accumulated content for chunked responses
		var contentBuilder strings.Builder

		// Convert docker agent events to ADK events and yield them

		for event := range eventsChan {
			if ctx.Ended() {
				slog.Debug("Invocation ended, stopping agent", "agent", agentName)
				return
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
					return
				}

			case *runtime.ErrorEvent:
				// Yield error and stop

				yield(nil, fmt.Errorf("%s", e.Error))
				return

			case *runtime.StreamStoppedEvent:
				// Send final complete event with all accumulated content
				if contentBuilder.Len() > 0 {
					finalEvent := &adksession.Event{
						Author: agentName,
						LLMResponse: model.LLMResponse{
							Content:      genai.NewContentFromParts([]*genai.Part{{Text: contentBuilder.String()}}, genai.RoleModel),
							Partial:      false,
							TurnComplete: true,
							FinishReason: genai.FinishReasonStop,
						},
					}
					yield(finalEvent, nil)
					return
				}
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
