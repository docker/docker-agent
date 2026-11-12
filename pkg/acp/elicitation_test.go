package acp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type elicitationOutcome struct {
	result tools.ElicitationResult
	err    error
}

type acpElicitingToolset struct {
	mu      sync.Mutex
	handler tools.ElicitationHandler

	request  *mcp.ElicitParams
	atStart  bool
	approved *atomic.Bool
	outcomes chan elicitationOutcome
}

func (s *acpElicitingToolset) SetElicitationHandler(handler tools.ElicitationHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
}

func (s *acpElicitingToolset) elicit(ctx context.Context) error {
	s.mu.Lock()
	handler := s.handler
	s.mu.Unlock()
	if handler == nil {
		return errors.New("elicitation handler was not installed")
	}
	result, err := handler(ctx, s.request)
	s.outcomes <- elicitationOutcome{result: result, err: err}
	return err
}

func (s *acpElicitingToolset) Start(ctx context.Context) error {
	if s.atStart {
		return s.elicit(ctx)
	}
	return nil
}

func (*acpElicitingToolset) Stop(context.Context) error { return nil }

func (s *acpElicitingToolset) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{
		Name: "ask_user", Parameters: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			if !s.approved.Load() {
				return nil, errors.New("tool ran without ACP permission")
			}
			if !s.atStart {
				if err := s.elicit(ctx); err != nil {
					return nil, err
				}
			}
			return tools.ResultSuccess("User input unavailable"), nil
		},
	}}, nil
}

type elicitationTestProvider struct {
	approvalTestProvider
}

func (p *elicitationTestProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	if p.next < len(p.toolNames) {
		return p.approvalTestProvider.CreateChatCompletionStream(ctx, messages, available)
	}
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Done"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

func TestPromptDeclinesUnsupportedElicitation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"form", "url"} {
		for _, stage := range []string{"tool call", "tool startup"} {
			t.Run(mode+"/"+stage, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					wd := t.TempDir()
					a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
					a.team = team.New()
					var approved atomic.Bool
					outcomes := make(chan elicitationOutcome, 2)
					var loads int
					a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
						loads++
						req := &mcp.ElicitParams{Mode: mode, Message: "private elicitation message"}
						if mode == "form" {
							req.RequestedSchema = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}}
						} else {
							req.URL = "https://example.invalid/private-flow"
							req.ElicitationID = "server-request-id"
						}
						ts := &acpElicitingToolset{request: req, atStart: stage == "tool startup", approved: &approved, outcomes: outcomes}
						prov := &elicitationTestProvider{approvalTestProvider: approvalTestProvider{
							mockProvider: mockProvider{id: modelsdev.NewID("test", "elicitation")}, toolNames: []string{"ask_user"},
						}}
						root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts), agent.WithMaxIterations(1))
						return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
					}

					reader, writer := io.Pipe()
					output := &captureWriter{}
					peer := &peerResponder{t: t, out: output, peer: writer, respond: func(req acpsdk.RequestPermissionRequest) any {
						// Resume uses an unbuffered channel; let the runtime reach its wait.
						synctest.Wait()
						if req.ToolCall.ToolCallId == "max_iterations" {
							return permissionSelected("continue")
						}
						approved.Store(true)
						return permissionSelected("allow")
					}}
					conn := acpsdk.NewAgentSideConnection(a, peer, reader)
					conn.SetLogger(slog.New(slog.DiscardHandler))
					a.SetAgentConnection(conn)
					t.Cleanup(func() {
						require.NoError(t, a.Stop(t.Context()))
						_ = writer.Close()
						<-conn.Done()
					})

					created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
					require.NoError(t, err)
					for turn := range 2 {
						if turn == 1 {
							_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
							require.NoError(t, err)
							_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
							require.NoError(t, err)
						}
						approved.Store(false)
						s := a.sessions[string(created.SessionId)]
						assert.False(t, s.sess.NonInteractive, "permission and iteration prompts must remain interactive")
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						response, err := a.Prompt(ctx, acpsdk.PromptRequest{
							SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("Ask for input")},
						})
						cancel()
						require.NoError(t, err)
						require.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason, "elicitation must not stall until cancellation")
						select {
						case outcome := <-outcomes:
							require.NoError(t, outcome.err)
							assert.Equal(t, tools.ElicitationActionDecline, outcome.result.Action)
							assert.Nil(t, outcome.result.Content)
						default:
							t.Fatal("toolset did not receive an elicitation response")
						}
						assert.Equal(t, "Done", s.sess.GetLastAssistantMessageContent())
						reqs := peer.recordedRequests()
						require.Len(t, reqs, (turn+1)*2, "only permission and max-iteration requests should reach the client")
						assert.NotEmpty(t, reqs[turn*2].ToolCall.ToolCallId)
						assert.Equal(t, "ask_user", *reqs[turn*2].ToolCall.Title)
						assert.Equal(t, acpsdk.ToolCallId("max_iterations"), reqs[turn*2+1].ToolCall.ToolCallId)
					}
					assert.Equal(t, 2, loads)
					assert.Empty(t, outcomes)
					notifications := strings.Join(output.lines(), "\n")
					assert.NotContains(t, notifications, "private elicitation message")
					assert.NotContains(t, notifications, "private-flow")
					assert.NotContains(t, notifications, "server-request-id")
				})
			})
		}
	}
}
