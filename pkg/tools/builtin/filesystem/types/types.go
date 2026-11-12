// Package types defines the shared wire contracts for the filesystem tools.
package types

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
	// Line and Limit are pointers so an omitted value is distinguishable
	// from an explicit (invalid) zero.
	Line  *int `json:"line,omitempty" jsonschema:"1-based line number to start reading from (text files only; defaults to the first line)"`
	Limit *int `json:"limit,omitempty" jsonschema:"Maximum number of lines to read (text files only; defaults to reading through the end of the file)"`
}

type ReadFileMeta struct {
	Path      string `json:"path"`
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
