package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func fileDiff(t *testing.T, result *tools.ToolCallResult) *acpsdk.ToolCallContentDiff {
	t.Helper()
	update := buildToolCallComplete(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: result.Output, Result: result})
	for _, content := range update.ToolCallUpdate.Content {
		if content.Diff != nil {
			return content.Diff
		}
	}
	return nil
}

func TestEditDiffCapturesWholeFile(t *testing.T) {
	t.Parallel()
	original := "header\r\nalpha alpha\r\nfooter"
	fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
	peer.content = original
	result, err := fs.handleEditFile(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path":"file.txt","edits":[{"oldText":"alpha","newText":"beta"},{"oldText":"beta alpha","newText":"finished"}]}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	diff := fileDiff(t, result)
	require.NotNil(t, diff)
	require.NotNil(t, diff.OldText)
	assert.Equal(t, original, *diff.OldText)
	assert.Equal(t, "header\r\nfinished\r\nfooter", diff.NewText)
	assert.True(t, filepath.IsAbs(diff.Path))
	reads, writes := peer.counts()
	assert.Equal(t, 1, reads)
	assert.Equal(t, 1, writes)
	peer.content = "later editor changes"
	require.NoError(t, os.WriteFile(diff.Path, []byte("later disk changes"), 0o600))
	assert.Equal(t, "header\r\nfinished\r\nfooter", fileDiff(t, result).NewText, "rendering must use captured content")
	reads, writes = peer.counts()
	assert.Equal(t, 1, reads)
	assert.Equal(t, 1, writes)
}

func TestWriteCompletionDoesNotReadOrInventBeforeState(t *testing.T) {
	t.Parallel()
	for _, readSupported := range []bool{false, true} {
		fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
		peer.content = strings.Repeat("x", 11<<20)
		fs.agent.clientFS.ReadTextFile = readSupported
		result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameWriteFile, "file.txt")
		require.False(t, result.IsError, result.Output)
		assert.Nil(t, fileDiff(t, result))
		reads, writes := peer.counts()
		assert.Zero(t, reads, "a large optional pre-read must not disconnect the client")
		assert.Equal(t, 1, writes)
	}
}

func TestFileDiffOmittedOnFailureOrHooks(t *testing.T) {
	t.Parallel()
	for _, name := range []string{filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
		for _, mode := range []string{"write failure", "configured hook", "hook failure"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				cfg := latest.Toolset{}
				if mode == "configured hook" {
					cfg.PostEdit = []latest.PostEditConfig{{Path: "*.go", Cmd: "exit 9"}}
				}
				if mode == "hook failure" {
					cfg.PostEdit = []latest.PostEditConfig{{Path: "*", Cmd: "exit 9"}}
				}
				fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), cfg)
				peer.failWrite = mode == "write failure"
				result := callPolicyFileTool(t, ctx, fs, name, "file.txt")
				assert.Nil(t, fileDiff(t, result))
				update := buildToolCallComplete(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: result.Output, Result: result})
				require.Len(t, update.ToolCallUpdate.Content, 1)
				assert.Equal(t, result.Output, update.ToolCallUpdate.Content[0].Content.Content.Text.Text)
				if mode != "configured hook" {
					assert.True(t, result.IsError)
					assert.Equal(t, acpsdk.ToolCallStatusFailed, *update.ToolCallUpdate.Status)
				}
				if mode == "hook failure" {
					assert.Contains(t, result.Output, "successfully but post-edit command failed")
				}
			})
		}
	}
}

func TestEditDiffDoesNotReuseSnapshotAfterTargetChanges(t *testing.T) {
	t.Parallel()
	if goruntime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	wd := t.TempDir()
	first, second := filepath.Join(wd, "first.txt"), filepath.Join(wd, "second.txt")
	require.NoError(t, os.WriteFile(first, []byte("original"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("other"), 0o600))
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{})
	peer.onRead = func() {
		assert.NoError(t, os.Remove(first))
		assert.NoError(t, os.Symlink(second, first))
	}
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameEditFile, "first.txt")
	require.False(t, result.IsError, result.Output)
	assert.Nil(t, fileDiff(t, result))
	peer.mu.Lock()
	defer peer.mu.Unlock()
	require.Len(t, peer.writes, 1)
	assert.Equal(t, "second.txt", filepath.Base(peer.writes[0].Path))
}

func TestFileDiffMetadataIsPresentationOnly(t *testing.T) {
	t.Parallel()
	fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
	peer.content = "private before snapshot original"
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameEditFile, "private-file.txt")
	result.Output = "transformed response"
	event := &runtime.ToolCallResponseEvent{ToolCallID: "call", Response: result.Output, Result: result.WithoutPayload()}
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private before snapshot")
	assert.NotContains(t, string(encoded), "private-file.txt")
	assert.NotContains(t, string(encoded), "updated")
	update := buildToolCallComplete(event)
	require.Len(t, update.ToolCallUpdate.Content, 2)
	assert.Equal(t, "transformed response", update.ToolCallUpdate.Content[0].Content.Content.Text.Text)
	assert.Equal(t, "private before snapshot original", *update.ToolCallUpdate.Content[1].Diff.OldText)
	assert.Equal(t, map[string]any{"content": "transformed response"}, update.ToolCallUpdate.RawOutput)
	result.Meta = map[string]any{"path": "/fake", "oldText": "forged", "newText": "forged"}
	assert.Nil(t, fileDiff(t, result), "generic metadata is not a trusted snapshot")
}

func TestToolLocationsUseSessionWorkspace(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	args := map[string]any{"path": "sub/../file.txt", "line": float64(3), "paths": []any{"other.txt", "https://example.com/file", "file:///tmp/file", ""}}
	want := []acpsdk.ToolCallLocation{{Path: filepath.Join(wd, "file.txt"), Line: new(3)}, {Path: filepath.Join(wd, "other.txt")}}
	assert.Equal(t, want, extractLocations(args, wd))
	data, err := json.Marshal(args)
	require.NoError(t, err)
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "read_file", Arguments: string(data)}}
	definition := tools.Tool{Name: "read_file"}
	assert.Equal(t, want, buildToolCallStart(call, definition, wd).ToolCall.Locations)
	assert.Equal(t, want, buildToolCallUpdate(call, definition, acpsdk.ToolCallStatusPending, wd).Locations)
	assert.Nil(t, extractLocations(map[string]any{"path": "relative.txt"}, ""))
	assert.Nil(t, extractLocations(map[string]any{"path": "relative.txt"}, "relative-workspace"))
	for _, line := range []float64{0, -1, 1.5} {
		got := extractLocations(map[string]any{"path": "file.txt", "line": line}, wd)
		require.Len(t, got, 1)
		assert.Nil(t, got[0].Line)
	}
	assert.Empty(t, toolLocationPath("https://example.com", wd))
	assert.Empty(t, toolLocationPath("file:///tmp/foo", wd))
	assert.Empty(t, toolLocationPath("bad\x00path", wd))
}

func TestToolLocationHomeExpansion(t *testing.T) {
	wd := t.TempDir()
	t.Setenv("USERPROFILE", wd)
	t.Setenv("HOME", wd)
	assert.Equal(t, filepath.Join(wd, "file.txt"), toolLocationPath("~/file.txt", t.TempDir()))
}

func TestWindowsToolLocations(t *testing.T) {
	t.Parallel()
	if goruntime.GOOS != "windows" {
		t.Skip("Windows path semantics")
	}
	for _, path := range []string{`C:\workspace\file.txt`, `\\server\share\file.txt`} {
		assert.Equal(t, filepath.Clean(path), toolLocationPath(path, `D:\workspace`))
	}
	for _, path := range []string{`C:relative.txt`, `\root-relative.txt`, `/root-relative.txt`, "https://example.com"} {
		assert.Empty(t, toolLocationPath(path, `D:\workspace`))
	}
	assert.Equal(t, `D:\workspace\file.txt`, toolLocationPath("file.txt", `D:\workspace`))
}

func TestEditDiffSizeBudget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	empty, err := json.Marshal(acpsdk.ToolCallContentDiff{Type: "diff", Path: path, OldText: new(""), NewText: ""})
	require.NoError(t, err)
	budget := maxFileDiffBytes - len(empty)
	before := strings.Repeat("a", budget/2)
	after := strings.Repeat("b", budget-len(before))
	require.NotNil(t, capturedFileChange(path, before, after))
	assert.Nil(t, capturedFileChange(path, before, after+"x"))
	assert.Nil(t, capturedFileChange(path, "\xff", "text"), "invalid UTF-8 cannot be represented as an exact JSON text snapshot")
	assert.Nil(t, capturedFileChange(path, strings.Repeat("<", maxFileDiffBytes/4), ""), "HTML escaping contributes six bytes per character")
	for _, text := range []string{"plain", "\"\\\t\n\r\b\f", "\x00<>&", "é😀\u2028\u2029"} {
		encoded, err := json.Marshal(text)
		require.NoError(t, err)
		remaining, ok := consumeJSONStringBudget(text, len(encoded)-2)
		assert.True(t, ok)
		assert.Zero(t, remaining)
	}
}

func TestLargeEditCompletesWithoutDiff(t *testing.T) {
	t.Parallel()
	fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
	peer.content = "original" + strings.Repeat("x", 6<<20)
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameEditFile, "large.txt")
	require.False(t, result.IsError, result.Output)
	assert.Nil(t, result.Meta, "oversized snapshots must not be retained")
	update := buildToolCallComplete(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: result.Output, Result: result})
	encoded, err := json.Marshal(update)
	require.NoError(t, err)
	assert.Less(t, len(encoded), maxFileDiffBytes)
	reads, writes := peer.counts()
	assert.Equal(t, 1, reads)
	assert.Equal(t, 1, writes)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	assert.True(t, strings.HasPrefix(peer.writes[0].Content, "updated"))
}

type editSnapshotProvider struct {
	mockProvider

	called bool
}

func (p *editSnapshotProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	if !p.called {
		p.called = true
		return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{
			Delta:        chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "edit", Type: "function", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path":"file.txt","edits":[{"oldText":"original","newText":"updated"}]}`}}}},
			FinishReason: chat.FinishReasonToolCalls,
		}}}}}, nil
	}
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Done"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

func TestRuntimeEditSnapshotSurvivesOutputTransform(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	fs, _, peer := newPolicyFileFixture(t, wd, latest.Toolset{})
	const privateContent = "private full-file context\r\noriginal\r\ntail"
	peer.content = privateContent
	out := &captureWriter{}
	peer.notifications = out
	registry := hooks.NewRegistry()
	var hookOutput string
	require.NoError(t, registry.RegisterBuiltin("rewrite", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{UpdatedToolResponse: new("transformed response")}}, nil
	}))
	require.NoError(t, registry.RegisterBuiltin("observe", func(_ context.Context, input *hooks.Input, _ []string) (*hooks.Output, error) {
		encoded, err := json.Marshal(input)
		hookOutput = string(encoded)
		return nil, err
	}))
	root := agent.New("root", "test", agent.WithModel(&editSnapshotProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "diff")}}), agent.WithToolSets(fs), agent.WithHooks(&latest.HooksConfig{
		ToolResponseTransform: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{Type: "builtin", Command: "rewrite"}}}},
		PostToolUse:           []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{Type: "builtin", Command: "observe"}}}},
	}))
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false), runtime.WithSessionStore(store), runtime.WithHooksRegistry(registry))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a := fs.agent
	s := a.sessions["policy-session"]
	s.rt = rt
	s.sess = session.New(session.WithID(s.id), session.WithWorkingDir(wd), session.WithToolsApproved(true))
	require.NoError(t, store.AddSession(t.Context(), s.sess))
	response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(s.id), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("edit the file")}})
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	var diff *acpsdk.ToolCallContentDiff
	for _, line := range out.lines() {
		var message struct {
			Params acpsdk.SessionNotification `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &message))
		if update := message.Params.Update.ToolCallUpdate; update != nil && update.Status != nil && *update.Status == acpsdk.ToolCallStatusCompleted {
			require.Len(t, update.Content, 2)
			diff = update.Content[1].Diff
			assert.Equal(t, "transformed response", update.Content[0].Content.Content.Text.Text)
			assert.Equal(t, map[string]any{"content": "transformed response"}, update.RawOutput)
		}
	}
	require.NotNil(t, diff)
	assert.Equal(t, privateContent, *diff.OldText)
	assert.Equal(t, strings.Replace(privateContent, "original", "updated", 1), diff.NewText)
	persisted, err := store.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	encoded, err := json.Marshal(persisted)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private full-file context")
	assert.Contains(t, string(encoded), "transformed response")
	assert.NotContains(t, hookOutput, "private full-file context")
	assert.Contains(t, hookOutput, "transformed response")
}
