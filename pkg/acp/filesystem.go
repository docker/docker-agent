package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

type contextKey string

const sessionIDKey contextKey = "acp_session_id"

// withSessionID adds the session ID to the context
func withSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey, sessionID)
}

// getSessionID retrieves the session ID from the context
func getSessionID(ctx context.Context) (string, bool) {
	sid, ok := ctx.Value(sessionIDKey).(string)
	return sid, ok
}

// FilesystemToolset wraps a standard Tool and overrides read_file, write_file,
// and edit_file to use the ACP connection for file operations
type FilesystemToolset struct {
	*filesystem.ToolSet

	agent      *Agent
	workingDir string
}

var _ tools.ToolSet = (*FilesystemToolset)(nil)

// NewFilesystemToolset creates a new ACP-specific filesystem toolset
func NewFilesystemToolset(agent *Agent, workingDir string, opts ...filesystem.Opt) *FilesystemToolset {
	return &FilesystemToolset{
		ToolSet:    filesystem.New(workingDir, opts...),
		agent:      agent,
		workingDir: workingDir,
	}
}

// Tools returns the tool definitions with ACP-specific overrides
func (t *FilesystemToolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	baseTools, err := t.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}

	for i := range baseTools {
		switch baseTools[i].Name {
		case filesystem.ToolNameReadFile:
			baseTools[i].Handler = t.handleReadFile
		case filesystem.ToolNameWriteFile:
			baseTools[i].Handler = t.handleWriteFile
		case filesystem.ToolNameEditFile:
			baseTools[i].Handler = t.handleEditFile
		}
	}

	return baseTools, nil
}

// resolvePath resolves a user-supplied path relative to the working directory
// and validates that the resulting path does not escape the working directory.
// It follows symlinks to prevent a symlink inside the working directory from
// pointing outside it.
func (t *FilesystemToolset) resolvePath(userPath string) (string, error) {
	return resolvePathInRoots(userPath, t.workingDir, []string{t.workingDir})
}

func (t *FilesystemToolset) resolvePathForSession(ctx context.Context, userPath string) (string, error) {
	sessionID, ok := getSessionID(ctx)
	if !ok {
		return "", errors.New("session ID not found in context")
	}
	if t.agent == nil {
		return "", errors.New("ACP agent not configured")
	}

	t.agent.mu.Lock()
	acpSess := t.agent.sessions[sessionID]
	t.agent.mu.Unlock()
	if acpSess == nil {
		return "", fmt.Errorf("session %s not found", sessionID)
	}

	workingDir, roots := acpSess.pathRoots(t.workingDir)
	return resolvePathInRoots(userPath, workingDir, roots)
}

func resolvePathInRoots(userPath, workingDir string, roots []string) (string, error) {
	if workingDir == "" {
		return "", errors.New("working directory is not configured")
	}

	var resolved string
	if filepath.IsAbs(userPath) {
		resolved = filepath.Clean(userPath)
	} else {
		resolved = filepath.Clean(filepath.Join(workingDir, userPath))
	}

	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}

	// Resolve symlinks. For paths that don't exist yet (e.g. a new file
	// being created), walk up to the nearest existing ancestor, resolve
	// symlinks on that, then re-append the remaining components.
	realResolved, err := evalSymlinksAllowMissing(absResolved)
	if err != nil {
		return "", fmt.Errorf("failed to evaluate symlinks: %w", err)
	}

	for _, root := range roots {
		if root == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("failed to resolve working directory: %w", err)
		}
		realRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			return "", fmt.Errorf("failed to evaluate symlinks for working directory: %w", err)
		}
		if pathWithinRoot(realResolved, realRoot) {
			return realResolved, nil
		}
	}

	return "", fmt.Errorf("path %q escapes the working directory", userPath)
}

func pathWithinRoot(path, root string) bool {
	normPath := normalizePathForComparison(filepath.Clean(path))
	normRoot := normalizePathForComparison(filepath.Clean(root))
	if normPath == normRoot {
		return true
	}
	rel, err := filepath.Rel(normRoot, normPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// evalSymlinksAllowMissing resolves symlinks for a path that may not fully
// exist. It walks up from the given path until it finds an existing ancestor,
// resolves symlinks on that ancestor, then re-appends the missing tail.
func evalSymlinksAllowMissing(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	// Walk up to find the nearest existing ancestor.
	parent := filepath.Dir(path)
	if parent == path {
		// Reached filesystem root without finding an existing path.
		return path, nil
	}
	realParent, err := evalSymlinksAllowMissing(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

func (t *FilesystemToolset) handleReadFile(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args filesystem.ReadFileArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}
	if err := filesystem.ValidateReadFileRange(args.Line, args.Limit); err != nil {
		return tools.ResultError(fmt.Sprintf("Error: %s", err)), nil
	}

	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}
	if !t.agent.supportsClientReadTextFile() {
		return tools.ResultError("Error: ACP client does not support reading files"), nil
	}

	resolvedPath, err := t.resolvePathForSession(ctx, args.Path)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error: %s", err)), nil
	}

	resp, err := t.agent.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
		Line:      args.Line,
		Limit:     args.Limit,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error reading file: %s", err)), nil
	}

	return tools.ResultSuccess(resp.Content), nil
}

func (t *FilesystemToolset) handleWriteFile(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args filesystem.WriteFileArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}

	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}
	if !t.agent.supportsClientWriteTextFile() {
		return tools.ResultError("Error: ACP client does not support writing files"), nil
	}

	resolvedPath, err := t.resolvePathForSession(ctx, args.Path)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error: %s", err)), nil
	}

	_, err = t.agent.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
		Content:   args.Content,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	return tools.ResultSuccess("File written successfully"), nil
}

func (t *FilesystemToolset) handleEditFile(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	data := toolCall.Function.Arguments
	if data == "" {
		data = "{}"
	}
	args, err := filesystem.ParseEditFileArgs([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}

	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}
	if !t.agent.supportsClientReadTextFile() || !t.agent.supportsClientWriteTextFile() {
		return tools.ResultError("Error: ACP client does not support editing files"), nil
	}

	resolvedPath, err := t.resolvePathForSession(ctx, args.Path)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error: %s", err)), nil
	}

	resp, err := t.agent.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error reading file: %s", err)), nil
	}

	modifiedContent := resp.Content

	for i, edit := range args.Edits {
		// strings.Contains always matches "" and strings.Replace would insert
		// newText at offset 0, silently prepending to the file. Mirrors the
		// guard in the built-in filesystem toolset, which serves the same
		// edit_file tool name and schema over a different transport.
		if edit.OldText == "" {
			return tools.ResultError(fmt.Sprintf("Edit %d failed: oldText must not be empty", i+1)), nil
		}
		if !strings.Contains(modifiedContent, edit.OldText) {
			return tools.ResultError(fmt.Sprintf("Edit %d failed: old text not found", i+1)), nil
		}
		modifiedContent = strings.Replace(modifiedContent, edit.OldText, edit.NewText, 1)
	}

	_, err = t.agent.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
		Content:   modifiedContent,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	return tools.ResultSuccess("File edited successfully"), nil
}
