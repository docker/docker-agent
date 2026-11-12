package acp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func TestFilesystemCapabilities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                      string
		read, write, disconnected bool
	}{
		{name: "neither"},
		{name: "read only", read: true},
		{name: "write only", write: true},
		{name: "both", read: true, write: true},
		{name: "no connection", read: true, write: true, disconnected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
			fs.agent.clientFS = acpsdk.FileSystemCapabilities{ReadTextFile: tc.read, WriteTextFile: tc.write}
			if tc.disconnected {
				fs.agent.SetAgentConnection(nil)
			}
			base, err := fs.ToolSet.Tools(ctx)
			require.NoError(t, err)
			for range 2 {
				available, err := fs.Tools(ctx)
				require.NoError(t, err)
				var want []string
				for _, original := range base {
					supported := true
					switch original.Name {
					case filesystem.ToolNameReadFile, filesystem.ToolNameReadMultipleFiles:
						supported = tc.read && !tc.disconnected
					case filesystem.ToolNameWriteFile:
						supported = tc.write && !tc.disconnected
					case filesystem.ToolNameEditFile:
						supported = tc.read && tc.write && !tc.disconnected
					}
					if supported {
						want = append(want, original.Name)
					}
				}
				var names []string
				for _, tool := range available {
					names = append(names, tool.Name)
					i := slices.IndexFunc(base, func(b tools.Tool) bool { return b.Name == tool.Name })
					assert.Equal(t, base[i].Parameters, tool.Parameters)
					assert.Equal(t, base[i].Annotations, tool.Annotations)
					if tool.Name == filesystem.ToolNameReadFile || tool.Name == filesystem.ToolNameReadMultipleFiles {
						assert.Contains(t, tool.Description, "ACP client")
						assert.NotContains(t, tool.Description, "images")
					} else {
						assert.Equal(t, base[i].Description, tool.Description)
					}
				}
				assert.Equal(t, want, names)
			}
			reads, writes := peer.counts()
			assert.Zero(t, reads)
			assert.Zero(t, writes)
		})
	}
}

func connectFilesystemPeer(t *testing.T, a *Agent) *policyFilePeer {
	t.Helper()
	reader, writer := io.Pipe()
	peer := &policyFilePeer{peer: writer, notifications: &captureWriter{}, content: "editor buffer"}
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
	return peer
}

func TestFilesystemCapabilitiesSessionDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, options string
		read, write   bool
		want          []string
	}{
		{name: "none"},
		{name: "read", read: true, want: []string{"read_file", "read_multiple_files"}},
		{name: "write", write: true, want: []string{"write_file"}},
		{name: "both", read: true, write: true, want: []string{"read_file", "read_multiple_files", "write_file", "edit_file"}},
		{name: "readonly filter", read: true, write: true, options: "        readonly: true\n", want: []string{"read_file", "read_multiple_files"}},
		{name: "tool filter", read: true, write: true, options: "        tools: [read_multiple_files, write_file]\n", want: []string{"read_multiple_files", "write_file"}},
	} {
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deferred=%t", tc.name, deferred), func(t *testing.T) {
				t.Parallel()
				wd := t.TempDir()
				cfg := "agents:\n  root:\n    model: openai/gpt-4o\n    toolsets:\n      - type: filesystem\n" + tc.options
				if deferred {
					cfg += "        defer: true\n"
				}
				a := NewAgent(config.NewBytesSource("agent.yaml", []byte(cfg)), &config.RuntimeConfig{
					Config:              config.Config{WorkingDir: t.TempDir()},
					EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}),
				}, session.NewInMemorySessionStore())
				peer := connectFilesystemPeer(t, a)
				t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
				_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{
					ProtocolVersion:    acpsdk.ProtocolVersionNumber,
					ClientCapabilities: acpsdk.ClientCapabilities{Fs: acpsdk.FileSystemCapabilities{ReadTextFile: tc.read, WriteTextFile: tc.write}},
				})
				require.NoError(t, err)
				created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
				require.NoError(t, err)
				for turn := range 2 {
					if turn == 1 {
						_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
						require.NoError(t, err)
						_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
						require.NoError(t, err)
					}
					s := a.sessions[string(created.SessionId)]
					available, err := s.rt.CurrentAgentTools(withSessionID(t.Context(), s.id))
					require.NoError(t, err)
					var names []string
					if deferred {
						result := workspaceTool(t, s, "search_tool", `{"query":""}`)
						var entries []struct {
							Name string `json:"name"`
						}
						_, data, found := strings.Cut(result.Output, "\n")
						require.True(t, found)
						require.NoError(t, json.Unmarshal([]byte(data), &entries))
						for _, entry := range entries {
							names = append(names, entry.Name)
						}
					} else {
						for _, tool := range available {
							names = append(names, tool.Name)
						}
					}
					for _, name := range []string{"read_file", "read_multiple_files", "write_file", "edit_file"} {
						assert.Equal(t, slices.Contains(tc.want, name), slices.Contains(names, name), name)
					}
					if slices.Contains(tc.want, "read_multiple_files") {
						if deferred {
							workspaceTool(t, s, "add_tool", `{"name":"read_multiple_files"}`)
						}
						result := workspaceTool(t, s, "read_multiple_files", `{"paths":["not-on-disk.txt"]}`)
						assert.Contains(t, result.Output, "editor buffer")
					}
				}
				reads, writes := peer.counts()
				expected := 0
				if slices.Contains(tc.want, "read_multiple_files") {
					expected = 2
				}
				assert.Equal(t, expected, reads, "listing must not read file contents")
				assert.Zero(t, writes)
				root, err := filepath.EvalSymlinks(wd)
				require.NoError(t, err)
				for _, request := range peer.reads {
					assert.Equal(t, created.SessionId, request.SessionId)
					assert.Equal(t, filepath.Join(root, "not-on-disk.txt"), request.Path)
				}
			})
		}
	}
}

func TestFilesystemUnsupportedHandlersDoNotFallback(t *testing.T) {
	t.Parallel()
	for _, caps := range []acpsdk.FileSystemCapabilities{{}, {ReadTextFile: true}, {WriteTextFile: true}} {
		fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
		fs.agent.clientFS = caps
		path := filepath.Join(fs.workingDir, "file.txt")
		require.NoError(t, os.WriteFile(path, []byte("original on disk"), 0o600))
		for _, tc := range []struct {
			enabled bool
			handler tools.ToolHandler
			args    string
		}{
			{caps.ReadTextFile, fs.handleReadFile, `{"path":"file.txt"}`},
			{caps.ReadTextFile, fs.handleReadMultipleFiles, `{"paths":["file.txt"]}`},
			{caps.WriteTextFile, fs.handleWriteFile, `{"path":"file.txt","content":"changed"}`},
			{false, fs.handleEditFile, `{"path":"file.txt","edits":[{"oldText":"original","newText":"changed"}]}`},
		} {
			if tc.enabled {
				continue
			}
			result, err := tc.handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: tc.args}}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Output, "does not support")
			assert.Equal(t, "original on disk", string(mustReadACPFile(t, path)))
		}
		reads, writes := peer.counts()
		assert.Zero(t, reads)
		assert.Zero(t, writes)
	}
}

func TestFilesystemIndependentReadAndWriteCapabilities(t *testing.T) {
	t.Parallel()
	for _, read := range []bool{false, true} {
		wd := t.TempDir()
		fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{})
		fs.agent.clientFS = acpsdk.FileSystemCapabilities{ReadTextFile: read, WriteTextFile: !read}
		require.NoError(t, os.WriteFile(filepath.Join(wd, "file.txt"), []byte("original on disk"), 0o600))
		name := filesystem.ToolNameWriteFile
		if read {
			name = filesystem.ToolNameReadFile
		}
		result := callPolicyFileTool(t, ctx, fs, name, "file.txt")
		require.False(t, result.IsError, result.Output)
		reads, writes := peer.counts()
		if read {
			assert.Equal(t, "original", result.Output)
			assert.Equal(t, 1, reads)
			assert.Zero(t, writes)
		} else {
			assert.Zero(t, reads, "writes must not introduce a pre-read")
			assert.Equal(t, 1, writes)
		}
		assert.Equal(t, "original on disk", string(mustReadACPFile(t, filepath.Join(wd, "file.txt"))))
	}
}
