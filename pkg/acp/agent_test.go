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

func TestBuildUserContent_ResourceLinkFallbackDoesNotExposeAbsoluteURI(t *testing.T) {
	t.Parallel()

	acpAgent := &Agent{sessions: make(map[string]*Session)}
	uri := "file:///var/folders/40/w2lqxd4564132ydf4v_7s5wh0000gn/T/TemporaryItems/Screenshot%202026-06-15.png"

	content := acpAgent.buildUserContent(t.Context(), "missing-session", []acpsdk.ContentBlock{
		acpsdk.ResourceLinkBlock("Screenshot 2026-06-15.png", uri),
	})

	assert.Contains(t, content, "Screenshot 2026-06-15.png")
	assert.Contains(t, content, "content unavailable")
	assert.NotContains(t, content, uri)
	assert.NotContains(t, content, "/var/folders")
}

func TestBuildUserMessage_ImageContent(t *testing.T) {
	t.Parallel()

	acpAgent := &Agent{sessions: make(map[string]*Session)}
	msg := acpAgent.buildUserMessage(t.Context(), "session-id", []acpsdk.ContentBlock{
		acpsdk.TextBlock("look at this"),
		acpsdk.ImageBlock("AAAA", "image/png"),
	})

	require.NotNil(t, msg)
	assert.Equal(t, "look at this", msg.Message.Content)
	require.Len(t, msg.Message.MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeText, msg.Message.MultiContent[0].Type)
	assert.Equal(t, "look at this", msg.Message.MultiContent[0].Text)
	assert.Equal(t, chat.MessagePartTypeImageURL, msg.Message.MultiContent[1].Type)
	require.NotNil(t, msg.Message.MultiContent[1].ImageURL)
	assert.Equal(t, "data:image/png;base64,AAAA", msg.Message.MultiContent[1].ImageURL.URL)
}

func TestResolveSessionPathUsesSessionRoots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	additionalDir := t.TempDir()
	outsideDir := t.TempDir()
	absWorkingDir, err := filepath.EvalSymlinks(workingDir)
	require.NoError(t, err)
	absAdditionalDir, err := filepath.EvalSymlinks(additionalDir)
	require.NoError(t, err)

	acpAgent := &Agent{
		runConfig: &config.RuntimeConfig{},
		sessions: map[string]*Session{
			"session-id": {
				id:             "session-id",
				sess:           session.New(session.WithWorkingDir(workingDir)),
				workingDir:     workingDir,
				additionalDirs: []string{additionalDir},
			},
		},
	}

	resolved, err := acpAgent.resolveSessionPath("session-id", "relative.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(absWorkingDir, "relative.txt"), resolved)

	resolved, err = acpAgent.resolveSessionPath("session-id", filepath.Join(additionalDir, "attached.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(absAdditionalDir, "attached.txt"), resolved)

	_, err = acpAgent.resolveSessionPath("session-id", filepath.Join(outsideDir, "blocked.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}

// TestACPSessionPersistence verifies that ACP sessions are persisted to the SQLite store.
func TestACPSessionPersistence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Create a temp SQLite session DB
	dbPath := filepath.Join(t.TempDir(), "session.db")
	sessStore, err := session.NewSQLiteSessionStore(t.Context(), dbPath)
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

func TestCommandName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		text   string
		want   string
		wantOk bool
	}{
		{"new command", "/new", "new", true},
		{"compact command", "/compact", "compact", true},
		{"usage command", "/usage", "usage", true},
		{"leading/trailing space", "  /new  ", "new", true},
		{"command with args is not a bare command", "/compact focus on tests", "", false},
		{"plain message", "hello there", "", false},
		{"message starting with slash but with words", "/path/to/file please read", "", false},
		{"empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, ok := commandName([]acpsdk.ContentBlock{acpsdk.TextBlock(tt.text)})
			assert.Equal(t, tt.wantOk, ok)
			assert.Equal(t, tt.want, name)
		})
	}
}

// TestClearSessionHistory verifies that /new drops the persisted conversation
// so it cannot reload on a subsequent resume (the bug behind "/new clears the
// session but it comes back when I reopen sbx and gordon").
func TestClearSessionHistory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dbPath := filepath.Join(t.TempDir(), "session.db")
	sessStore, err := session.NewSQLiteSessionStore(ctx, dbPath)
	require.NoError(t, err)
	if closer, ok := sessStore.(io.Closer); ok {
		defer closer.Close()
	}

	sess := session.New(session.WithWorkingDir(t.TempDir()))
	sess.AddMessage(session.UserMessage("first message"))
	require.NoError(t, sessStore.AddSession(ctx, sess))

	loaded, err := sessStore.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)

	acpAgent := &Agent{
		runConfig:    &config.RuntimeConfig{},
		sessionStore: sessStore,
		sessions:     make(map[string]*Session),
	}
	acpSess := &Session{id: sess.ID, sess: sess}

	require.NoError(t, acpAgent.clearSessionHistory(ctx, acpSess))

	// In-memory session is cleared.
	assert.Empty(t, acpSess.sess.Messages)

	// A resume (GetSession) must not bring the old messages back.
	reloaded, err := sessStore.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Empty(t, reloaded.Messages, "cleared session must not reload previous messages")
}
