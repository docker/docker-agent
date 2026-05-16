package filesystem

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/fsx"
	pathx "github.com/docker/docker-agent/pkg/path"
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

type Tool struct {
	workingDir       string
	postEditCommands []PostEditConfig
	ignoreVCS        bool
	repoMatcher      *fsx.VCSMatcher
	repoMatcherOnce  sync.Once

	// sandboxBroken is set when allow/deny list construction fails.
	// When true, all filesystem operations are rejected (fail-closed).
	sandboxBroken bool

	// allowList, when non-nil, restricts every filesystem operation to paths
	// that resolve under one of the listed roots. nil means "no allow-list";
	// the toolset accepts any path the OS will let it touch.
	allowList *pathRootSet
	// denyList, when non-nil, rejects every filesystem operation on paths
	// that resolve under one of the listed roots, even when the path also
	// matches the allow-list. nil means "no deny-list".
	denyList *pathRootSet
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
	_ io.Closer          = (*Tool)(nil)
)

type Opt func(*Tool)

func WithPostEditCommands(postEditCommands []PostEditConfig) Opt {
	return func(t *Tool) {
		t.postEditCommands = postEditCommands
	}
}

func WithIgnoreVCS(ignoreVCS bool) Opt {
	return func(t *Tool) {
		t.ignoreVCS = ignoreVCS
	}
}

// WithAllowList restricts every filesystem operation to paths that resolve
// under one of the supplied roots. Each entry may be:
//   - "." — the agent's working directory
//   - "~" or "~/..." — the user's home directory
//   - "$VAR" / "${VAR}" — an environment variable
//   - any absolute or relative path (relative paths are anchored at the
//     working directory)
//
// Symlinks are resolved before the containment check, so a symlink inside an
// allowed root cannot be used to escape it. An empty or nil slice disables
// the allow-list and preserves the default behaviour (any path is allowed).
//
// Invalid entries (e.g. an empty string) are logged and the allow-list is
// silently dropped, mirroring how WithIgnoreVCS handles construction errors.
func WithAllowList(roots []string) Opt {
	return func(t *Tool) {
		set, err := newPathRootSet(t.workingDir, roots)
		if err != nil {
			slog.Error("filesystem allow-list: invalid entry; disabling toolset", "error", err)
			t.sandboxBroken = true
			return
		}
		t.allowList = set
	}
}

// WithDenyList forbids every filesystem operation on paths that resolve under
// one of the supplied roots. Tokens follow the same expansion rules as
// [WithAllowList]. The deny-list takes precedence over the allow-list: a
// path that matches both is rejected. An empty or nil slice disables the
// deny-list.
func WithDenyList(roots []string) Opt {
	return func(t *Tool) {
		set, err := newPathRootSet(t.workingDir, roots)
		if err != nil {
			slog.Error("filesystem deny-list: invalid entry; disabling toolset", "error", err)
			t.sandboxBroken = true
			return
		}
		t.denyList = set
	}
}

func NewFilesystemTool(workingDir string, opts ...Opt) *Tool {
	t := &Tool{
		workingDir: workingDir,
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

// Close releases any *os.Root file descriptors held by the allow/deny lists.
// It is safe to call Close multiple times.
func (t *Tool) Close() error {
	if t.allowList != nil {
		t.allowList.close()
	}
	if t.denyList != nil {
		t.denyList.close()
	}
	return nil
}

func (t *Tool) Instructions() string {
	var b strings.Builder
	b.WriteString(`## Filesystem Tools

- Relative paths resolve from the working directory; absolute paths and ".." work as expected
- Prefer read_multiple_files over sequential read_file calls
- Use search_files_content to locate code or text across files
- Use exclude patterns in searches and max_depth in directory_tree to limit output`)
	if d := t.allowList.describe(); d != "" {
		fmt.Fprintf(&b, "\n- These tools are restricted to paths under: %s. Any other path is rejected without touching the filesystem.", d)
	}
	if d := t.denyList.describe(); d != "" {
		fmt.Fprintf(&b, "\n- These tools must not access paths under: %s. Such paths are rejected without touching the filesystem.", d)
	}
	return b.String()
}

type DirectoryTreeArgs struct {
	Path string `json:"path" jsonschema:"Directory to traverse"`
}

type WriteFileArgs struct {
	Path    string `json:"path" jsonschema:"File to write"`
	Content string `json:"content" jsonschema:"File content"`
}

type ReadMultipleFilesArgs struct {
	Paths []string `json:"paths" jsonschema:"Files to read"`
	JSON  bool     `json:"json,omitempty" jsonschema:"Return result as JSON"`
}

type ReadMultipleFilesMeta struct {
	Files []ReadFileMeta `json:"files"`
}

type SearchFilesContentArgs struct {
	Path            string   `json:"path" jsonschema:"Starting directory"`
	Query           string   `json:"query" jsonschema:"Text or regex to search"`
	IsRegex         bool     `json:"is_regex,omitempty" jsonschema:"Treat query as regex"`
	ExcludePatterns []string `json:"excludePatterns,omitempty" jsonschema:"Patterns to exclude"`
}

type SearchFilesContentMeta struct {
	MatchCount int `json:"matchCount"`
	FileCount  int `json:"fileCount"`
}

type ListDirectoryArgs struct {
	Path string `json:"path" jsonschema:"Directory to list"`
}

type CreateDirectoryArgs struct {
	Paths []string `json:"paths" jsonschema:"Directories to create"`
}

type RemoveDirectoryArgs struct {
	Paths []string `json:"paths" jsonschema:"Directories to remove"`
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
	Path string `json:"path" jsonschema:"File to read"`
}

type ReadFileMeta struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	LineCount int    `json:"lineCount"`
	Error     string `json:"error,omitempty"`
}

type Edit struct {
	OldText string `json:"oldText" jsonschema:"Exact text to replace"`
	NewText string `json:"newText" jsonschema:"Replacement text"`
}

type EditFileArgs struct {
	Path  string `json:"path" jsonschema:"File to edit"`
	Edits []Edit `json:"edits" jsonschema:"Edits to apply"`
}

// ParseEditFileArgs parses LLM-generated edit_file arguments, handling two
// common failure modes:
//  1. The outer JSON itself is malformed — typically extra closing braces/brackets
//     or stray escape sequences caused by the model losing track of nesting depth
//     when the text payload contains structural characters (e.g. YAML, Dockerfiles).
//  2. The "edits" field is double-serialized (a JSON string instead of an array).
func ParseEditFileArgs(data []byte) (EditFileArgs, error) {
	var raw struct {
		Path  string          `json:"path"`
		Edits json.RawMessage `json:"edits"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		repaired, ok := tryRepairEditFileJSON(data)
		if !ok {
			return EditFileArgs{}, fmt.Errorf("failed to parse edit_file arguments: %w", err)
		}
		if err := json.Unmarshal(repaired, &raw); err != nil {
			return EditFileArgs{}, fmt.Errorf("failed to parse edit_file arguments after repair: %w", err)
		}
		slog.Debug("Repaired malformed edit_file JSON arguments")
	}

	args := EditFileArgs{Path: raw.Path}

	// When edits is missing or null (e.g. during argument streaming in
	// the TUI, or partial tool calls), accept the partial result.
	if len(raw.Edits) == 0 || string(raw.Edits) == "null" {
		return args, nil
	}

	// Try parsing edits as an array first (normal case).
	if err := json.Unmarshal(raw.Edits, &args.Edits); err == nil {
		return args, nil
	}

	// Try unwrapping a double-serialized JSON string.
	var editsStr string
	if err := json.Unmarshal(raw.Edits, &editsStr); err != nil {
		return EditFileArgs{}, fmt.Errorf("edits field is neither an array nor a JSON string: %w", err)
	}
	if err := json.Unmarshal([]byte(editsStr), &args.Edits); err != nil {
		return EditFileArgs{}, fmt.Errorf("failed to parse double-serialized edits string: %w", err)
	}

	return args, nil
}

// tryRepairEditFileJSON attempts to fix common LLM JSON malformations by
// iteratively removing the offending character(s) at each json.SyntaxError
// offset. Observed failure modes from production sessions:
//
//   - Extra '}' — model loses brace count (e.g. "}}]}" instead of "}]}")
//   - Extra ']' — model adds a spurious array wrapper
//   - Stray '\' — model emits an escape sequence outside of a string value
//     (e.g. literal \n between tokens, or \" where " is expected)
func tryRepairEditFileJSON(data []byte) ([]byte, bool) {
	current := append([]byte(nil), data...) // defensive copy
	for range 3 {
		var synErr *json.SyntaxError
		if err := json.Unmarshal(current, &json.RawMessage{}); err == nil {
			return current, true
		} else if !errors.As(err, &synErr) {
			return nil, false
		}

		// json.SyntaxError.Offset is 1-based.
		offset := int(synErr.Offset) - 1
		if offset < 0 || offset >= len(current) {
			return nil, false
		}

		ch := current[offset]
		removeCount := 1

		switch ch {
		case '}', ']':
			// Extra closing delimiter — just remove it.
		case '\\':
			// Stray escape sequence outside a string value. For \n, \t, \r
			// both characters are garbage so remove them. For \" the quote
			// is a valid structural character (string delimiter), so only
			// strip the backslash.
			if offset+1 < len(current) {
				switch current[offset+1] {
				case 'n', 't', 'r':
					removeCount = 2
				}
			}
		default:
			return nil, false
		}

		repaired := make([]byte, 0, len(current)-removeCount)
		repaired = append(repaired, current[:offset]...)
		repaired = append(repaired, current[offset+removeCount:]...)
		current = repaired
	}

	if json.Valid(current) {
		return current, true
	}
	return nil, false
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
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
			Handler:      t.editFileHandler(),
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
func (t *Tool) executePostEditCommands(ctx context.Context, filePath string) error {
	if len(t.postEditCommands) == 0 {
		return nil
	}
	return runPostEditCommands(ctx, t.postEditCommands, filePath)
}

// resolvePath resolves a path relative to the working directory.
// A leading "~" or "~/" is expanded to the user's home directory so that
// LLM-supplied paths like "~/file.txt" work without the agent having to
// know the user's home directory upfront. Relative paths (including ".")
// are joined with the working directory. Absolute paths and paths
// starting with ".." are used as-is.
//
// resolvePath does NOT enforce the allow- or deny-lists; callers should use
// [resolveAndCheckPath] when those checks are required (i.e. for any path
// that originates from a tool argument).
func (t *Tool) resolvePath(path string) string {
	if expandedPath, err := pathx.ExpandHomeDir(path); err == nil {
		path = expandedPath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}

	return filepath.Clean(filepath.Join(t.workingDir, path))
}

// resolveAndCheckPath is the canonical entry point used by every filesystem
// handler that operates on a user-supplied path. It resolves the path against
// the working directory and validates the result against the allow- and
// deny-lists. The returned string is the on-disk path the handler should pass
// to the os.* call.
//
// When neither list is configured the function is functionally equivalent to
// [resolvePath]. When a list is configured, symlinks are resolved before the
// containment check so that a symlink inside an allowed directory cannot leak
// access outside it. Paths that don't exist yet (e.g. for write_file or
// create_directory) are checked against their nearest existing ancestor so
// the caller can still create them.
//
// The deny-list takes precedence over the allow-list: a path that matches
// both is rejected.
//
// SECURITY NOTE: this static check has a small TOCTOU window before the
// subsequent os.* call. To close it for paths that fall inside the
// allow-list, the toolset routes its actual I/O through the [*os.Root]
// handles owned by [pathRootSet] (see [Tool.readFile], [.writeFile],
// [.mkdirAll], etc.). Those methods reject ".." and out-of-root symlinks
// in the kernel, regardless of timing. The threat model is the LLM
// itself, which has no symlink-creation primitive, so this layered
// defence is sufficient.
//
// PLATFORM NOTE: [*os.Root] guarantees vary by GOOS — see the package
// docs. Notable carve-outs that matter here:
//
//   - Linux bind mounts, filesystem boundaries, /proc special files and
//     Unix device files are NOT blocked by [*os.Root]. An allow-list root
//     that contains, e.g., a /proc bind mount can still leak.
//   - On GOOS=js (WASM), [*os.Root] is itself vulnerable to TOCTOU in
//     symlink validation and cannot guarantee containment. The agent is
//     not supported on WASM today; this is documented for future ports.
//   - On GOOS=plan9 and GOOS=js, [*os.Root] tracks a directory name
//     rather than a file descriptor, so it does not follow renames of
//     the allow-list root.
//   - On GOOS=windows, [*os.Root] additionally rejects reserved device
//     names (NUL, COM1, …), which is a strengthening, not a weakening.
func (t *Tool) resolveAndCheckPath(path string) (string, error) {
	if t.sandboxBroken {
		return "", errors.New("filesystem toolset is disabled due to invalid allow/deny list configuration")
	}

	resolved := t.resolvePath(path)
	if t.allowList == nil && t.denyList == nil {
		return resolved, nil
	}
	realPath, err := resolveRealPath(resolved)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	if t.denyList != nil && t.denyList.contains(realPath) {
		return "", fmt.Errorf("path %q is inside a denied directory (%s)", path, t.denyList.describe())
	}
	if t.allowList != nil && !t.allowList.contains(realPath) {
		return "", fmt.Errorf("path %q is outside the allowed directories (%s)", path, t.allowList.describe())
	}
	return resolved, nil
}

// rootedAccess returns the [*os.Root] handle and rooted (slash-separated)
// name for resolved when the allow-list is configured.
//
//   - No allow-list → (nil, "", nil): callers fall back to plain os.*.
//   - Path inside an entry whose [*os.Root] is open → (root, rel, nil):
//     callers MUST use the rooted handle so the kernel re-checks
//     containment on every component, closing the race.
//   - Path inside an entry whose [*os.Root] could not be opened (e.g. the
//     directory did not exist at construction time) → (nil, "", nil):
//     callers fall back to plain os.* with the lexical guarantee already
//     enforced by [resolveAndCheckPath].
//   - Path no longer inside any entry (e.g. a symlink swap moved the real
//     target out between the static check and the I/O) → (nil, "", err):
//     callers MUST refuse; falling back to os.* would follow the symlink.
func (t *Tool) rootedAccess(resolved string) (*os.Root, string, error) {
	if t.allowList == nil {
		return nil, "", nil
	}
	realPath, err := resolveRealPath(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("resolving %q: %w", resolved, err)
	}
	entry, rel := t.allowList.entryFor(realPath)
	if entry == nil {
		return nil, "", fmt.Errorf("path %q is no longer inside the allow-list (possible symlink swap)", resolved)
	}
	return entry.root, rel, nil // entry.root may be nil; caller falls back to os.*
}

// readFile is a TOCTOU-safe equivalent of [os.ReadFile] for paths that the
// allow-list contains. When no rooted access is available it falls back to
// the plain [os.ReadFile]. Callers MUST pass a path that has already been
// validated by [resolveAndCheckPath].
func (t *Tool) readFile(resolved string) ([]byte, error) {
	root, rel, err := t.rootedAccess(resolved)
	if err != nil {
		return nil, err
	}
	if root != nil {
		return root.ReadFile(rel)
	}
	return os.ReadFile(resolved)
}

// writeFile is a TOCTOU-safe equivalent of [os.WriteFile]. See [readFile]
// for the contract. The call is rejected by the kernel when any component
// of rel is an out-of-root symlink, so an attacker cannot win the swap
// race between the [resolveAndCheckPath] check and the write.
func (t *Tool) writeFile(resolved string, data []byte, perm os.FileMode) error {
	root, rel, err := t.rootedAccess(resolved)
	if err != nil {
		return err
	}
	if root != nil {
		return root.WriteFile(rel, data, perm)
	}
	return os.WriteFile(resolved, data, perm)
}

// stat is a TOCTOU-safe equivalent of [os.Stat]. See [readFile] for the
// contract.
func (t *Tool) stat(resolved string) (os.FileInfo, error) {
	root, rel, err := t.rootedAccess(resolved)
	if err != nil {
		return nil, err
	}
	if root != nil {
		return root.Stat(rel)
	}
	return os.Stat(resolved)
}

// mkdirAll is a TOCTOU-safe equivalent of [os.MkdirAll]. See [readFile]
// for the contract. A rooted MkdirAll on "." is a no-op (the root already
// exists by construction).
func (t *Tool) mkdirAll(resolved string, perm os.FileMode) error {
	root, rel, err := t.rootedAccess(resolved)
	if err != nil {
		return err
	}
	if root != nil {
		if rel == "." {
			return nil
		}
		return root.MkdirAll(rel, perm)
	}
	return os.MkdirAll(resolved, perm)
}

// readDir is a TOCTOU-safe equivalent of [os.ReadDir]. See [readFile]
// for the contract. We use [*os.Root].Open + [*os.File].ReadDir because
// [*os.Root] does not expose ReadDir directly.
func (t *Tool) readDir(resolved string) ([]os.DirEntry, error) {
	root, rel, err := t.rootedAccess(resolved)
	if err != nil {
		return nil, err
	}
	if root != nil {
		f, err := root.Open(rel)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return f.ReadDir(-1)
	}
	return os.ReadDir(resolved)
}

// removeDir removes an empty directory at resolved. When a rooted handle
// is available we use [*os.Root].Remove, which only unlinks the named
// directory entry and refuses to follow a trailing symlink that escapes
// the root. Otherwise we fall back to the platform-specific [rmdir].
func (t *Tool) removeDir(resolved string) error {
	root, rel, err := t.rootedAccess(resolved)
	if err != nil {
		return err
	}
	if root != nil {
		return root.Remove(rel)
	}
	return rmdir(resolved)
}

// initGitignoreMatcher initializes the gitignore matcher for the working directory.
// It is safe to call multiple times; initialization only happens once.
func (t *Tool) initGitignoreMatcher() {
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
func (t *Tool) shouldIgnorePath(path string) bool {
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

func (t *Tool) handleDirectoryTree(ctx context.Context, args DirectoryTreeArgs) (*tools.ToolCallResult, error) {
	resolvedPath, err := t.resolveAndCheckPath(args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	// Create a path checker that enforces allow/deny lists for every child path.
	// This prevents symlinked directories from being traversed outside the sandbox.
	pathChecker := func(childPath string) error {
		_, err := t.resolveAndCheckPath(childPath)
		return err
	}

	tree, err := fsx.DirectoryTree(ctx, resolvedPath, pathChecker, t.shouldIgnorePath, maxFiles)
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

// editFileHandler returns a ToolHandler that parses edit_file arguments with
// repair logic for malformed JSON, then delegates to handleEditFile.
// This bypasses tools.NewHandler because Go's json.Unmarshal scanner rejects
// structurally invalid JSON before calling any custom UnmarshalJSON method.
func (t *Tool) editFileHandler() tools.ToolHandler {
	return func(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
		data := toolCall.Function.Arguments
		if data == "" {
			data = "{}"
		}
		args, err := ParseEditFileArgs([]byte(data))
		if err != nil {
			return nil, err
		}
		return t.handleEditFile(ctx, args)
	}
}

func (t *Tool) handleEditFile(ctx context.Context, args EditFileArgs) (*tools.ToolCallResult, error) {
	resolvedPath, err := t.resolveAndCheckPath(args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	content, err := t.readFile(resolvedPath)
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

	if err := t.writeFile(resolvedPath, []byte(modifiedContent), 0o644); err != nil {
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

func (t *Tool) handleListDirectory(_ context.Context, args ListDirectoryArgs) (*tools.ToolCallResult, error) {
	resolvedPath, err := t.resolveAndCheckPath(args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	entries, err := t.readDir(resolvedPath)
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

func (t *Tool) handleReadFile(_ context.Context, args ReadFileArgs) (*tools.ToolCallResult, error) {
	resolvedPath, err := t.resolveAndCheckPath(args.Path)
	if err != nil {
		return &tools.ToolCallResult{
			Output:  err.Error(),
			IsError: true,
			Meta:    ReadFileMeta{Path: args.Path, Error: err.Error()},
		}, nil
	}

	// Check if the file exists before any type detection.
	info, err := t.stat(resolvedPath)
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

	content, err := t.readFile(resolvedPath)
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
func (t *Tool) readImageFile(resolvedPath, originalPath string) (*tools.ToolCallResult, error) {
	data, err := t.readFile(resolvedPath)
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

func (t *Tool) handleReadMultipleFiles(ctx context.Context, args ReadMultipleFilesArgs) (*tools.ToolCallResult, error) {
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

		resolvedPath, err := t.resolveAndCheckPath(path)
		if err != nil {
			errMsg := err.Error()
			contents = append(contents, PathContent{
				Path:    path,
				Content: errMsg,
			})
			entry.Error = errMsg
			meta.Files = append(meta.Files, entry)
			continue
		}

		content, err := t.readFile(resolvedPath)
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

func (t *Tool) handleSearchFilesContent(_ context.Context, args SearchFilesContentArgs) (*tools.ToolCallResult, error) {
	resolvedPath, err := t.resolveAndCheckPath(args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

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

	err = filepath.WalkDir(resolvedPath, func(path string, d fs.DirEntry, err error) error {
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

		// Check this file against allow/deny lists before reading.
		// This prevents symlinks inside the allowed root from escaping the sandbox.
		if _, checkErr := t.resolveAndCheckPath(path); checkErr != nil {
			// Skip files outside the sandbox silently (don't fail the whole search).
			return nil
		}

		content, err := t.readFile(path)
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

func (t *Tool) handleWriteFile(ctx context.Context, args WriteFileArgs) (*tools.ToolCallResult, error) {
	resolvedPath, err := t.resolveAndCheckPath(args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	// Create parent directory structure if it doesn't exist
	dir := filepath.Dir(resolvedPath)
	if err := t.mkdirAll(dir, 0o755); err != nil {
		return tools.ResultError(fmt.Sprintf("Error creating directory structure: %s", err)), nil
	}

	if err := t.writeFile(resolvedPath, []byte(args.Content), 0o644); err != nil {
		return tools.ResultError(fmt.Sprintf("Error writing file: %s", err)), nil
	}

	if err := t.executePostEditCommands(ctx, resolvedPath); err != nil {
		return tools.ResultError(fmt.Sprintf("File written successfully but post-edit command failed: %s", err)), nil
	}

	return tools.ResultSuccess(fmt.Sprintf("File written successfully: %s (%d bytes)", args.Path, len(args.Content))), nil
}

func (t *Tool) handleCreateDirectory(_ context.Context, args CreateDirectoryArgs) (*tools.ToolCallResult, error) {
	var results []string
	for _, path := range args.Paths {
		resolvedPath, err := t.resolveAndCheckPath(path)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
		if err := t.mkdirAll(resolvedPath, 0o755); err != nil {
			return tools.ResultError(fmt.Sprintf("Error creating directory %s: %s", path, err)), nil
		}
		results = append(results, "Directory created successfully: "+path)
	}

	return tools.ResultSuccess(strings.Join(results, "\n")), nil
}

func (t *Tool) handleRemoveDirectory(_ context.Context, args RemoveDirectoryArgs) (*tools.ToolCallResult, error) {
	var results []string
	for _, path := range args.Paths {
		resolvedPath, err := t.resolveAndCheckPath(path)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}

		if err := t.removeDir(resolvedPath); err != nil {
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
