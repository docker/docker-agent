package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func TestResolvePath(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	outsideDir := t.TempDir()

	ts := &FilesystemToolset{
		workingDir: workingDir,
	}

	absWorkingDir, err := filepath.EvalSymlinks(workingDir)
	require.NoError(t, err)
	absOutsideDir, err := filepath.EvalSymlinks(outsideDir)
	require.NoError(t, err)

	tests := []struct {
		name      string
		userPath  string
		wantPath  string
		wantError bool
	}{
		{
			name:     "simple relative path",
			userPath: "file.txt",
			wantPath: filepath.Join(absWorkingDir, "file.txt"),
		},
		{
			name:     "nested relative path",
			userPath: "subdir/file.txt",
			wantPath: filepath.Join(absWorkingDir, "subdir", "file.txt"),
		},
		{
			name:     "dot path resolves to working directory",
			userPath: ".",
			wantPath: absWorkingDir,
		},
		{
			name:      "parent directory escape blocked",
			userPath:  "../escape.txt",
			wantError: true,
		},
		{
			name:      "deep parent directory escape blocked",
			userPath:  "subdir/../../escape.txt",
			wantError: true,
		},
		{
			name:     "dot-dot within working dir is fine",
			userPath: "subdir/../file.txt",
			wantPath: filepath.Join(absWorkingDir, "file.txt"),
		},
		{
			name:     "absolute path inside working dir is allowed",
			userPath: filepath.Join(absWorkingDir, "file.txt"),
			wantPath: filepath.Join(absWorkingDir, "file.txt"),
		},
		{
			name:      "absolute path outside working dir is blocked",
			userPath:  filepath.Join(absOutsideDir, "file.txt"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolved, err := ts.resolvePath(tt.userPath)
			if tt.wantError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "escapes the working directory")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, resolved)
		})
	}
}

func TestResolvePath_AdditionalDirectories(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	additionalDir := t.TempDir()
	outsideDir := t.TempDir()

	absWorkingDir, err := filepath.EvalSymlinks(workingDir)
	require.NoError(t, err)
	absAdditionalDir, err := filepath.EvalSymlinks(additionalDir)
	require.NoError(t, err)
	absOutsideDir, err := filepath.EvalSymlinks(outsideDir)
	require.NoError(t, err)

	resolved, err := resolvePathInRoots("relative.txt", workingDir, []string{workingDir, additionalDir})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(absWorkingDir, "relative.txt"), resolved)

	resolved, err = resolvePathInRoots(filepath.Join(additionalDir, "attached.txt"), workingDir, []string{workingDir, additionalDir})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(absAdditionalDir, "attached.txt"), resolved)

	_, err = resolvePathInRoots(filepath.Join(absOutsideDir, "blocked.txt"), workingDir, []string{workingDir, additionalDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}

func TestNormalizePathForComparison(t *testing.T) {
	t.Parallel()

	// On macOS/Windows (case-insensitive), normalization should lowercase.
	// On Linux (case-sensitive), it should be identity.
	result := normalizePathForComparison("/Some/Path")

	// This test validates the function exists and returns a string.
	// The exact behavior depends on the platform.
	assert.NotEmpty(t, result)
}

func TestResolvePath_SymlinkEscape(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	workingDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a secret file outside the working directory.
	secretFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("secret"), 0o644))

	// Create a symlink inside the working directory pointing outside.
	symlink := filepath.Join(workingDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, symlink))

	ts := &FilesystemToolset{workingDir: workingDir}

	// Accessing a file through the symlink should be blocked.
	_, err := ts.resolvePath("escape/secret.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")

	// The symlink target itself should also be blocked.
	_, err = ts.resolvePath("escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}

func TestResolvePath_SymlinkWithinWorkingDir(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	workingDir := t.TempDir()

	// Create a subdirectory and a symlink to it within the working dir.
	subdir := filepath.Join(workingDir, "real")
	require.NoError(t, os.Mkdir(subdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "file.txt"), []byte("ok"), 0o644))

	link := filepath.Join(workingDir, "link")
	require.NoError(t, os.Symlink(subdir, link))

	ts := &FilesystemToolset{workingDir: workingDir}

	// Symlink within working dir should be allowed.
	resolved, err := ts.resolvePath("link/file.txt")
	require.NoError(t, err)
	assert.Contains(t, resolved, "real/file.txt")
}

func TestResolvePath_NonExistentPathWithSymlinkAncestor(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	workingDir := t.TempDir()
	outsideDir := t.TempDir()

	// Symlink inside working dir pointing outside.
	symlink := filepath.Join(workingDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, symlink))

	ts := &FilesystemToolset{workingDir: workingDir}

	// Even for a non-existent file under the symlink, traversal should be blocked.
	_, err := ts.resolvePath("escape/nonexistent.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}

// readTextFileResponder plays the ACP client side of the connection for
// fs/read_text_file requests: each decoded request is recorded and answered
// with the configured content. Any other JSON-RPC request fails the test
// immediately instead of deadlocking the sender.
type readTextFileResponder struct {
	t       *testing.T
	peer    io.Writer // write half of the connection's inbound peer pipe
	content string

	mu       sync.Mutex
	requests []acpsdk.ReadTextFileRequest
}

func (p *readTextFileResponder) Write(b []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		p.t.Errorf("peer received malformed JSON-RPC message %q: %v", b, err)
		return 0, err
	}
	if len(msg.ID) == 0 || msg.Method != acpsdk.ClientMethodFsReadTextFile {
		err := fmt.Errorf("peer cannot answer JSON-RPC message %q (id %s)", msg.Method, msg.ID)
		p.t.Error(err)
		return 0, err
	}

	var req acpsdk.ReadTextFileRequest
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		p.t.Errorf("peer failed to decode %s params: %v", msg.Method, err)
		return 0, err
	}
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	response, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: "2.0", ID: msg.ID, Result: acpsdk.ReadTextFileResponse{Content: p.content}})
	if err != nil {
		return 0, fmt.Errorf("marshal response: %w", err)
	}
	if _, err := p.peer.Write(append(response, '\n')); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *readTextFileResponder) recordedRequests() []acpsdk.ReadTextFileRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]acpsdk.ReadTextFileRequest(nil), p.requests...)
}

// TestFilesystemToolset_ReadFileForwardsLineRange verifies that the ACP
// read_file override forwards the optional line/limit arguments to the
// client's fs/read_text_file request and leaves them unset for a path-only
// call.
func TestFilesystemToolset_ReadFileForwardsLineRange(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	const sessionID = "read-range-session"

	acpAgent := &Agent{
		sessions: map[string]*Session{sessionID: {id: sessionID, workingDir: workingDir}},
		clientFS: acpsdk.FileSystemCapabilities{ReadTextFile: true},
	}

	peerReader, peerWriter := io.Pipe()
	responder := &readTextFileResponder{t: t, peer: peerWriter, content: "two\nthree\n"}
	conn := acpsdk.NewAgentSideConnection(acpAgent, responder, peerReader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	acpAgent.SetAgentConnection(conn)
	t.Cleanup(func() {
		_ = peerWriter.Close()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for ACP connection shutdown")
		}
	})

	ts := NewFilesystemToolset(acpAgent, workingDir)
	ctx := withSessionID(t.Context(), sessionID)

	result, err := ts.handleReadFile(ctx, tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      filesystem.ToolNameReadFile,
			Arguments: `{"path": "notes.txt", "line": 2, "limit": 2}`,
		},
	}, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	assert.Equal(t, "two\nthree\n", result.Output)

	result, err = ts.handleReadFile(ctx, tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      filesystem.ToolNameReadFile,
			Arguments: `{"path": "notes.txt"}`,
		},
	}, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)

	reqs := responder.recordedRequests()
	require.Len(t, reqs, 2)

	ranged := reqs[0]
	assert.Equal(t, acpsdk.SessionId(sessionID), ranged.SessionId)
	assert.Equal(t, "notes.txt", filepath.Base(ranged.Path))
	assert.True(t, filepath.IsAbs(ranged.Path), "ACP read requests must carry absolute paths")
	require.NotNil(t, ranged.Line)
	assert.Equal(t, 2, *ranged.Line)
	require.NotNil(t, ranged.Limit)
	assert.Equal(t, 2, *ranged.Limit)

	pathOnly := reqs[1]
	assert.Nil(t, pathOnly.Line, "path-only read must not invent a line")
	assert.Nil(t, pathOnly.Limit, "path-only read must not invent a limit")
}

// TestFilesystemToolset_ReadFileRejectsInvalidRange verifies that the ACP
// read_file override applies the same line/limit contract as the builtin
// filesystem toolset: explicitly invalid values are rejected with a tool
// error before any fs/read_text_file request reaches the client.
func TestFilesystemToolset_ReadFileRejectsInvalidRange(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	const sessionID = "invalid-range-session"

	acpAgent := &Agent{
		sessions: map[string]*Session{sessionID: {id: sessionID, workingDir: workingDir}},
		clientFS: acpsdk.FileSystemCapabilities{ReadTextFile: true},
	}

	peerReader, peerWriter := io.Pipe()
	responder := &readTextFileResponder{t: t, peer: peerWriter, content: "unreachable"}
	conn := acpsdk.NewAgentSideConnection(acpAgent, responder, peerReader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	acpAgent.SetAgentConnection(conn)
	t.Cleanup(func() {
		_ = peerWriter.Close()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for ACP connection shutdown")
		}
	})

	ts := NewFilesystemToolset(acpAgent, workingDir)
	ctx := withSessionID(t.Context(), sessionID)

	for _, tc := range []struct {
		name      string
		arguments string
		wantErr   string
	}{
		{"zero line", `{"path": "notes.txt", "line": 0}`, "invalid line 0"},
		{"negative line", `{"path": "notes.txt", "line": -3}`, "invalid line -3"},
		{"zero limit", `{"path": "notes.txt", "limit": 0}`, "invalid limit 0"},
		{"negative limit", `{"path": "notes.txt", "limit": -1}`, "invalid limit -1"},
	} {
		result, err := ts.handleReadFile(ctx, tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      filesystem.ToolNameReadFile,
				Arguments: tc.arguments,
			},
		}, nil)
		require.NoError(t, err, tc.name)
		require.NotNil(t, result, tc.name)
		assert.True(t, result.IsError, tc.name)
		assert.Contains(t, result.Output, tc.wantErr, tc.name)
	}

	assert.Empty(t, responder.recordedRequests(), "invalid ranges must be rejected before any RPC")
}

// editFileResponder answers both fs/read_text_file and fs/write_text_file so an
// edit_file round trip can be driven over a real AgentSideConnection. Written
// content is recorded, which is what lets a test assert that a refused edit
// never reached the client.
type editFileResponder struct {
	t       *testing.T
	peer    io.Writer
	content string

	mu      sync.Mutex
	written []string
}

func (p *editFileResponder) Write(b []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		p.t.Errorf("peer received malformed JSON-RPC message %q: %v", b, err)
		return 0, err
	}

	var result any
	switch msg.Method {
	case acpsdk.ClientMethodFsReadTextFile:
		result = acpsdk.ReadTextFileResponse{Content: p.content}
	case acpsdk.ClientMethodFsWriteTextFile:
		var req acpsdk.WriteTextFileRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			p.t.Errorf("peer failed to decode %s params: %v", msg.Method, err)
			return 0, err
		}
		p.mu.Lock()
		p.written = append(p.written, req.Content)
		p.mu.Unlock()
		result = acpsdk.WriteTextFileResponse{}
	default:
		err := fmt.Errorf("peer cannot answer JSON-RPC message %q (id %s)", msg.Method, msg.ID)
		p.t.Error(err)
		return 0, err
	}

	response, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: "2.0", ID: msg.ID, Result: result})
	if err != nil {
		return 0, fmt.Errorf("marshal response: %w", err)
	}
	if _, err := p.peer.Write(append(response, '\n')); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *editFileResponder) writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.written...)
}

// newEditFileFixture wires a FilesystemToolset to a real AgentSideConnection
// whose peer answers reads with content and records writes.
func newEditFileFixture(t *testing.T, content string) (*FilesystemToolset, context.Context, *editFileResponder) {
	t.Helper()

	workingDir := t.TempDir()
	const sessionID = "edit-session"

	acpAgent := &Agent{
		sessions: map[string]*Session{sessionID: {id: sessionID, workingDir: workingDir}},
		clientFS: acpsdk.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
	}

	peerReader, peerWriter := io.Pipe()
	responder := &editFileResponder{t: t, peer: peerWriter, content: content}
	conn := acpsdk.NewAgentSideConnection(acpAgent, responder, peerReader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	acpAgent.SetAgentConnection(conn)
	t.Cleanup(func() {
		_ = peerWriter.Close()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for ACP connection shutdown")
		}
	})

	return NewFilesystemToolset(acpAgent, workingDir), withSessionID(t.Context(), sessionID), responder
}

// The ACP toolset serves the same edit_file tool name and schema as the built-in
// filesystem toolset, so it must refuse an empty oldText for the same reason:
// strings.Contains always matches "" and strings.Replace would prepend.
func TestFilesystemToolset_EditFileRejectsEmptyOldText(t *testing.T) {
	t.Parallel()

	const original = "line one\nline two\n"
	ts, ctx, responder := newEditFileFixture(t, original)

	result, err := ts.handleEditFile(ctx, tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      filesystem.ToolNameEditFile,
			Arguments: `{"path":"f.txt","edits":[{"oldText":"","newText":"INJECTED"}]}`,
		},
	}, nil)
	require.NoError(t, err)
	assert.True(t, result.IsError, result.Output)
	assert.Contains(t, result.Output, "oldText must not be empty")
	assert.Empty(t, responder.writes(), "a refused edit must never reach the client")
}

// A normal edit still works, so the guard is not over-broad.
func TestFilesystemToolset_EditFileAppliesNonEmptyEdit(t *testing.T) {
	t.Parallel()

	ts, ctx, responder := newEditFileFixture(t, "line one\nline two\n")

	result, err := ts.handleEditFile(ctx, tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      filesystem.ToolNameEditFile,
			Arguments: `{"path":"f.txt","edits":[{"oldText":"line one","newText":"LINE ONE"}]}`,
		},
	}, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	assert.Equal(t, []string{"LINE ONE\nline two\n"}, responder.writes())
}
