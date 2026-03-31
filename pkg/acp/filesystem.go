package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
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

// FilesystemToolset wraps a standard FilesystemTool and overrides read_file, write_file,
// and edit_file to use the ACP connection for file operations
type FilesystemToolset struct {
	*builtin.FilesystemTool

	agent      *Agent
	workingDir string
}

var _ tools.ToolSet = (*FilesystemToolset)(nil)

// NewFilesystemToolset creates a new ACP-specific filesystem toolset
func NewFilesystemToolset(agent *Agent, workingDir string, opts ...builtin.FileSystemOpt) *FilesystemToolset {
	return &FilesystemToolset{
		FilesystemTool: builtin.NewFilesystemTool(workingDir, opts...),
		agent:          agent,
		workingDir:     workingDir,
	}
}

// Tools returns the tool definitions with ACP-specific overrides
func (t *FilesystemToolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	baseTools, err := t.FilesystemTool.Tools(ctx)
	if err != nil {
		return nil, err
	}

	for i := range baseTools {
		switch baseTools[i].Name {
		case builtin.ToolNameReadFile:
			baseTools[i].Handler = t.handleReadFile
		case builtin.ToolNameWriteFile:
			baseTools[i].Handler = t.handleWriteFile
		case builtin.ToolNameEditFile:
			baseTools[i].Handler = t.handleEditFile
		}
	}

	return baseTools, nil
}

// resolvePath resolves a user-supplied path relative to the working directory
// and validates that the resulting path does not escape the working directory.
func (t *FilesystemToolset) resolvePath(userPath string) (string, error) {
	resolved := filepath.Clean(filepath.Join(t.workingDir, userPath))
	absWorkingDir, err := filepath.Abs(t.workingDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve working directory: %w", err)
	}
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}
	// Normalize paths for comparison to prevent bypasses on case-insensitive
	// filesystems (macOS, Windows) where differing case could defeat the check.
	normResolved := normalizePathForComparison(absResolved)
	normWorkingDir := normalizePathForComparison(absWorkingDir)
	if !strings.HasPrefix(normResolved, normWorkingDir+string(filepath.Separator)) && normResolved != normWorkingDir {
		return "", fmt.Errorf("path %q escapes the working directory", userPath)
	}
	return absResolved, nil
}

func (t *FilesystemToolset) handleReadFile(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var args builtin.ReadFileArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}

	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}

	resolvedPath, err := t.resolvePath(args.Path)
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

	return tools.ResultSuccess(resp.Content), nil
}

func (t *FilesystemToolset) handleWriteFile(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var args builtin.WriteFileArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}

	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}

	resolvedPath, err := t.resolvePath(args.Path)
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

func (t *FilesystemToolset) handleEditFile(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var args builtin.EditFileArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}

	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}

	resolvedPath, err := t.resolvePath(args.Path)
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
		match, err := builtin.FindMatch(modifiedContent, edit.OldText)
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Edit %d failed: %s", i+1, err)), nil
		}
		modifiedContent = strings.Replace(modifiedContent, match.SearchText, edit.NewText, 1)
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
