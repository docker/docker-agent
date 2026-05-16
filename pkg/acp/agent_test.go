package acp

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockStream simulates a chat completion stream for testing.
type mockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
}

func (m *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if m.idx >= len(m.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := m.responses[m.idx]
	m.idx++
	return resp, nil
}

func (m *mockStream) Close() {}

// mockProvider returns a predetermined stream for testing.
type mockProvider struct {
	id     modelsdev.ID
	stream chat.MessageStream
}

func (m *mockProvider) ID() modelsdev.ID { return m.id }

func (m *mockProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return m.stream, nil
}

func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func (m *mockProvider) MaxTokens() int { return 0 }

// TestACPSessionPersistence verifies that ACP sessions are persisted to the SQLite store.
func TestACPSessionPersistence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Create a temp SQLite session DB
	dbPath := filepath.Join(t.TempDir(), "session.db")
	sessStore, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)

	// Close the store at the end
	if closer, ok := sessStore.(io.Closer); ok {
		defer closer.Close()
	}

	// Create a mock provider that returns a simple assistant message
	stream := &mockStream{
		responses: []chat.MessageStreamResponse{
			{
				Choices: []chat.MessageStreamChoice{{
					Index: 0,
					Delta: chat.MessageDelta{Content: "Hello from the agent!"},
				}},
			},
			{
				Choices: []chat.MessageStreamChoice{{
					Index:        0,
					FinishReason: chat.FinishReasonStop,
				}},
				Usage: &chat.Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	}
	prov := &mockProvider{id: modelsdev.NewID("test", "mock-model"), stream: stream}

	// Create a minimal team with a root agent
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	// Create the ACP agent with the session store
	// Note: we set team directly to avoid Initialize requiring full config loading
	acpAgent := &Agent{
		agentSource:  nil, // Not needed since team is pre-set
		runConfig:    &config.RuntimeConfig{},
		sessionStore: sessStore,
		sessions:     make(map[string]*Session),
		team:         tm,
	}

	// Create a new session via ACP with a real temp directory
	workingDir := t.TempDir()
	newSessResp, err := acpAgent.NewSession(ctx, acpsdk.NewSessionRequest{
		Cwd: workingDir,
	})
	require.NoError(t, err)
	acpSessionID := string(newSessResp.SessionId)
	require.NotEmpty(t, acpSessionID)

	// Get the session and add a user message
	acpAgent.mu.Lock()
	acpSess := acpAgent.sessions[acpSessionID]
	acpAgent.mu.Unlock()
	require.NotNil(t, acpSess)

	// Use the actual session ID for lookups (should match the ACP session ID after fix)
	sessionID := acpSess.sess.ID

	// Add user message to the session
	acpSess.sess.AddMessage(session.UserMessage("Hello, agent!"))

	// Run the runtime directly (bypasses ACP connection which we don't have in test)
	// This tests that the session store is properly used by the runtime
	eventsChan := acpSess.rt.RunStream(ctx, acpSess.sess)

	// Drain events
	for range eventsChan {
		// Just consume all events
	}

	// Verify the session is persisted via GetSessionSummaries
	summaries, err := sessStore.GetSessionSummaries(ctx)
	require.NoError(t, err)

	// Find our session in the summaries
	var found bool
	for _, s := range summaries {
		if s.ID == sessionID {
			found = true
			assert.Contains(t, s.Title, "ACP Session")
			break
		}
	}
	assert.True(t, found, "ACP session should appear in GetSessionSummaries")

	// Also verify full session retrieval
	loadedSess, err := sessStore.GetSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionID, loadedSess.ID)
	assert.Contains(t, loadedSess.Title, "ACP Session")
	assert.Equal(t, workingDir, loadedSess.WorkingDir)

	// Verify messages were persisted (user + assistant)
	assert.GreaterOrEqual(t, len(loadedSess.Messages), 2, "Session should have at least user and assistant messages")

	// Find user message
	var hasUserMsg, hasAssistantMsg bool
	for _, item := range loadedSess.Messages {
		if item.Message != nil {
			if item.Message.Message.Role == chat.MessageRoleUser {
				hasUserMsg = true
			}
			if item.Message.Message.Role == chat.MessageRoleAssistant {
				hasAssistantMsg = true
			}
		}
	}
	assert.True(t, hasUserMsg, "Session should have a user message")
	assert.True(t, hasAssistantMsg, "Session should have an assistant message")
}
