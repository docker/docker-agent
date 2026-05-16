package a2a

import (
	"cmp"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// newDockerAgentAdapter creates a new ADK agent adapter from a docker agent team and agent name.
// When agentName is empty, the team's default agent (one explicitly named "root" if it
// exists, otherwise the first agent declared) is used.
func newDockerAgentAdapter(t *team.Team, agentName string, sessStore session.Store) (agent.Agent, error) {
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
			return runDockerAgent(ctx, t, agentName, a, sessStore)
		},
	})
}

// runDockerAgent executes a docker agent and returns ADK session events
func runDockerAgent(ctx agent.InvocationContext, t *team.Team, agentName string, a *dagent.Agent, sessStore session.Store) iter.Seq2[*adksession.Event, error] {
	return func(yield func(*adksession.Event, error) bool) {
		// Extract user message from the ADK context
		userContent := ctx.UserContent()
		message := contentToMessage(userContent)

		// Use the A2A contextID (exposed as the ADK session ID) as the
		// docker-agent session ID so subsequent `run --session <id>`
		// invocations can resume the same conversation.
		sessionID := ctx.Session().ID()

		var sess *session.Session
		if existing, err := sessStore.GetSession(ctx, sessionID); err == nil && existing != nil {
			sess = existing
			sess.AddMessage(session.UserMessage(message))
			sess.ToolsApproved = true
			sess.NonInteractive = true
		} else {
			workingDir, _ := os.Getwd()
			sess = session.New(
				session.WithID(sessionID),
				session.WithUserMessage(message),
				session.WithMaxIterations(a.MaxIterations()),
				session.WithMaxConsecutiveToolCalls(a.MaxConsecutiveToolCalls()),
				session.WithMaxOldToolCallTokens(a.MaxOldToolCallTokens()),
				session.WithToolsApproved(true),
				session.WithNonInteractive(true),
				session.WithWorkingDir(workingDir),
			)
			sess.Title = "A2A Session " + sessionID
		}

		// Create runtime
		rt, err := runtime.New(t,
			runtime.WithCurrentAgent(agentName),
			runtime.WithSessionStore(sessStore),
			runtime.WithTracer(otel.Tracer("cagent")),
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
