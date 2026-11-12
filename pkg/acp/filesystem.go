package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

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

// FilesystemToolset routes supported text operations through the ACP client.
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

	canRead := t.agent != nil && t.agent.supportsClientReadTextFile()
	canWrite := t.agent != nil && t.agent.supportsClientWriteTextFile()
	available := baseTools[:0]
	for _, tool := range baseTools {
		switch tool.Name {
		case filesystem.ToolNameReadFile:
			if !canRead {
				continue
			}
			tool.Handler = t.handleReadFile
			tool.Description = "Read a text file through the ACP client, including unsaved editor content. By default the complete file is returned; optional line (1-based start line) and limit (maximum number of lines) select a line range."
		case filesystem.ToolNameReadMultipleFiles:
			if !canRead {
				continue
			}
			tool.Handler = t.handleReadMultipleFiles
			tool.Description = "Read multiple text files through the ACP client, including unsaved editor content. Results are returned in input order, with per-file errors."
		case filesystem.ToolNameWriteFile:
			if !canWrite {
				continue
			}
			tool.Handler = t.handleWriteFile
		case filesystem.ToolNameEditFile:
			if !canRead || !canWrite {
				continue
			}
			tool.Handler = t.handleEditFile
		}
		available = append(available, tool)
	}

	return available, nil
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

	checkedPath, err := t.ResolveAndCheckPath(userPath)
	if err != nil {
		return "", err
	}
	workingDir, roots := acpSess.pathRoots(t.workingDir)
	return resolvePathInRoots(checkedPath, workingDir, roots)
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
	// A dangling symlink is not a missing path: the client might create its target.
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("cannot resolve symlink %q: %w", path, err)
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
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
	if err := tools.UnmarshalToolArguments(ctx, toolCall, &args); err != nil {
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
		Meta:      traceMeta(ctx, nil),
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

func (t *FilesystemToolset) handleReadMultipleFiles(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args filesystem.ReadMultipleFilesArgs
	if err := tools.UnmarshalToolArguments(ctx, toolCall, &args); err != nil {
		return nil, fmt.Errorf("failed to parse arguments: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionID, ok := getSessionID(ctx)
	if !ok {
		return tools.ResultError("Error: session ID not found in context"), nil
	}
	if !t.agent.supportsClientReadTextFile() {
		return tools.ResultError("Error: ACP client does not support reading files"), nil
	}

	type pathContent struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	var contents []pathContent
	var meta filesystem.ReadMultipleFilesMeta
	for _, path := range args.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resolvedPath, err := t.resolvePathForSession(ctx, path)
		var response acp.ReadTextFileResponse
		if err == nil {
			// The SDK can send a request even when its context is already canceled.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			response, err = t.agent.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
				Meta:      traceMeta(ctx, nil),
				SessionId: acp.SessionId(sessionID),
				Path:      resolvedPath,
			})
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := filesystem.ReadFileMeta{Path: path}
		content := response.Content
		if err != nil {
			entry.Error = err.Error()
			content = entry.Error
		} else {
			entry.LineCount = strings.Count(content, "\n") + 1
		}
		contents = append(contents, pathContent{Path: path, Content: content})
		meta.Files = append(meta.Files, entry)
	}

	var output string
	if args.JSON {
		encoded, err := json.Marshal(contents)
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Error formatting JSON: %s", err)), nil
		}
		output = string(encoded)
	} else {
		var result strings.Builder
		for _, content := range contents {
			fmt.Fprintf(&result, "=== %s ===\n%s\n\n", content.Path, content.Content)
		}
		output = result.String()
	}
	return &tools.ToolCallResult{Output: output, Meta: meta}, nil
}

// fileChange is presentation-only: never serialize file contents as generic tool metadata.
type fileChange struct {
	path    string
	oldText *string
	newText string
}

const maxFileDiffBytes = 1 << 20

func capturedFileChange(path, before, after string) *fileChange {
	// Include JSON escaping in the cap before allocating the encoded snapshot.
	remaining := maxFileDiffBytes - len(`{"type":"diff","path":"","oldText":"","newText":""}`)
	for _, part := range []string{path, before, after} {
		var ok bool
		remaining, ok = consumeJSONStringBudget(part, remaining)
		if !ok {
			return nil
		}
	}
	change := &fileChange{path: path, oldText: &before, newText: after}
	encoded, err := json.Marshal(acp.ToolCallContentDiff{Type: "diff", Path: path, OldText: &before, NewText: after})
	if err != nil || len(encoded) > maxFileDiffBytes {
		return nil
	}
	return change
}

func consumeJSONStringBudget(text string, remaining int) (int, bool) {
	if len(text) > remaining {
		return 0, false
	}
	for i := 0; i < len(text); {
		cost, width := 1, 1
		switch c := text[i]; {
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			cost = 2
		case c < 0x20 || c == '<' || c == '>' || c == '&':
			cost = 6
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(text[i:])
			width, cost = size, size
			if r == utf8.RuneError && size == 1 {
				return 0, false
			}
			if r == '\u2028' || r == '\u2029' {
				cost = 6
			}
		}
		remaining -= cost
		if remaining < 0 {
			return 0, false
		}
		i += width
	}
	return remaining, true
}

func (t *FilesystemToolset) handleWriteFile(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args filesystem.WriteFileArgs
	if err := tools.UnmarshalToolArguments(ctx, toolCall, &args); err != nil {
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
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
		Content:   args.Content,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	if err := t.ExecutePostEditCommands(ctx, resolvedPath); err != nil {
		return tools.ResultError(fmt.Sprintf("File written successfully but post-edit command failed: %s", err)), nil
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
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error reading file: %s", err)), nil
	}

	readPath := resolvedPath
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

	resolvedPath, err = t.resolvePathForSession(ctx, resolvedPath)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error: %s", err)), nil
	}
	_, err = t.agent.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
		Content:   modifiedContent,
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	if err := t.ExecutePostEditCommands(ctx, resolvedPath); err != nil {
		return tools.ResultError(fmt.Sprintf("File edited successfully but post-edit command failed: %s", err)), nil
	}
	result := tools.ResultSuccess("File edited successfully")
	if !t.HasPostEditCommands() && readPath == resolvedPath {
		if change := capturedFileChange(resolvedPath, resp.Content, modifiedContent); change != nil {
			result.Meta = change
		}
	}
	return result, nil
}
