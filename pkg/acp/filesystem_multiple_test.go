package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func multipleFileCall(t *testing.T, paths []string, asJSON bool) tools.ToolCall {
	t.Helper()
	data, err := json.Marshal(filesystem.ReadMultipleFilesArgs{Paths: paths, JSON: asJSON})
	require.NoError(t, err)
	return tools.ToolCall{Function: tools.FunctionCall{Name: filesystem.ToolNameReadMultipleFiles, Arguments: string(data)}}
}

func TestReadMultipleFilesClientBuffers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		for _, asJSON := range []bool{false, true} {
			wd := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(wd, "stale.txt"), []byte("stale disk data"), 0o600))
			fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{DenyList: []string{"blocked"}})
			peer.readResponse = func(req acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, *acpsdk.RequestError) {
				switch filepath.Base(req.Path) {
				case "stale.txt":
					return acpsdk.ReadTextFileResponse{Content: "unsaved\neditor\n"}, nil
				case "new.txt":
					return acpsdk.ReadTextFileResponse{Content: "buffer only"}, nil
				case "empty.txt":
					return acpsdk.ReadTextFileResponse{}, nil
				default:
					return acpsdk.ReadTextFileResponse{}, &acpsdk.RequestError{Code: -32002, Message: "not found"}
				}
			}
			paths := []string{"stale.txt", "blocked/file.txt", "missing.txt", "new.txt", "stale.txt", "empty.txt"}
			result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, paths, asJSON), tools.NopRuntime{})
			require.NoError(t, err)
			require.False(t, result.IsError)
			meta, ok := result.Meta.(filesystem.ReadMultipleFilesMeta)
			require.True(t, ok)
			require.Len(t, meta.Files, len(paths))
			var contents []struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if asJSON {
				require.NoError(t, json.Unmarshal([]byte(result.Output), &contents))
			}
			var text strings.Builder
			for i, entry := range meta.Files {
				assert.Equal(t, paths[i], entry.Path)
				content := entry.Error
				switch i {
				case 0, 4:
					content = "unsaved\neditor\n"
					assert.Equal(t, 3, entry.LineCount)
				case 3:
					content = "buffer only"
					assert.Equal(t, 1, entry.LineCount)
				case 5:
					assert.Empty(t, entry.Error)
					assert.Equal(t, 1, entry.LineCount)
				default:
					assert.NotEmpty(t, entry.Error)
					assert.Zero(t, entry.LineCount)
				}
				if asJSON {
					assert.Equal(t, paths[i], contents[i].Path)
					assert.Equal(t, content, contents[i].Content)
				}
				text.WriteString("=== " + paths[i] + " ===\n" + content + "\n\n")
			}
			if !asJSON {
				assert.Equal(t, text.String(), result.Output)
			}
			assert.NotContains(t, result.Output, "stale disk data")
			assert.Contains(t, meta.Files[1].Error, "denied directory")
			assert.Contains(t, meta.Files[2].Error, "not found")
			reads, writes := peer.counts()
			assert.Equal(t, 5, reads)
			assert.Zero(t, writes)
			for i, req := range peer.reads {
				assert.Equal(t, []string{"stale.txt", "missing.txt", "new.txt", "stale.txt", "empty.txt"}[i], filepath.Base(req.Path))
				assert.Equal(t, acpsdk.SessionId("policy-session"), req.SessionId)
				assert.True(t, filepath.IsAbs(req.Path))
				assert.Nil(t, req.Line)
				assert.Nil(t, req.Limit)
			}
			assert.NoFileExists(t, filepath.Join(wd, "new.txt"))
		}
	})
}

func TestReadMultipleFilesEmptyAndFailures(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
		peer.failRead = true
		for _, asJSON := range []bool{false, true} {
			result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, nil, asJSON), tools.NopRuntime{})
			require.NoError(t, err)
			if asJSON {
				assert.Equal(t, "null", result.Output)
			} else {
				assert.Empty(t, result.Output)
			}
			assert.Equal(t, filesystem.ReadMultipleFilesMeta{}, result.Meta)
			assert.False(t, result.IsError)
		}
		reads, _ := peer.counts()
		assert.Zero(t, reads)
		result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, []string{"a", "b"}, true), tools.NopRuntime{})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		meta := result.Meta.(filesystem.ReadMultipleFilesMeta)
		require.Len(t, meta.Files, 2)
		for _, entry := range meta.Files {
			assert.Contains(t, entry.Error, "read failed")
			assert.Zero(t, entry.LineCount)
		}
	})
}

func TestReadMultipleFilesCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelAt := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(cancelAt), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fs, baseCtx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
				ctx, cancel := context.WithCancel(baseCtx)
				defer cancel()
				if cancelAt == 0 {
					cancel()
				}
				count := 0
				peer.readResponse = func(acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, *acpsdk.RequestError) {
					count++
					if count == cancelAt {
						cancel()
					}
					return acpsdk.ReadTextFileResponse{Content: "partial"}, nil
				}
				result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, []string{"a", "b"}, false), tools.NopRuntime{})
				require.ErrorIs(t, err, context.Canceled)
				assert.Nil(t, result)
				reads, writes := peer.counts()
				assert.Equal(t, cancelAt, reads)
				assert.Zero(t, writes)
			})
		})
	}
}

func TestReadMultipleFilesValidatesArgumentsAndSession(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
		_, err := fs.handleReadMultipleFiles(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"paths":42}`}}, tools.NopRuntime{})
		require.ErrorContains(t, err, "failed to parse arguments")
		for _, ctx := range []context.Context{t.Context(), withSessionID(t.Context(), "unknown-session")} {
			result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, []string{"a"}, false), tools.NopRuntime{})
			require.NoError(t, err)
			assert.Contains(t, result.Output, "not found")
		}
		reads, writes := peer.counts()
		assert.Zero(t, reads)
		assert.Zero(t, writes)
	})
}

func TestReadMultipleFilesRechecksAdditionalRoots(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		wd, extra := t.TempDir(), t.TempDir()
		fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{}, extra)
		peer.onRead = func() {
			s := fs.agent.sessions["policy-session"]
			s.mu.Lock()
			defer s.mu.Unlock()
			s.additionalDirs = nil
		}
		result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, []string{"a", filepath.Join(extra, "b"), "c"}, false), tools.NopRuntime{})
		require.NoError(t, err)
		meta := result.Meta.(filesystem.ReadMultipleFilesMeta)
		require.Len(t, meta.Files, 3)
		assert.Empty(t, meta.Files[0].Error)
		assert.Contains(t, meta.Files[1].Error, "escapes the working directory")
		assert.Empty(t, meta.Files[2].Error)
		reads, _ := peer.counts()
		assert.Equal(t, 2, reads)
	})
}

func TestReadMultipleFilesClientFailureNeverReadsDisk(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		wd := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(wd, "file.txt"), []byte("private disk content"), 0o600))
		fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{})
		peer.failRead = true
		result, err := fs.handleReadMultipleFiles(ctx, multipleFileCall(t, []string{"file.txt"}, false), tools.NopRuntime{})
		require.NoError(t, err)
		assert.NotContains(t, result.Output, "private disk content")
		assert.Contains(t, result.Meta.(filesystem.ReadMultipleFilesMeta).Files[0].Error, "read failed")
		reads, writes := peer.counts()
		assert.Equal(t, 1, reads)
		assert.Zero(t, writes)
	})
}

func TestReadMultipleFilesSessionIsolation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		fs, ctx, peer := newPolicyFileFixture(t, first, latest.Toolset{})
		fs.agent.sessions["second"] = &Session{id: "second", workingDir: second}
		other := NewFilesystemToolset(fs.agent, second)
		t.Cleanup(func() { require.NoError(t, other.Close()) })
		peer.readResponse = func(req acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, *acpsdk.RequestError) {
			return acpsdk.ReadTextFileResponse{Content: string(req.SessionId)}, nil
		}
		for i, toolset := range []*FilesystemToolset{fs, other} {
			callCtx, sid := ctx, "policy-session"
			if i == 1 {
				sid = "second"
				callCtx = withSessionID(t.Context(), sid)
			}
			result, err := toolset.handleReadMultipleFiles(callCtx, multipleFileCall(t, []string{"buffer.txt"}, true), tools.NopRuntime{})
			require.NoError(t, err)
			assert.Contains(t, result.Output, sid)
			root, err := filepath.EvalSymlinks(toolset.workingDir)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(root, "buffer.txt"), peer.reads[i].Path)
			assert.Equal(t, acpsdk.SessionId(sid), peer.reads[i].SessionId)
		}
	})
}
