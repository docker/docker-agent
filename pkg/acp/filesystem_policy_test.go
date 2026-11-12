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
	"strconv"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

type policyFilePeer struct {
	notifications io.Writer
	peer          io.Writer
	content       string
	failRead      bool
	failWrite     bool
	readResponse  func(acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, *acpsdk.RequestError)
	onRead        func()
	onWrite       func(acpsdk.WriteTextFileRequest)

	mu     sync.Mutex
	reads  []acpsdk.ReadTextFileRequest
	writes []acpsdk.WriteTextFileRequest
}

func (p *policyFilePeer) Write(b []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		return 0, err
	}
	if len(msg.ID) == 0 && p.notifications != nil {
		return p.notifications.Write(b)
	}
	var result any
	var rpcErr *acpsdk.RequestError
	switch msg.Method {
	case acpsdk.ClientMethodFsReadTextFile:
		var req acpsdk.ReadTextFileRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			return 0, err
		}
		p.mu.Lock()
		p.reads = append(p.reads, req)
		p.mu.Unlock()
		if p.onRead != nil {
			p.onRead()
		}
		result = acpsdk.ReadTextFileResponse{Content: p.content}
		if p.readResponse != nil {
			result, rpcErr = p.readResponse(req)
		}
		if p.failRead {
			rpcErr = acpsdk.NewInternalError("read failed")
		}
	case acpsdk.ClientMethodFsWriteTextFile:
		var req acpsdk.WriteTextFileRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			return 0, err
		}
		p.mu.Lock()
		p.writes = append(p.writes, req)
		p.mu.Unlock()
		result = acpsdk.WriteTextFileResponse{}
		if p.failWrite {
			rpcErr = acpsdk.NewInternalError("write failed")
		} else if p.onWrite != nil {
			p.onWrite(req)
		}
	default:
		return 0, fmt.Errorf("unexpected method %s", msg.Method)
	}
	response := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return 0, err
	}
	if _, err := p.peer.Write(append(encoded, '\n')); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *policyFilePeer) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reads), len(p.writes)
}

func newPolicyFileFixture(t *testing.T, wd string, cfg latest.Toolset, additional ...string) (*FilesystemToolset, context.Context, *policyFilePeer) {
	t.Helper()
	const sid = "policy-session"
	a := &Agent{
		sessions: map[string]*Session{sid: {id: sid, workingDir: wd, additionalDirs: additional}},
		clientFS: acpsdk.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
	}
	reader, writer := io.Pipe()
	peer := &policyFilePeer{peer: writer, content: "original"}
	conn := acpsdk.NewAgentSideConnection(a, peer, reader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	a.SetAgentConnection(conn)
	t.Cleanup(func() {
		_ = writer.Close()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for ACP connection shutdown")
		}
	})
	cfg.Type = "filesystem"
	ts, err := createToolsetRegistry(a).CreateTool(t.Context(), cfg, wd,
		&config.RuntimeConfig{Config: config.Config{WorkingDir: wd}}, "root")
	require.NoError(t, err)
	fs, ok := ts.(*FilesystemToolset)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, fs.Close()) })
	return fs, withSessionID(t.Context(), sid), peer
}

func callPolicyFileTool(t *testing.T, ctx context.Context, fs *FilesystemToolset, name, path string) *tools.ToolCallResult {
	t.Helper()
	args := map[string]any{"path": path}
	switch name {
	case filesystem.ToolNameReadMultipleFiles:
		args = map[string]any{"paths": []string{path}}
	case filesystem.ToolNameWriteFile:
		args["content"] = "updated"
	case filesystem.ToolNameEditFile:
		args["edits"] = []filesystem.Edit{{OldText: "original", NewText: "updated"}}
	}
	data, err := json.Marshal(args)
	require.NoError(t, err)
	available, err := fs.Tools(ctx)
	require.NoError(t, err)
	for _, tool := range available {
		if tool.Name != name {
			continue
		}
		result, err := tool.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: string(data)}}, tools.NopRuntime{})
		require.NoError(t, err)
		require.NotNil(t, result)
		if name == filesystem.ToolNameReadMultipleFiles {
			meta, ok := result.Meta.(filesystem.ReadMultipleFilesMeta)
			require.True(t, ok)
			require.Len(t, meta.Files, 1)
			assert.False(t, result.IsError, "batch failures are per-file")
			// Share single-path policy assertions with the batch tool.
			result.IsError = meta.Files[0].Error != ""
		}
		return result
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func TestFilesystemPolicyRejectsBeforeRPC(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     latest.Toolset
		path    string
		ignore  string
		wantErr string
	}{
		{name: "deny wins", cfg: latest.Toolset{AllowList: []string{"."}, DenyList: []string{"blocked"}}, path: "blocked/file.txt", wantErr: "denied directory"},
		{name: "allow miss", cfg: latest.Toolset{AllowList: []string{"allowed"}}, path: "blocked/file.txt", wantErr: "outside the allowed"},
		{name: "allow sibling prefix", cfg: latest.Toolset{AllowList: []string{"allowed"}}, path: "allowed-other/file.txt", wantErr: "outside the allowed"},
		{name: "invalid allow", cfg: latest.Toolset{AllowList: []string{""}}, path: "file.txt", wantErr: "disabled"},
		{name: "invalid deny", cfg: latest.Toolset{DenyList: []string{""}}, path: "file.txt", wantErr: "disabled"},
		{name: "agentsignore with vcs disabled", cfg: latest.Toolset{IgnoreVCS: new(false)}, ignore: "*.txt\n", path: "missing/file.txt", wantErr: ".agentsignore"},
		{name: "ignore file itself", ignore: "*.txt\n", path: ".agentsignore", wantErr: ".agentsignore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wd := t.TempDir()
			if tc.ignore != "" {
				require.NoError(t, os.WriteFile(filepath.Join(wd, ".agentsignore"), []byte(tc.ignore), 0o600))
			}
			tc.cfg.PostEdit = []latest.PostEditConfig{{Path: "*", Cmd: "exit 99"}}
			fs, ctx, peer := newPolicyFileFixture(t, wd, tc.cfg)
			for _, name := range []string{filesystem.ToolNameReadFile, filesystem.ToolNameReadMultipleFiles, filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
				result := callPolicyFileTool(t, ctx, fs, name, tc.path)
				assert.True(t, result.IsError, name)
				assert.Contains(t, result.Output, tc.wantErr, name)
				assert.NotContains(t, result.Output, "post-edit", name)
			}
			reads, writes := peer.counts()
			assert.Zero(t, reads)
			assert.Zero(t, writes)
		})
	}
}

func TestFilesystemPolicyAdditionalRoots(t *testing.T) {
	t.Parallel()
	for _, allowed := range []bool{false, true} {
		t.Run(strconv.FormatBool(allowed), func(t *testing.T) {
			t.Parallel()
			wd, extra, outside := t.TempDir(), t.TempDir(), t.TempDir()
			roots := []string{"."}
			if allowed {
				roots = append(roots, extra)
			}
			fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: roots}, extra)
			for _, name := range []string{filesystem.ToolNameReadFile, filesystem.ToolNameReadMultipleFiles, filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
				result := callPolicyFileTool(t, ctx, fs, name, filepath.Join(extra, "file.txt"))
				assert.Equal(t, !allowed, result.IsError, result.Output)
			}
			reads, writes := peer.counts()
			if allowed {
				assert.Equal(t, 3, reads)
				assert.Equal(t, 2, writes)
			} else {
				assert.Zero(t, reads)
				assert.Zero(t, writes)
			}
			fs2, ctx2, peer2 := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: []string{outside}})
			result := callPolicyFileTool(t, ctx2, fs2, filesystem.ToolNameWriteFile, filepath.Join(outside, "file.txt"))
			assert.True(t, result.IsError)
			assert.Contains(t, result.Output, "escapes the working directory")
			reads, writes = peer2.counts()
			assert.Zero(t, reads)
			assert.Zero(t, writes)
		})
	}
}

func TestFilesystemPolicySymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	wd, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(wd, "denied"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(wd, "denied", "secret.txt"), []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(wd, "denied"), filepath.Join(wd, "alias")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "missing.txt"), filepath.Join(wd, "dangling")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "missing-dir"), filepath.Join(wd, "dangling-dir")))
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: []string{"."}, DenyList: []string{"denied"}})
	for _, path := range []string{"alias/secret.txt", "dangling", "dangling-dir/file.txt"} {
		for _, name := range []string{filesystem.ToolNameReadFile, filesystem.ToolNameReadMultipleFiles, filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
			result := callPolicyFileTool(t, ctx, fs, name, path)
			assert.True(t, result.IsError, "%s %s: %s", name, path, result.Output)
		}
	}
	reads, writes := peer.counts()
	assert.Zero(t, reads)
	assert.Zero(t, writes)
}

func TestFilesystemPolicyRechecksEditBeforeWrite(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	wd, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(wd, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: []string{"."}})
	peer.onRead = func() {
		assert.NoError(t, os.Remove(path))
		assert.NoError(t, os.Symlink(secret, path))
	}
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameEditFile, "file.txt")
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "outside the allowed")
	reads, writes := peer.counts()
	assert.Equal(t, 1, reads)
	assert.Zero(t, writes)
}

func TestFilesystemRegistryRejectsUnreadableIgnoreRules(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	ignorePath := filepath.Join(wd, ".agentsignore")
	require.NoError(t, os.WriteFile(ignorePath, []byte("secrets.env\n"), 0o600))
	makeUnreadable(t, ignorePath)
	ts, err := createToolsetRegistry(&Agent{}).CreateTool(t.Context(), latest.Toolset{Type: "filesystem"}, wd,
		&config.RuntimeConfig{Config: config.Config{WorkingDir: wd}}, "root")
	require.ErrorContains(t, err, ".agentsignore")
	assert.Nil(t, ts)
}

func TestFilesystemPostEditCommands(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	for _, name := range []string{filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
		for _, tc := range []struct {
			name      string
			command   string
			pattern   string
			failRead  bool
			failWrite bool
			noMatch   bool
			wantError string
		}{
			{name: "success", pattern: "sub/*.txt", command: `test "$(cat "${file}")" = updated && printf first >> hook-output`},
			{name: "no matching hook", pattern: "*.go", command: "exit 9", noMatch: true},
			{name: "read failed", pattern: "*", command: "exit 9", failRead: true, wantError: "Error reading file"},
			{name: "write failed", pattern: "*", command: "exit 9", failWrite: true, wantError: "Error writing file"},
			{name: "hook failed", pattern: "*", command: "exit 9", wantError: "successfully but post-edit command failed"},
		} {
			if tc.failRead && name != filesystem.ToolNameEditFile {
				continue
			}
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				wd := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(wd, "sub"), 0o700))
				fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{PostEdit: []latest.PostEditConfig{
					{Path: tc.pattern, Cmd: tc.command},
					{Path: tc.pattern, Cmd: `printf second >> hook-output`},
				}})
				peer.failRead = tc.failRead
				peer.failWrite = tc.failWrite
				peer.onWrite = func(req acpsdk.WriteTextFileRequest) {
					assert.NoError(t, os.WriteFile(req.Path, []byte(req.Content), 0o600))
				}
				result := callPolicyFileTool(t, ctx, fs, name, "sub/edited file.txt")
				if tc.wantError != "" {
					assert.True(t, result.IsError, result.Output)
					assert.Contains(t, result.Output, tc.wantError)
					assert.NoFileExists(t, filepath.Join(wd, "hook-output"))
				} else {
					assert.False(t, result.IsError, result.Output)
					if tc.noMatch {
						assert.NoFileExists(t, filepath.Join(wd, "hook-output"))
					} else {
						data, err := os.ReadFile(filepath.Join(wd, "hook-output"))
						require.NoError(t, err)
						assert.Equal(t, "firstsecond", string(data))
					}
				}
				_, writes := peer.counts()
				if tc.failRead {
					assert.Zero(t, writes)
				} else {
					assert.Equal(t, 1, writes, "hook failures must not retry writes")
				}
			})
		}
	}
}

func TestFilesystemPostEditRequiresSharedFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	wd := t.TempDir()
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{PostEdit: []latest.PostEditConfig{{Path: "*", Cmd: `cat "${file}"`}}})
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameWriteFile, "buffer-only.txt")
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "File written successfully but post-edit command failed")
	_, writes := peer.counts()
	assert.Equal(t, 1, writes)
	assert.NoFileExists(t, filepath.Join(wd, "buffer-only.txt"), "ACP must not mirror editor content to local disk")
}

func TestFilesystemPostEditUsesCanonicalTarget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell and symlinks")
	}
	for _, name := range []string{filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspace, outside := t.TempDir(), t.TempDir()
			wd := filepath.Join(t.TempDir(), "workspace-link")
			require.NoError(t, os.Symlink(workspace, wd))
			require.NoError(t, os.Mkdir(filepath.Join(workspace, "sub"), 0o700))
			target := filepath.Join(workspace, "sub", "target.txt")
			require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))
			outsideFile := filepath.Join(outside, "secret.txt")
			require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o600))
			alias := filepath.Join(workspace, "alias.go")
			require.NoError(t, os.Symlink(target, alias))
			fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: []string{"."}, PostEdit: []latest.PostEditConfig{
				{Path: "*.go", Cmd: "exit 9"},
				{Path: "sub/*.txt", Cmd: `printf '%s' "${file}" > hook-target; printf formatted >> "${file}"`},
			}})
			peer.onWrite = func(req acpsdk.WriteTextFileRequest) {
				assert.NoError(t, os.WriteFile(req.Path, []byte(req.Content), 0o600))
				assert.NoError(t, os.Remove(alias))
				assert.NoError(t, os.Symlink(outsideFile, alias))
			}
			result := callPolicyFileTool(t, ctx, fs, name, "alias.go")
			require.False(t, result.IsError, result.Output)
			canonical, err := filepath.EvalSymlinks(target)
			require.NoError(t, err)
			assert.Equal(t, canonical, string(mustReadACPFile(t, filepath.Join(workspace, "hook-target"))))
			assert.Equal(t, "updatedformatted", string(mustReadACPFile(t, target)))
			assert.Equal(t, "secret", string(mustReadACPFile(t, outsideFile)))
		})
	}
}

func TestFilesystemPostEditSkipsFailedEdit(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{PostEdit: []latest.PostEditConfig{{Path: "*", Cmd: "exit 9"}}})
	peer.content = "no matching old text"
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameEditFile, "file.txt")
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "old text not found")
	reads, writes := peer.counts()
	assert.Equal(t, 1, reads)
	assert.Zero(t, writes)
}

func TestFilesystemPolicyAllowsUnignoredPaths(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd, ".agentsignore"), []byte("*.txt\n!allowed.txt\n"), 0o600))
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: []string{"."}})
	for _, path := range []string{"allowed.txt", filepath.Join(wd, "allowed.txt")} {
		for _, name := range []string{filesystem.ToolNameReadFile, filesystem.ToolNameReadMultipleFiles, filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile} {
			result := callPolicyFileTool(t, ctx, fs, name, path)
			assert.False(t, result.IsError, result.Output)
		}
	}
	reads, writes := peer.counts()
	assert.Equal(t, 6, reads)
	assert.Equal(t, 4, writes)
}

func TestFilesystemPolicyExpandsHomeDirectory(t *testing.T) {
	wd := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", wd)
	} else {
		t.Setenv("HOME", wd)
	}
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{AllowList: []string{"~"}})
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameReadFile, "~/file.txt")
	require.False(t, result.IsError, result.Output)
	canonical, err := filepath.EvalSymlinks(wd)
	require.NoError(t, err)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	require.Len(t, peer.reads, 1)
	assert.Equal(t, filepath.Join(canonical, "file.txt"), peer.reads[0].Path)
}
