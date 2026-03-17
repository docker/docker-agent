package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameReadFile           = "read_file"
	ToolNameReadMultipleFiles  = "read_multiple_files"
	ToolNameEditFile           = "edit_file"
	ToolNameWriteFile          = "write_file"
	ToolNameDirectoryTree      = "directory_tree"
	ToolNameListDirectory      = "list_directory"
	ToolNameSearchFilesContent = "search_files_content"
	ToolNameMkdir              = "create_directory"
	ToolNameRmdir              = "remove_directory"
)

// PostEditConfig represents a post-edit command configuration
type PostEditConfig struct {
	Path string // File path pattern (glob-style)
	Cmd  string // Command to execute (with $path placeholder)
}

type FilesystemTool struct {
	workingDir       string
	postEditCommands []PostEditConfig
	ignoreVCS        bool
	repoMatcher      *fsx.VCSMatcher
	repoMatcherOnce  sync.Once
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*FilesystemTool)(nil)
	_ tools.Instructable = (*FilesystemTool)(nil)
)

// allowAllPaths is a no-op path filter that permits every path.
func allowAllPaths(_ string) error { return nil }

type FileSystemOpt func(*FilesystemTool)

func WithPostEditCommands(postEditCommands []PostEditConfig) FileSystemOpt {
	return func(t *FilesystemTool) {
		t.postEditCommands = postEditCommands
	}
}

func WithIgnoreVCS(ignoreVCS bool) FileSystemOpt {
	return func(t *FilesystemTool) {
		t.ignoreVCS = ignoreVCS
	}
}

func NewFilesystemTool(workingDir string, opts ...FileSystemOpt) *FilesystemTool {
	t := &FilesystemTool{
		workingDir: workingDir,
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

func (t *FilesystemTool) Instructions() string {
	return `## Filesystem Tools

- Relative paths resolve from the working directory; absolute paths and ".." work as expected
- Prefer read_multiple_files over sequential read_file calls
- Use search_files_content to locate code or text across files
- Use exclude patterns in searches and max_depth in directory_tree to limit output`
}

type DirectoryTreeArgs struct {
	Path string `json:"path" jsonschema:"The directory path to traverse (relative to working directory)"`
}

type WriteFileArgs struct {
	Path    string `json:"path" jsonschema:"The file path to write"`
	Content string `json:"content" jsonschema:"The content to write to the file"`
}

type ReadMultipleFilesArgs struct {
	Paths []string `json:"paths" jsonschema:"Array of file paths to read"`
	JSON  bool     `json:"json,omitempty" jsonschema:"Whether to return the result as JSON"`
}

type ReadMultipleFilesMeta struct {
	Files []ReadFileMeta `json:"files"`
}

type SearchFilesContentArgs struct {
	Path            string   `json:"path" jsonschema:"The starting directory path"`
	Query           string   `json:"query" jsonschema:"The text or regex pattern to search for"`
	IsRegex         bool     `json:"is_regex,omitempty" jsonschema:"If true, treat query as regex; otherwise literal text"`
	ExcludePatterns []string `json:"excludePatterns,omitempty" jsonschema:"Patterns to exclude from search"`
}

type SearchFilesContentMeta struct {
	MatchCount int `json:"matchCount"`
	FileCount  int `json:"fileCount"`
}

type ListDirectoryArgs struct {
	Path string `json:"path" jsonschema:"The directory path to list"`
}

type CreateDirectoryArgs struct {
	Paths []string `json:"paths" jsonschema:"Array of directory paths to create"`
}

type RemoveDirectoryArgs struct {
	Paths []string `json:"paths" jsonschema:"Array of directory paths to remove"`
}

type ListDirectoryMeta struct {
	Files     []string `json:"files"`
	Dirs      []string `json:"dirs"`
	Truncated bool     `json:"truncated"`
}

type DirectoryTreeMeta struct {
	FileCount int  `json:"fileCount"`
	DirCount  int  `json:"dirCount"`
	Truncated bool `json:"truncated"`
}

type ReadFileArgs struct {
	Path string `json:"path" jsonschema:"The file path to read"`
}

type ReadFileMeta struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	LineCount int    `json:"lineCount"`
	Error     string `json:"error,omitempty"`
}

type Edit struct {
	OldText string `json:"oldText" jsonschema:"The exact text to replace"`
	NewText string `json:"newText" jsonschema:"The replacement text"`
}

type EditFileArgs struct {
	Path  string `json:"path" jsonschema:"The file path to edit"`
	Edits []Edit `json:"edits" jsonschema:"Array of edit operations"`
}

// UnmarshalJSON handles LLM-generated arguments where "edits" may be
// a JSON string instead of a JSON array (double-serialized).
func (a *EditFileArgs) UnmarshalJSON(data []byte) error {
	// First, try standard unmarshaling.
	type Alias EditFileArgs
	var std Alias
	if err := json.Unmarshal(data, &std); err == nil {
		*a = EditFileArgs(std)
		return nil
	}

	// If that failed, the "edits" field may be a JSON string.
	// Parse it as a raw message and attempt double-deserialization.
	var raw struct {
		Path  string          `json:"path"`
		Edits json.RawMessage `json:"edits"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("failed to parse edit_file arguments: %w", err)
	}

	a.Path = raw.Path

	// Try parsing edits as an array first (normal case).
	if err := json.Unmarshal(raw.Edits, &a.Edits); err == nil {
		return nil
	}

	// Try unwrapping a double-serialized JSON string.
	var editsStr string
	if err := json.Unmarshal(raw.Edits, &editsStr); err != nil {
		return fmt.Errorf("edits field is neither an array nor a JSON string: %w", err)
	}
	if err := json.Unmarshal([]byte(editsStr), &a.Edits); err != nil {
		return fmt.Errorf("failed to parse double-serialized edits string: %w", err)
	}

	return nil
}

func (t *FilesystemTool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameDirectoryTree,
			Category:    "filesystem",
			Description: "Get a recursive tree view of files and directories as a JSON structure.",
			Parameters:  tools.MustSchemaFor[DirectoryTreeArgs](),
			// Manually define the schema here because
			// tools.MustSchemaFor(reflect.TypeFor[*TreeNode]()) doesn't support recursive types.
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The name of the node",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "The type of the node (file or directory)",
					},
					"children": map[string]any{
						"type":        "array",
						"description": "Optional list of child nodes",
						"items": map[string]any{
							"$ref": "#",
						},
					},
				},
				"required":             []string{"name", "type"},
				"additionalProperties": false,
			},
			Handler: tools.NewHandler(t.handleDirectoryTree),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Directory Tree",
			},
		},
		{
			Name:         ToolNameEditFile,
			Category:     "filesystem",
			Description:  "Make line-based edits to a text file. Each edit replaces exact line sequences with new content.",
			Parameters:   tools.MustSchemaFor[EditFileArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleEditFile),
			Annotations: tools.ToolAnnotations{
				Title: "Edit",
			},
			AddDescriptionParameter: true,
		},
		{
			Name:         ToolNameListDirectory,
			Category:     "filesystem",
			Description:  "Get a detailed listing of all files and directories in a specified path.",
			Parameters:   tools.MustSchemaFor[ListDirectoryArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleListDirectory),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "List Directory",
			},
			AddDescriptionParameter: true,
		},
		{
			Name:         ToolNameReadFile,
			Category:     "filesystem",
			Description:  "Read the complete contents of a file from the file system. Supports text files and images (jpg, png, gif, webp). Images are returned as image content that you can view directly.",
			Parameters:   tools.MustSchemaFor[ReadFileArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleReadFile),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Read",
			},
		},
		{
			Name:        ToolNameReadMultipleFiles,
			Category:    "filesystem",
			Description: "Read the contents of multiple files simultaneously.",
			Parameters:  tools.MustSchemaFor[ReadMultipleFilesArgs](),
			// TODO(dga): depends on the json param
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleReadMultipleFiles),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Read Multiple Files",
			},
		},
		{
			Name:         ToolNameSearchFilesContent,
			Category:     "filesystem",
			Description:  "Searches for text or regex patterns in the content of files matching a GLOB pattern.",
			Parameters:   tools.MustSchemaFor[SearchFilesContentArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleSearchFilesContent),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Search Files Content",
			},
			AddDescriptionParameter: true,
		},
		{
			Name:         ToolNameWriteFile,
			Category:     "filesystem",
			Description:  "Create a new file or completely overwrite an existing file with new content.",
			Parameters:   tools.MustSchemaFor[WriteFileArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleWriteFile),
			Annotations: tools.ToolAnnotations{
				Title: "Write",
			},
			AddDescriptionParameter: true,
		},
		{
			Name:         ToolNameMkdir,
			Category:     "filesystem",
			Description:  "Create one or more new directories or nested directory structures.",
			Parameters:   tools.MustSchemaFor[CreateDirectoryArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleCreateDirectory),
			Annotations: tools.ToolAnnotations{
				Title: "Create Directory",
			},
		},
		{
			Name:         ToolNameRmdir,
			Category:     "filesystem",
			Description:  "Remove one or more empty directories.",
			Parameters:   tools.MustSchemaFor[RemoveDirectoryArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleRemoveDirectory),
			Annotations: tools.ToolAnnotations{
				Title: "Remove Directory",
			},
		},
	}, nil
}

// executePostEditCommands executes any matching post-edit commands for the given file path
func (t *FilesystemTool) executePostEditCommands(ctx context.Context, filePath string) error {
	if len(t.postEditCommands) == 0 {
		return nil
	}

	for _, postEdit := range t.postEditCommands {
		matched, err := filepath.Match(postEdit.Path, filepath.Base(filePath))
		if err != nil {
			slog.WarnContext(ctx, "Invalid post-edit pattern", "pattern", postEdit.Path, "error", err)
			continue
		}
		if !matched {
			continue
		}

		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", postEdit.Cmd)
		cmd.Env = cmd.Environ()
		cmd.Env = append(cmd.Env, "path="+filePath)

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("post-edit command failed for %s: %w", filePath, err)
		}
	}
	return nil
}

// resolvePath resolves a path relative to the working directory.
// Relative paths (including ".") are joined with the working directory.
// Absolute paths and paths starting with ".." are used as-is.
func (t *FilesystemTool) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}

	return filepath.Clean(filepath.Join(t.workingDir, path))
}

// initGitignoreMatcher initializes the gitignore matcher for the working directory.
// It is safe to call multiple times; initialization only happens once.
func (t *FilesystemTool) initGitignoreMatcher() {
	if !t.ignoreVCS {
		return
	}

	t.repoMatcherOnce.Do(func() {
		absDir, err := filepath.Abs(t.workingDir)
		if err != nil {
			slog.Warn("Failed to get absolute path for working directory", "dir", t.workingDir, "error", err)
			return
		}

		matcher, err := fsx.NewVCSMatcher(absDir)
		if err != nil {
			slog.Warn("Failed to create VCS matcher", "path", absDir, "error", err)
			return
		}

		t.repoMatcher = matcher
	})
}

// shouldIgnorePath checks if a path should be ignored based on VCS rules
func (t *FilesystemTool) shouldIgnorePath(path string) bool {
	if !t.ignoreVCS {
		return false
	}

	// Always ignore .git directories and their contents
	normalizedPath := filepath.ToSlash(path)
	if strings.Contains(normalizedPath, "/.git/") || strings.HasSuffix(normalizedPath, "/.git") {
		return true
	}

	// Lazily initialize the gitignore matcher on first use
	t.initGitignoreMatcher()

	return t.repoMatcher != nil && t.repoMatcher.ShouldIgnore(path)
}

// Handler implementations

func (t *FilesystemTool) handleDirectoryTree(ctx context.Context, args DirectoryTreeArgs) (*tools.ToolCallResult, error) {
	resolvedPath := t.resolvePath(args.Path)

	tree, err := fsx.DirectoryTree(ctx, resolvedPath, allowAllPaths, t.shouldIgnorePath, maxFiles)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error building directory tree: %s", err)), nil
	}

	result, err := json.Marshal(tree)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error formatting tree: %s", err)), nil
	}

	fileCount, dirCount := countTreeNodes(tree)
	meta := DirectoryTreeMeta{
		FileCount: fileCount,
		DirCount:  dirCount,
		Truncated: fileCount+dirCount >= maxFiles,
	}

	return &tools.ToolCallResult{
		Output: string(result),
		Meta:   meta,
	}, nil
}

func countTreeNodes(node *fsx.TreeNode) (files, dirs int) {
	if node == nil {
		return 0, 0
	}
	if node.Type == "file" {
		return 1, 0
	}
	if node.Type == "directory" {
		dirs = 1
		for _, child := range node.Children {
			f, d := countTreeNodes(child)
			files += f
			dirs += d
		}
	}
	return files, dirs
}

func (t *FilesystemTool) handleEditFile(ctx context.Context, args EditFileArgs) (*tools.ToolCallResult, error) {
	resolvedPath := t.resolvePath(args.Path)

	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error reading file: %s", err)), nil
	}

	originalContent := string(content)
	modifiedContent := originalContent

	var changes []string
	for i, edit := range args.Edits {
		if !strings.Contains(modifiedContent, edit.OldText) {
			return tools.ResultError(fmt.Sprintf("Edit %d failed: old text not found", i+1)), nil
		}
		modifiedContent = strings.Replace(modifiedContent, edit.OldText, edit.NewText, 1)
		changes = append(changes, fmt.Sprintf("Edit %d: Replaced %d characters", i+1, len(edit.OldText)))
	}

	if err := os.WriteFile(resolvedPath, []byte(modifiedContent), 0o644); err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	if err := t.executePostEditCommands(ctx, resolvedPath); err != nil {
		return tools.ResultError(fmt.Sprintf("File edited successfully but post-edit command failed: %s", err)), nil
	}

	if len(changes) == 1 {
		return tools.ResultSuccess("File edited successfully. " + strings.TrimPrefix(changes[0], "Edit 1: ")), nil
	}

	return tools.ResultSuccess("File edited successfully. Changes:\n" + strings.Join(changes, "\n")), nil
}

func (t *FilesystemTool) handleListDirectory(_ context.Context, args ListDirectoryArgs) (*tools.ToolCallResult, error) {
	resolvedPath := t.resolvePath(args.Path)

	entries, err := os.ReadDir(resolvedPath)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error reading directory: %s", err)), nil
	}

	var result strings.Builder
	meta := ListDirectoryMeta{}
	count := 0
	for _, entry := range entries {
		entryPath := filepath.Join(resolvedPath, entry.Name())
		if t.shouldIgnorePath(entryPath) {
			continue
		}

		if entry.IsDir() {
			fmt.Fprintf(&result, "DIR  %s\n", entry.Name())
			meta.Dirs = append(meta.Dirs, entry.Name())
		} else {
			fmt.Fprintf(&result, "FILE %s\n", entry.Name())
			meta.Files = append(meta.Files, entry.Name())
		}
		count++
		if count >= maxFiles {
			result.WriteString("...output truncated due to file limit...\n")
			meta.Truncated = true
			break
		}
	}

	return &tools.ToolCallResult{
		Output: result.String(),
		Meta:   meta,
	}, nil
}

func (t *FilesystemTool) handleReadFile(_ context.Context, args ReadFileArgs) (*tools.ToolCallResult, error) {
	resolvedPath := t.resolvePath(args.Path)

	// Check if the file exists before any type detection.
	info, err := os.Stat(resolvedPath)
	if err != nil {
		var errMsg string
		if errors.Is(err, fs.ErrNotExist) {
			errMsg = "not found"
		} else {
			errMsg = err.Error()
		}

		return &tools.ToolCallResult{
			Output:  errMsg,
			IsError: true,
			Meta: ReadFileMeta{
				Error: errMsg,
			},
		}, nil
	}

	// Only check for image files on regular files (not directories, etc.)
	if info.Mode().IsRegular() && chat.IsImageFile(resolvedPath) {
		return t.readImageFile(resolvedPath, args.Path)
	}

	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return &tools.ToolCallResult{
			Output:  err.Error(),
			IsError: true,
			Meta: ReadFileMeta{
				Error: err.Error(),
			},
		}, nil
	}

	text := string(content)

	return &tools.ToolCallResult{
		Output: text,
		Meta: ReadFileMeta{
			LineCount: strings.Count(text, "\n") + 1,
		},
	}, nil
}

// readImageFile reads an image file and returns it as base64-encoded image content.
// The caller must ensure the file exists (e.g. via os.Stat) before calling this method.
func (t *FilesystemTool) readImageFile(resolvedPath, originalPath string) (*tools.ToolCallResult, error) {
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		errMsg := err.Error()
		return &tools.ToolCallResult{
			Output:  errMsg,
			IsError: true,
			Meta: ReadFileMeta{
				Error: errMsg,
			},
		}, nil
	}

	mimeType := chat.DetectMimeType(resolvedPath)

	// Resize the image if it exceeds provider limits (max 2000×2000, max 4.5MB).
	resized, err := chat.ResizeImage(data, mimeType)
	if err != nil {
		// Check if the original exceeds limits before falling back
		if len(data) > chat.MaxImageBytes {
			return &tools.ToolCallResult{
				Output:  fmt.Sprintf("Error: Image file too large (%d bytes, max %d bytes)", len(data), chat.MaxImageBytes),
				IsError: true,
				Meta:    ReadFileMeta{Path: originalPath, Error: "image too large"},
			}, nil
		}
		// Original is within limits, proceed with fallback
		slog.Warn("Image resize failed, sending original (within limits)", "path", originalPath, "error", err)
		encoded := base64.StdEncoding.EncodeToString(data)
		return &tools.ToolCallResult{
			Output: fmt.Sprintf("Read image file %s [%s] (%d bytes)", originalPath, mimeType, len(data)),
			Images: []tools.ImageContent{{Data: encoded, MimeType: mimeType}},
			Meta:   ReadFileMeta{Path: originalPath},
		}, nil
	}

	encoded := base64.StdEncoding.EncodeToString(resized.Data)
	output := fmt.Sprintf("Read image file %s [%s] (%d bytes)", originalPath, resized.MimeType, len(resized.Data))
	if note := chat.FormatDimensionNote(resized); note != "" {
		output += "\n" + note
	}

	return &tools.ToolCallResult{
		Output: output,
		Images: []tools.ImageContent{{Data: encoded, MimeType: resized.MimeType}},
		Meta:   ReadFileMeta{Path: originalPath},
	}, nil
}

func (t *FilesystemTool) handleReadMultipleFiles(ctx context.Context, args ReadMultipleFilesArgs) (*tools.ToolCallResult, error) {
	type PathContent struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}

	var contents []PathContent
	var meta ReadMultipleFilesMeta

	for _, path := range args.Paths {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		entry := ReadFileMeta{Path: path}

		resolvedPath := t.resolvePath(path)

		content, err := os.ReadFile(resolvedPath)
		if err != nil {
			errMsg := err.Error()
			if errors.Is(err, fs.ErrNotExist) {
				errMsg = "not found"
			}
			contents = append(contents, PathContent{
				Path:    path,
				Content: errMsg,
			})
			entry.Error = errMsg
			meta.Files = append(meta.Files, entry)
			continue
		}

		text := string(content)
		contents = append(contents, PathContent{
			Path:    path,
			Content: text,
		})
		entry.Content = text
		entry.LineCount = strings.Count(text, "\n") + 1
		meta.Files = append(meta.Files, entry)
	}

	var output string
	if args.JSON {
		jsonResult, err := json.Marshal(contents)
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Error formatting JSON: %s", err)), nil
		}
		output = string(jsonResult)
	} else {
		var result strings.Builder
		for _, content := range contents {
			fmt.Fprintf(&result, "=== %s ===\n%s\n\n", content.Path, content.Content)
		}
		output = result.String()
	}

	return &tools.ToolCallResult{
		Output: output,
		Meta:   meta,
	}, nil
}

func (t *FilesystemTool) handleSearchFilesContent(_ context.Context, args SearchFilesContentArgs) (*tools.ToolCallResult, error) {
	resolvedPath := t.resolvePath(args.Path)

	var regex *regexp.Regexp
	if args.IsRegex {
		var err error
		regex, err = regexp.Compile(args.Query)
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Invalid regex pattern: %s", err)), nil
		}
	}

	var results []string
	filesWithMatches := make(map[string]struct{})

	err := filepath.WalkDir(resolvedPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		// Check VCS ignore rules
		if t.shouldIgnorePath(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Check exclude patterns against relative path from search root
		relPath, err := filepath.Rel(resolvedPath, path)
		if err != nil {
			return nil
		}

		for _, exclude := range args.ExcludePatterns {
			if matchExcludePattern(exclude, relPath) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}

		// Only process files, not directories
		if d.IsDir() {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(content), "\n")
		for lineNum, line := range lines {
			var matched bool
			var matchStart, matchEnd int

			if args.IsRegex {
				if loc := regex.FindStringIndex(line); loc != nil {
					matched = true
					matchStart, matchEnd = loc[0], loc[1]
				}
			} else {
				if idx := strings.Index(line, args.Query); idx != -1 {
					matched = true
					matchStart, matchEnd = idx, idx+len(args.Query)
				}
			}

			if matched {
				filesWithMatches[path] = struct{}{}
				preview := line
				if len(preview) > 100 {
					start := max(matchStart-20, 0)
					end := matchEnd + 20
					end = min(end, len(preview))
					preview = preview[start:end]
				}

				result := fmt.Sprintf("%s:%d:%d: %s", path, lineNum+1, matchStart+1, preview)
				results = append(results, result)
			}
		}

		return nil
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error searching file contents: %s", err)), nil
	}

	meta := SearchFilesContentMeta{
		MatchCount: len(results),
		FileCount:  len(filesWithMatches),
	}

	if len(results) == 0 {
		return &tools.ToolCallResult{
			Output: "No results found",
			Meta:   meta,
		}, nil
	}

	return &tools.ToolCallResult{
		Output: strings.Join(results, "\n"),
		Meta:   meta,
	}, nil
}

func (t *FilesystemTool) handleWriteFile(ctx context.Context, args WriteFileArgs) (*tools.ToolCallResult, error) {
	resolvedPath := t.resolvePath(args.Path)

	// Create parent directory structure if it doesn't exist
	dir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.ResultError(fmt.Sprintf("Error creating directory structure: %s", err)), nil
	}

	if err := os.WriteFile(resolvedPath, []byte(args.Content), 0o644); err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	if err := t.executePostEditCommands(ctx, resolvedPath); err != nil {
		return tools.ResultError(fmt.Sprintf("File written successfully but post-edit command failed: %s", err)), nil
	}

	return tools.ResultSuccess(fmt.Sprintf("File written successfully: %s (%d bytes)", args.Path, len(args.Content))), nil
}

func (t *FilesystemTool) handleCreateDirectory(_ context.Context, args CreateDirectoryArgs) (*tools.ToolCallResult, error) {
	var results []string
	for _, path := range args.Paths {
		resolvedPath := t.resolvePath(path)
		if err := os.MkdirAll(resolvedPath, 0o755); err != nil {
			return tools.ResultError(fmt.Sprintf("Error creating directory %s: %s", path, err)), nil
		}
		results = append(results, "Directory created successfully: "+path)
	}

	return tools.ResultSuccess(strings.Join(results, "\n")), nil
}

func (t *FilesystemTool) handleRemoveDirectory(_ context.Context, args RemoveDirectoryArgs) (*tools.ToolCallResult, error) {
	var results []string
	for _, path := range args.Paths {
		resolvedPath := t.resolvePath(path)

		if err := rmdir(resolvedPath); err != nil {
			return tools.ResultError(fmt.Sprintf("Error removing directory %s: %s", path, err)), nil
		}
		results = append(results, "Directory removed successfully: "+path)
	}

	return tools.ResultSuccess(strings.Join(results, "\n")), nil
}

// matchExcludePattern checks if a path should be excluded based on the exclude pattern
// It supports glob patterns and directory wildcards like .git/*
func matchExcludePattern(pattern, relPath string) bool {
	// Normalize path separators to forward slashes for consistent matching
	normalizedPath := filepath.ToSlash(relPath)
	normalizedPattern := filepath.ToSlash(pattern)

	// Handle directory patterns ending with /*
	if dirPattern, found := strings.CutSuffix(normalizedPattern, "/*"); found {
		// Check if path starts with the directory pattern
		if strings.HasPrefix(normalizedPath, dirPattern+"/") || normalizedPath == dirPattern {
			return true
		}
	}

	// Try glob pattern matching on the full relative path
	if matched, _ := filepath.Match(normalizedPattern, normalizedPath); matched {
		return true
	}

	// Try glob pattern matching on just the base name for backwards compatibility
	if matched, _ := filepath.Match(normalizedPattern, filepath.Base(normalizedPath)); matched {
		return true
	}

	// Check if pattern matches any parent directory path
	pathParts := strings.Split(normalizedPath, "/")
	for i := range pathParts {
		subPath := strings.Join(pathParts[:i+1], "/")
		if matched, _ := filepath.Match(normalizedPattern, subPath); matched {
			return true
		}
	}

	return false
}
