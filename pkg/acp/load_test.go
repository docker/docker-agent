package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

func loadRequest(s *Session) acpsdk.LoadSessionRequest {
	return acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir, McpServers: []acpsdk.McpServer{}}
}

func captureReplay(t *testing.T, a *Agent, out *captureWriter) {
	t.Helper()
	f := newRunAgentFixture(t, &fakeRuntime{}, out)
	a.SetAgentConnection(f.agent.conn)
}

func replayUpdates(t *testing.T, out *captureWriter, sid string) []acpsdk.SessionUpdate {
	t.Helper()
	var updates []acpsdk.SessionUpdate
	for _, line := range out.lines() {
		var msg struct {
			Method string                     `json:"method"`
			Params acpsdk.SessionNotification `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &msg))
		assert.Equal(t, "session/update", msg.Method)
		assert.Equal(t, acpsdk.SessionId(sid), msg.Params.SessionId)
		updates = append(updates, msg.Params.Update)
	}
	return updates
}

func TestLoadReplaysHistoryWithoutExecution(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	rt := &commandRuntime{current: "root"}
	s.rt = rt
	out := &captureWriter{}
	captureReplay(t, a, out)
	call := tools.ToolCall{ID: "reused", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"note.txt"}`}}
	s.sess.AddMessage(session.UserMessage("user"))
	s.sess.AddMessage(session.ImplicitUserMessage("hidden user"))
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "hidden instructions"}})
	s.sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", ReasoningContent: "visible thought", ThinkingSignature: "private signature", ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "read_file", Annotations: tools.ToolAnnotations{Title: "Read saved file"}}}}})
	child := session.New(session.WithWorkingDir(s.workingDir))
	child.AddMessage(session.ImplicitUserMessage("hidden child prompt"))
	child.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "child", ToolCalls: []tools.ToolCall{call}}})
	child.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "reused", Content: "child failure", IsError: true}})
	s.sess.AddSubSession(child)
	s.sess.AddMessage(&session.Message{AgentName: "other", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "reused", Content: "transformed output", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "raw secret"}}}})
	s.sess.ApplyCompaction(1, 1, session.Item{Summary: "internal summary"})
	before := s.sess.Clone()
	_, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	updates := replayUpdates(t, out, s.id)
	require.Len(t, updates, 9)
	assert.Equal(t, "user", updates[0].UserMessageChunk.Content.Text.Text)
	assert.Equal(t, "visible thought", updates[1].AgentThoughtChunk.Content.Text.Text)
	assert.Equal(t, "answer", updates[2].AgentMessageChunk.Content.Text.Text)
	require.NotNil(t, updates[3].ToolCall)
	assert.Equal(t, "Read saved file", updates[3].ToolCall.Title)
	assert.Equal(t, acpsdk.ToolCallStatusCompleted, updates[3].ToolCall.Status)
	assert.Nil(t, updates[3].ToolCall.RawInput)
	assert.Empty(t, updates[3].ToolCall.Locations)
	assert.Equal(t, "child", updates[4].AgentMessageChunk.Content.Text.Text)
	assert.Equal(t, acpsdk.ToolCallStatusFailed, updates[5].ToolCall.Status)
	assert.NotEqual(t, updates[3].ToolCall.ToolCallId, updates[5].ToolCall.ToolCallId)
	assert.Equal(t, updates[5].ToolCall.ToolCallId, updates[6].ToolCallUpdate.ToolCallId)
	assert.Equal(t, updates[3].ToolCall.ToolCallId, updates[7].ToolCallUpdate.ToolCallId)
	assert.Equal(t, "transformed output", updates[7].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.NotNil(t, updates[8].AvailableCommandsUpdate)
	wire := strings.Join(out.lines(), "\n")
	for _, hidden := range []string{"hidden user", "hidden instructions", "hidden child prompt", "private signature", "raw secret", "internal summary", "in_progress", "pending"} {
		assert.NotContains(t, wire, hidden)
	}
	assert.Zero(t, rt.runs)
	assert.Zero(t, rt.toolLists)
	assert.Equal(t, before, s.sess.Clone())
	firstCount := len(out.lines())
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	assert.Len(t, out.lines(), firstCount*2, "repeat load replays a new snapshot")
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir})
	require.NoError(t, err)
	assert.Len(t, out.lines(), firstCount*2+1, "resume only refreshes commands")
}

func TestLoadToolOccurrencesAndMissingResults(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	out := &captureWriter{}
	captureReplay(t, a, out)
	for _, failed := range []bool{false, true} {
		s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "same", Function: tools.FunctionCall{Name: "unknown"}}, {ID: "known", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}}, ToolDefinitions: []tools.Tool{{Name: "shell", Annotations: tools.ToolAnnotations{Title: "Shell title"}}}}})
		s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "same", Content: "result", IsError: failed}})
		s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "known", Content: "ok"}})
	}
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "missing", Function: tools.FunctionCall{Name: "shell"}}}}})
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "orphan", Content: "orphan failed", IsError: true}})
	_, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	updates := replayUpdates(t, out, s.id)
	require.Len(t, updates, 11)
	assert.Equal(t, "unknown", updates[0].ToolCall.Title)
	assert.Equal(t, "Shell title", updates[1].ToolCall.Title)
	assert.Equal(t, updates[0].ToolCall.ToolCallId, updates[2].ToolCallUpdate.ToolCallId)
	assert.NotEqual(t, updates[0].ToolCall.ToolCallId, updates[4].ToolCall.ToolCallId)
	assert.Equal(t, acpsdk.ToolCallStatusFailed, updates[4].ToolCall.Status)
	assert.Contains(t, updates[8].ToolCall.Content[0].Content.Content.Text.Text, "outcome is unknown")
	assert.Nil(t, updates[9].ToolCall.RawInput)
	assert.Equal(t, acpsdk.ToolCallStatusFailed, updates[9].ToolCall.Status)
}

func TestLoadInlineAttachmentsAndFrameLimits(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	out := &captureWriter{}
	captureReplay(t, a, out)
	text := strings.Repeat("é\x00<", replayTextChunkBytes)
	s.sess.AddMessage(session.UserMessage("duplicate fallback", chat.MessagePart{Type: chat.MessagePartTypeText, Text: text},
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.pdf", MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: []byte("%PDF")}}},
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "oversized", MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: make([]byte, maxReplayUpdateBytes)}}},
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "generated", Source: chat.DocumentSource{ArtifactPath: "secret.txt"}}},
		chat.MessagePart{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/private/never-read.txt"}},
		chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://never-fetch.invalid/private"}},
		chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64," + testPNGBase64(t, 1)}},
	))
	_, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	var rebuilt strings.Builder
	var blocks []acpsdk.ContentBlock
	for _, update := range replayUpdates(t, out, s.id) {
		if update.UserMessageChunk != nil {
			blocks = append(blocks, update.UserMessageChunk.Content)
		}
	}
	for _, block := range blocks {
		if block.Text != nil && !strings.HasPrefix(block.Text.Text, "[Attachment:") {
			rebuilt.WriteString(block.Text.Text)
		}
	}
	assert.Equal(t, text, rebuilt.String())
	assert.NotNil(t, blocks[len(blocks)-1].Image)
	foundPDF := false
	for _, block := range blocks {
		if block.Resource != nil {
			foundPDF = true
			assert.Equal(t, "JVBERg==", block.Resource.Resource.BlobResourceContents.Blob)
		}
	}
	assert.True(t, foundPDF)
	wire := strings.Join(out.lines(), "\n")
	assert.NotContains(t, wire, "duplicate fallback")
	assert.NotContains(t, wire, "never-read")
	assert.NotContains(t, wire, "never-fetch")
	assert.Equal(t, 4, strings.Count(wire, "content unavailable during replay"))
	for _, line := range out.lines() {
		assert.Less(t, len(line), maxReplayUpdateBytes+1024)
	}
}

func TestLoadSQLiteColdAndPromptContinuation(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	wd := t.TempDir()
	saved := session.New(session.WithWorkingDir(wd), session.WithUserMessage("saved user"))
	saved.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "saved answer"}})
	require.NoError(t, store.AddSession(t.Context(), saved))
	a := NewAgent(nil, &config.RuntimeConfig{}, store)
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		loaded, _ := loadedLifecycleTeam(t)
		return loaded, nil
	}
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	out := &captureWriter{}
	captureReplay(t, a, out)
	_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(saved.ID), Cwd: wd})
	require.NoError(t, err)
	updates := replayUpdates(t, out, saved.ID)
	require.Len(t, updates, 3)
	assert.Equal(t, "saved user", updates[0].UserMessageChunk.Content.Text.Text)
	assert.Equal(t, "saved answer", updates[1].AgentMessageChunk.Content.Text.Text)
	loaded := a.sessions[saved.ID]
	rt := &fakeRuntime{}
	original := loaded.rt
	loaded.rt = rt
	require.NoError(t, original.Close())
	response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(saved.ID), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("next")}})
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	assert.Equal(t, "next", loaded.sess.GetLastUserMessageContent())
	stored, err := store.GetSession(t.Context(), saved.ID)
	require.NoError(t, err)
	assert.Len(t, stored.OwnMessages(), 2, "replay does not append transcript rows")
}

func TestLoadWireReplayPrecedesResponse(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	s.sess.AddMessage(session.UserMessage("history"))
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := acpsdk.NewAgentSideConnection(a, output, input)
	a.SetAgentConnection(conn)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, params := range []map[string]any{
		{"sessionId": s.id, "mcpServers": []any{}},
		{"sessionId": s.id, "cwd": s.workingDir},
		{"sessionId": s.id, "cwd": s.workingDir, "mcpServers": []any{}},
	} {
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": "session/load", "params": params}))
		var seenHistory bool
		for {
			var msg struct {
				ID     *int                       `json:"id"`
				Method string                     `json:"method"`
				Params acpsdk.SessionNotification `json:"params"`
				Result json.RawMessage            `json:"result"`
				Error  *acpsdk.RequestError       `json:"error"`
			}
			require.NoError(t, decoder.Decode(&msg))
			if msg.ID != nil {
				assert.Equal(t, i, *msg.ID)
				if i < 2 {
					require.NotNil(t, msg.Error)
					assert.Equal(t, -32602, msg.Error.Code)
					assert.False(t, seenHistory)
				} else {
					require.Nil(t, msg.Error)
					assert.True(t, seenHistory)
					assert.JSONEq(t, `{}`, string(msg.Result))
				}
				break
			}
			if chunk := msg.Params.Update.UserMessageChunk; chunk != nil {
				seenHistory = true
				assert.Equal(t, "history", chunk.Content.Text.Text)
			}
		}
	}
}

func TestLoadReplayBlocksPromptsAndReconnects(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		s.sess.AddMessage(session.UserMessage("history"))
		entered, release := make(chan struct{}), make(chan struct{})
		out := &captureWriter{failOn: func(n int) error {
			if n == 1 {
				close(entered)
				<-release
			}
			return nil
		}}
		captureReplay(t, a, out)
		done := make(chan error, 1)
		go func() { _, err := a.LoadSession(t.Context(), loadRequest(s)); done <- err }()
		<-entered
		_, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(s.id), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("must not append")}})
		require.ErrorContains(t, err, "loading")
		_, err = a.LoadSession(t.Context(), loadRequest(s))
		require.Error(t, err)
		_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir})
		require.Error(t, err)
		assert.Len(t, s.sess.OwnMessages(), 1)
		close(release)
		require.NoError(t, <-done)
		_, finish, err := s.startTurn(t.Context())
		require.NoError(t, err)
		finish()
	})
}

func TestLoadReplayFailureClosesAndCanRetry(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	s.sess.AddMessage(session.UserMessage("history"))
	out := &captureWriter{failOn: func(n int) error {
		if n == 1 {
			return errors.New("writer failed")
		}
		return nil
	}}
	captureReplay(t, a, out)
	_, err := a.LoadSession(t.Context(), loadRequest(s))
	require.ErrorContains(t, err, "writer failed")
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.NoError(t, err)
	assert.Empty(t, a.sessions)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		loaded, _ := loadedLifecycleTeam(t)
		return loaded, nil
	}
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(out.lines(), "\n"), "history")
}

func TestLoadCancellationAndCloseJoinReplay(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"request", "close", "stop"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, s := newResumeFixture(t, t.TempDir())
				s.sess.AddMessage(session.UserMessage("first"))
				s.sess.AddMessage(session.UserMessage("never sent"))
				entered, release := make(chan struct{}), make(chan struct{})
				out := &captureWriter{failOn: func(n int) error {
					if n == 1 {
						close(entered)
						<-release
					}
					return nil
				}}
				captureReplay(t, a, out)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				loaded := make(chan error, 1)
				go func() { _, err := a.LoadSession(ctx, loadRequest(s)); loaded <- err }()
				<-entered
				closed := make(chan error, 1)
				switch mode {
				case "request":
					cancel()
				case "close":
					go func() {
						_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
						closed <- err
					}()
				case "stop":
					go func() { closed <- a.Stop(t.Context()) }()
				}
				synctest.Wait()
				select {
				case <-loaded:
					t.Fatal("load returned before blocked write joined")
				default:
				}
				if mode != "request" {
					select {
					case <-closed:
						t.Fatal("close returned before replay joined")
					default:
					}
				}
				close(release)
				require.Error(t, <-loaded)
				if mode != "request" {
					require.NoError(t, <-closed)
				} else {
					_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
					require.NoError(t, err)
				}
				assert.NotContains(t, strings.Join(out.lines(), "\n"), "never sent")
				assert.Empty(t, a.sessions)
			})
		})
	}
}

func TestLoadWorkspaceValidationAndRootReplacement(t *testing.T) {
	t.Parallel()
	wd, oldRoot, newRoot := t.TempDir(), t.TempDir(), t.TempDir()
	a, s := newResumeFixture(t, wd, oldRoot)
	out := &captureWriter{}
	captureReplay(t, a, out)
	s.sess.AddMessage(session.UserMessage("history"))
	req := loadRequest(s)
	req.Cwd = newRoot
	_, err := a.LoadSession(t.Context(), req)
	require.ErrorContains(t, err, "must match")
	assert.Empty(t, out.lines())
	req = loadRequest(s)
	req.AdditionalDirectories = []string{"relative"}
	_, err = a.LoadSession(t.Context(), req)
	require.Error(t, err)
	req = loadRequest(s)
	req.AdditionalDirectories = []string{newRoot}
	_, err = a.LoadSession(t.Context(), req)
	require.NoError(t, err)
	_, roots := s.workspaceSnapshot()
	assert.Equal(t, []string{newRoot}, roots)
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	_, roots = s.workspaceSnapshot()
	assert.Empty(t, roots)
	req = loadRequest(s)
	req.SessionId = "missing"
	_, err = a.LoadSession(t.Context(), req)
	require.Error(t, err)
}

func TestLoadRegistrationLoserReplaysWinner(t *testing.T) {
	t.Parallel()
	a, previous := newResumeFixture(t, t.TempDir())
	previous.sess.AddMessage(session.UserMessage("persisted history"))
	_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(previous.id)})
	require.NoError(t, err)
	winner := &Session{id: previous.id, workingDir: previous.workingDir, rt: &fakeRuntime{}, sess: session.New(session.WithID(previous.id), session.WithWorkingDir(previous.workingDir), session.WithUserMessage("winner history"))}
	loaded, ts := loadedLifecycleTeam(t)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		_, stored, err := registerTestSession(t.Context(), a, winner)
		require.NoError(t, err)
		require.True(t, stored)
		return loaded, nil
	}
	// The registered winner and persisted row refer to the same transcript.
	previous.sess.AddMessage(session.UserMessage("winner history"))
	out := &captureWriter{}
	captureReplay(t, a, out)
	_, err = a.LoadSession(t.Context(), loadRequest(previous))
	require.NoError(t, err)
	assert.Same(t, winner, a.sessions[winner.id])
	assert.Equal(t, int32(1), ts.stops.Load())
	assert.Contains(t, strings.Join(out.lines(), "\n"), "winner history")
	assert.Len(t, replayUpdates(t, out, previous.id), 3)
}

func TestLoadCanceledColdConstructionCleansCandidate(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.NoError(t, err)
	loaded, ts := loadedLifecycleTeam(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) { cancel(); return loaded, nil }
	out := &captureWriter{}
	captureReplay(t, a, out)
	_, err = a.LoadSession(ctx, loadRequest(s))
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, a.sessions)
	assert.Empty(t, out.lines())
	assert.Equal(t, int32(1), ts.stops.Load())
}

func TestLoadReplacesClientMCPWithoutInvokingTools(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	out := &captureWriter{}
	captureReplay(t, a, out)
	wd, markers := t.TempDir(), t.TempDir()
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{clientServerSpec(t, "old", "old")}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.sess.AddMessage(session.UserMessage("saved history"))
	oldTool := clientTool(t, s, "inspect")
	before := s.clientMCP.generation()
	req := loadRequest(s)
	req.McpServers = []acpsdk.McpServer{{Stdio: &acpsdk.McpServerStdio{Name: "bad", Command: filepath.Join(markers, "not-an-executable")}}}
	_, err = a.LoadSession(t.Context(), req)
	require.Error(t, err)
	assert.Same(t, before, s.clientMCP.generation())
	assert.Same(t, s, a.sessions[s.id])
	spec := clientServerSpec(t, "new", "new")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_CALL_STARTED", Value: filepath.Join(markers, "called")})
	req.McpServers = []acpsdk.McpServer{spec}
	_, err = a.LoadSession(t.Context(), req)
	require.NoError(t, err)
	assert.NotEqual(t, oldTool.Name, clientTool(t, s, "inspect").Name)
	assert.NoFileExists(t, filepath.Join(markers, "called"))
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	available, err := s.clientMCP.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, available)
}

func TestInitializeAdvertisesLoadSession(t *testing.T) {
	t.Parallel()
	a := NewAgent(config.NewBytesSource("test.yaml", []byte("")), &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		loaded, _ := loadedLifecycleTeam(t)
		return loaded, nil
	}
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	response, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	require.NoError(t, err)
	assert.True(t, response.AgentCapabilities.LoadSession)
}

func TestLoadAbandonedCallDoesNotConsumeReusedIDResult(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	out := &captureWriter{}
	captureReplay(t, a, out)
	call := tools.ToolCall{ID: "reused", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"old"}`}}
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}})
	s.sess.AddMessage(session.UserMessage("retry"))
	call.Function.Arguments = `{"cmd":"new"}`
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}})
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "reused", Content: "success"}})
	_, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	updates := replayUpdates(t, out, s.id)
	require.Len(t, updates, 5)
	assert.Equal(t, acpsdk.ToolCallStatusFailed, updates[0].ToolCall.Status)
	assert.Contains(t, updates[0].ToolCall.Content[0].Content.Content.Text.Text, "outcome is unknown")
	assert.Equal(t, acpsdk.ToolCallStatusCompleted, updates[2].ToolCall.Status)
	assert.Equal(t, updates[2].ToolCall.ToolCallId, updates[3].ToolCallUpdate.ToolCallId)
}

func TestLoadOversizedOptionalMetadataDoesNotPreventReplay(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	out := &captureWriter{}
	captureReplay(t, a, out)
	paths := strings.Repeat(`"a",`, 14999) + `"a"`
	s.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "large", Function: tools.FunctionCall{Name: "read_multiple_files", Arguments: `{"paths":[` + paths + `]}`}}}, ToolDefinitions: []tools.Tool{{Name: "read_multiple_files", Annotations: tools.ToolAnnotations{Title: strings.Repeat("title", maxReplayUpdateBytes)}}}}})
	s.sess.AddMessage(session.UserMessage("", chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: strings.Repeat("name", maxReplayUpdateBytes), MimeType: strings.Repeat("mime", maxReplayUpdateBytes), Source: chat.DocumentSource{InlineData: []byte("binary")}}}))
	_, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	updates := replayUpdates(t, out, s.id)
	require.NotNil(t, updates[0].ToolCall)
	assert.LessOrEqual(t, len(updates[0].ToolCall.Title), chat.MaxSanitizedFieldBytes)
	assert.Nil(t, updates[0].ToolCall.RawInput)
	assert.Empty(t, updates[0].ToolCall.Locations)
	require.NotNil(t, updates[1].UserMessageChunk.Content.Resource)
	assert.Equal(t, "application/octet-stream", *updates[1].UserMessageChunk.Content.Resource.Resource.BlobResourceContents.MimeType)
	for _, line := range out.lines() {
		assert.Less(t, len(line), maxReplayUpdateBytes)
	}
}

type partialReplayStream struct{ sent bool }

func (s *partialReplayStream) Recv() (chat.MessageStreamResponse, error) {
	if !s.sent {
		s.sent = true
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "partial before failure"}}}}, nil
	}
	return chat.MessageStreamResponse{}, errors.New("terminal stream failure")
}
func (*partialReplayStream) Close() {}

func TestLoadSQLiteActiveIncludesPersistedPartialAndError(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a := NewAgent(nil, &config.RuntimeConfig{}, store)
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "partial"), stream: &partialReplayStream{}}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
	}
	out := &captureWriter{}
	captureReplay(t, a, out)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("question")}})
	require.Error(t, err)
	before := len(out.lines())
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	active := out.lines()[before:]
	assert.Contains(t, strings.Join(active, "\n"), "partial before failure")
	assert.Contains(t, strings.Join(active, "\n"), "terminal stream failure")
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	before = len(out.lines())
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	assert.Equal(t, active, out.lines()[before:], "active/cold replay must use the same persisted transcript")
}

func TestLoadNeverExposesPreTransformToolInput(t *testing.T) {
	t.Parallel()
	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a := NewAgent(nil, &config.RuntimeConfig{}, store)
	a.team = team.New()
	calls := 0
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		prov := &outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "redaction")}, streams: []chat.MessageStream{
			usageStream(&tools.ToolCall{ID: "call", Type: "function", Function: tools.FunctionCall{Name: "sample", Arguments: `{"path":"` + secret + `"}`}}, 10, 1), usageStream(nil, 10, 1),
		}}
		root := agent.New("root", "test", agent.WithModel(prov), agent.WithTools(tools.Tool{Name: "sample", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}, Handler: func(_ context.Context, call tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			calls++
			assert.NotContains(t, call.Function.Arguments, secret)
			return tools.ResultSuccess("safe"), nil
		}}), agent.WithHooks(&latest.HooksConfig{ToolInputTransform: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{Type: "builtin", Command: "redact_secrets"}}}}}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
	}
	out := &captureWriter{}
	captureReplay(t, a, out)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.sess.SetToolsApproved(true)
	_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("use sample")}})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	stored, err := store.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), secret, "fixture proves stored input predates transformation")
	assert.NotContains(t, strings.Join(out.lines(), "\n"), secret)
	for _, cold := range []bool{false, true} {
		if cold {
			_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
			require.NoError(t, err)
		}
		before := len(out.lines())
		_, err = a.LoadSession(t.Context(), loadRequest(s))
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(out.lines()[before:], "\n"), secret)
		assert.Equal(t, 1, calls, "loading must not rerun tools or transforms")
	}
}

func TestLoadDoesNotCancelRunningOrDrainingPrompt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rt := &drainingPromptRuntime{started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
		a, s, _ := newPromptTestAgent(t, rt)
		a.team = team.New()
		s.workingDir = t.TempDir()
		done := promptAsync(a, t.Context(), promptRequest("work"))
		<-rt.started
		_, err := a.LoadSession(t.Context(), loadRequest(s))
		require.ErrorContains(t, err, "running or pending prompt")
		select {
		case <-rt.stopped:
			t.Fatal("load canceled the running prompt")
		default:
		}
		require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: acpsdk.SessionId(s.id)}))
		synctest.Wait()
		_, err = a.LoadSession(t.Context(), loadRequest(s))
		require.ErrorContains(t, err, "running or pending prompt")
		close(rt.release)
		require.NoError(t, (<-done).err)
	})
}

func TestLoadColdPublishedCandidateBlocksPrompts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		s.sess.AddMessage(session.UserMessage("history"))
		_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.NoError(t, err)
		a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
			loaded, _ := loadedLifecycleTeam(t)
			return loaded, nil
		}
		entered, release := make(chan struct{}), make(chan struct{})
		out := &captureWriter{failOn: func(n int) error {
			if n == 1 {
				close(entered)
				<-release
			}
			return nil
		}}
		captureReplay(t, a, out)
		done := make(chan error, 1)
		go func() { _, err := a.LoadSession(t.Context(), loadRequest(s)); done <- err }()
		<-entered
		_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(s.id), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("must not run")}})
		require.ErrorContains(t, err, "loading")
		close(release)
		require.NoError(t, <-done)
		assert.Len(t, a.sessions[s.id].sess.OwnMessages(), 1)
	})
}

func TestLoadColdReplayFailureClosesCandidate(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	s.sess.AddMessage(session.UserMessage("history"))
	_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.NoError(t, err)
	loaded, ts := loadedLifecycleTeam(t)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) { return loaded, nil }
	out := &captureWriter{failOn: func(int) error { return errors.New("writer failed") }}
	captureReplay(t, a, out)
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.ErrorContains(t, err, "writer failed")
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.NoError(t, err)
	assert.Empty(t, a.sessions)
	assert.Equal(t, int32(1), ts.stops.Load())
}
