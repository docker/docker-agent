package lsp

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLSPTool(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", []string{}, nil, "/tmp")
	require.NotNil(t, tool)
	require.NotNil(t, tool.handler)
}

func TestLSPTool_Tools(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	tools, err := tool.Tools(t.Context())
	require.NoError(t, err)

	// Verify all expected tools are present
	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolNames[tool.Name] = true
	}

	expectedTools := []string{
		ToolNameLSPWorkspace,
		ToolNameLSPHover,
		ToolNameLSPDefinition,
		ToolNameLSPReferences,
		ToolNameLSPDocumentSymbols,
		ToolNameLSPWorkspaceSymbols,
		ToolNameLSPDiagnostics,
		ToolNameLSPRename,
		ToolNameLSPCodeActions,
		ToolNameLSPFormat,
		ToolNameLSPCallHierarchy,
		ToolNameLSPTypeHierarchy,
		ToolNameLSPImplementations,
		ToolNameLSPSignatureHelp,
		ToolNameLSPInlayHints,
	}

	for _, name := range expectedTools {
		assert.True(t, toolNames[name], "Expected tool %s to be present", name)
	}

	// Verify we have exactly the expected number of tools (no extras like initialize/shutdown)
	assert.Len(t, tools, len(expectedTools))
}

func TestLSPTool_ToolDescriptions(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	tools, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range tools {
		// Each tool should have a non-empty description
		assert.NotEmpty(t, tool.Description, "Tool %s should have a description", tool.Name)

		// Each description should be meaningful (more than 50 chars)
		assert.Greater(t, len(tool.Description), 50,
			"Tool %s should have a meaningful description, got: %s", tool.Name, tool.Description)
	}
}

func TestLSPTool_Instructions(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	instructions := tool.Instructions()

	// Should mention the tools are stateless
	assert.Contains(t, instructions, "Stateless")

	// Should list available operations
	assert.Contains(t, instructions, "lsp_hover")
	assert.Contains(t, instructions, "lsp_definition")
	assert.Contains(t, instructions, "lsp_references")

	// Should explain position format
	assert.Contains(t, instructions, "1-based")
}

func TestFormatLocation(t *testing.T) {
	t.Parallel()

	loc := lspLocation{
		URI: "file:///home/user/project/main.go",
		Range: lspRange{
			Start: lspPosition{Line: 9, Character: 4},
			End:   lspPosition{Line: 9, Character: 10},
		},
	}

	result := formatLocation(loc)
	// Output should be 1-based (line 9 -> 10, char 4 -> 5)
	assert.Equal(t, "- /home/user/project/main.go:10:5", result)
}

func TestFormatLocations_Single(t *testing.T) {
	t.Parallel()

	loc := lspLocation{
		URI: "file:///test.go",
		Range: lspRange{
			Start: lspPosition{Line: 0, Character: 0},
		},
	}

	data, err := json.Marshal(loc)
	require.NoError(t, err)

	result := formatLocations(data)
	// Line 0 -> 1, char 0 -> 1
	assert.Contains(t, result, "/test.go:1:1")
}

func TestFormatLocations_Array(t *testing.T) {
	t.Parallel()

	locs := []lspLocation{
		{URI: "file:///a.go", Range: lspRange{Start: lspPosition{Line: 0, Character: 0}}},
		{URI: "file:///b.go", Range: lspRange{Start: lspPosition{Line: 5, Character: 10}}},
	}

	data, err := json.Marshal(locs)
	require.NoError(t, err)

	result := formatLocations(data)
	assert.Contains(t, result, "Found 2 location(s)")
	assert.Contains(t, result, "/a.go:1:1")
	assert.Contains(t, result, "/b.go:6:11") // 0-based -> 1-based
}

func TestFormatSymbols_SymbolInformation(t *testing.T) {
	t.Parallel()

	symbols := []lspSymbolInformation{
		{
			Name:          "MyFunction",
			Kind:          12, // Function
			Location:      lspLocation{URI: "file:///test.go", Range: lspRange{Start: lspPosition{Line: 10, Character: 0}}},
			ContainerName: "main",
		},
		{
			Name:     "MyVariable",
			Kind:     13, // Variable
			Location: lspLocation{URI: "file:///test.go", Range: lspRange{Start: lspPosition{Line: 5, Character: 0}}},
		},
	}

	data, err := json.Marshal(symbols)
	require.NoError(t, err)

	result := formatSymbols(data)
	assert.Contains(t, result, "Function MyFunction")
	assert.Contains(t, result, "[in main]")
	assert.Contains(t, result, "Variable MyVariable")
}

func TestFormatDiagnostics(t *testing.T) {
	t.Parallel()

	diags := []lspDiagnostic{
		{
			Range:    lspRange{Start: lspPosition{Line: 10, Character: 5}},
			Severity: 1,
			Message:  "undefined: foo",
		},
		{
			Range:    lspRange{Start: lspPosition{Line: 20, Character: 0}},
			Severity: 2,
			Message:  "unused variable",
		},
	}

	result := formatDiagnostics("/test.go", diags)
	assert.Contains(t, result, "Diagnostics for /test.go")
	assert.Contains(t, result, "[Error] Line 11: undefined: foo") // 0-based -> 1-based
	assert.Contains(t, result, "[Warning] Line 21: unused variable")
}

func TestSymbolKindName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind     int
		expected string
	}{
		{1, "File"},
		{5, "Class"},
		{6, "Method"},
		{12, "Function"},
		{13, "Variable"},
		{23, "Struct"},
		{99, "Kind99"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, symbolKindName(tt.kind))
	}
}

func TestDiagnosticSeverityName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		severity int
		expected string
	}{
		{1, "Error"},
		{2, "Warning"},
		{3, "Info"},
		{4, "Hint"},
		{5, "Unknown"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, diagnosticSeverityName(tt.severity))
	}
}

func TestFormatHoverContents(t *testing.T) {
	t.Parallel()

	// String content
	assert.Equal(t, "hello", formatHoverContents("hello"))

	// Map with value
	mapContent := map[string]any{"value": "type info here"}
	assert.Equal(t, "type info here", formatHoverContents(mapContent))

	// Array content
	arrayContent := []any{"first", "second"}
	result := formatHoverContents(arrayContent)
	assert.Contains(t, result, "first")
	assert.Contains(t, result, "second")
}

func TestLSPHandler_NotInitialized_AutoInitializes(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("nonexistent-lsp-server", nil, nil, "/tmp")

	// Test that operations attempt auto-initialization
	// (will fail because the server doesn't exist, but should try)
	ctx := t.Context()

	result, err := tool.handler.hover(ctx, PositionArgs{
		File:      "/tmp/test.go",
		Line:      1,
		Character: 1,
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	// Should mention initialization failure, not "not initialized"
	assert.Contains(t, result.Output, "LSP initialization failed")
}

func TestLSPHandler_GetDiagnostics_NoDiagnostics(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	// Mark as initialized to test the diagnostics retrieval path
	tool.handler.initialized.Store(true)
	// Pretend we have a running server by setting a non-nil cmd
	// We use exec.Command which creates a valid *exec.Cmd without running anything
	tool.handler.cmd = exec.CommandContext(t.Context(), "true")

	ctx := t.Context()

	// Mark file as open to skip auto-open attempt
	tool.handler.openFiles["file:///nonexistent.go"] = 1

	result, err := tool.handler.getDiagnostics(ctx, FileArgs{File: "/nonexistent.go"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "No diagnostics")
}

func TestLSPHandler_GetDiagnostics_WithDiagnostics(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	// Mark as initialized to test the diagnostics retrieval path
	tool.handler.initialized.Store(true)
	// Pretend we have a running server
	tool.handler.cmd = exec.CommandContext(t.Context(), "true")

	// Manually set some diagnostics
	tool.handler.diagnostics["file:///test.go"] = []lspDiagnostic{
		{
			Range:    lspRange{Start: lspPosition{Line: 5, Character: 0}},
			Severity: 1,
			Message:  "test error",
		},
	}
	// Mark file as open to skip auto-open attempt
	tool.handler.openFiles["file:///test.go"] = 1

	ctx := t.Context()
	result, err := tool.handler.getDiagnostics(ctx, FileArgs{File: "/test.go"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "test error")
}

func TestProcessNotification_Diagnostics(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")

	// Create a diagnostic notification
	notification := map[string]any{
		"method": "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri": "file:///test.go",
			"diagnostics": []map[string]any{
				{
					"range": map[string]any{
						"start": map[string]any{"line": 10, "character": 0},
						"end":   map[string]any{"line": 10, "character": 5},
					},
					"severity": 1,
					"message":  "test diagnostic",
				},
			},
		},
	}

	data, err := json.Marshal(notification)
	require.NoError(t, err)

	tool.handler.processNotification(data)

	// Check that diagnostics were stored
	tool.handler.diagnosticsMu.RLock()
	diags, ok := tool.handler.diagnostics["file:///test.go"]
	tool.handler.diagnosticsMu.RUnlock()

	require.True(t, ok)
	require.Len(t, diags, 1)
	assert.Equal(t, "test diagnostic", diags[0].Message)
}

func TestLSPHandler_Stop_NotStarted(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	ctx := t.Context()

	// Should not error when stopping a non-started server
	err := tool.Stop(ctx)
	require.NoError(t, err)
}

func TestDetectLanguageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		expected string
	}{
		{"main.go", "go"},
		{"app.py", "python"},
		{"index.js", "javascript"},
		{"component.jsx", "javascriptreact"},
		{"app.ts", "typescript"},
		{"component.tsx", "typescriptreact"},
		{"lib.rs", "rust"},
		{"main.c", "c"},
		{"app.cpp", "cpp"},
		{"Main.java", "java"},
		{"app.rb", "ruby"},
		{"index.php", "php"},
		{"Program.cs", "csharp"},
		{"app.swift", "swift"},
		{"Main.kt", "kotlin"},
		{"config.yaml", "yaml"},
		{"config.yml", "yaml"},
		{"data.json", "json"},
		{"index.html", "html"},
		{"styles.css", "css"},
		{"script.sh", "shellscript"},
		{"README.md", "markdown"},
		{"Dockerfile", "dockerfile"},
		{"Makefile", "makefile"},
		{"unknown.xyz", "plaintext"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, detectLanguageID(tt.path))
		})
	}
}

func TestLSPTool_HandlesFile(t *testing.T) {
	t.Parallel()

	// Without file type filter (handles all)
	tool := NewLSPTool("gopls", nil, nil, "/tmp")
	assert.True(t, tool.HandlesFile("main.go"))
	assert.True(t, tool.HandlesFile("app.py"))
	assert.True(t, tool.HandlesFile("anything.txt"))

	// With file type filter
	toolFiltered := NewLSPTool("gopls", nil, nil, "/tmp")
	toolFiltered.SetFileTypes([]string{".go", ".mod"})
	assert.True(t, toolFiltered.HandlesFile("main.go"))
	assert.True(t, toolFiltered.HandlesFile("go.mod"))
	assert.False(t, toolFiltered.HandlesFile("app.py"))
	assert.False(t, toolFiltered.HandlesFile("index.js"))

	// Without leading dot in filter
	toolNoDot := NewLSPTool("gopls", nil, nil, "/tmp")
	toolNoDot.SetFileTypes([]string{"go", "py"})
	assert.True(t, toolNoDot.HandlesFile("main.go"))
	assert.True(t, toolNoDot.HandlesFile("app.py"))
	assert.False(t, toolNoDot.HandlesFile("index.js"))
}

func TestPathToURI(t *testing.T) {
	t.Parallel()

	// Absolute path
	uri := pathToURI("/home/user/project/main.go")
	assert.Equal(t, "file:///home/user/project/main.go", uri)
}

func TestLSPHandler_IsFileOpen(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")

	// Initially no files are open
	assert.False(t, tool.handler.isFileOpen("file:///test.go"))

	// Track a file as open
	tool.handler.openFilesMu.Lock()
	tool.handler.openFiles["file:///test.go"] = 1
	tool.handler.openFilesMu.Unlock()

	assert.True(t, tool.handler.isFileOpen("file:///test.go"))
	assert.False(t, tool.handler.isFileOpen("file:///other.go"))
}

func TestLSPHandler_DiagnosticsVersion(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", nil, nil, "/tmp")

	// Initial version should be 0
	assert.Equal(t, int64(0), tool.handler.diagnosticsVersion.Load())

	// Process a diagnostic notification
	notification := map[string]any{
		"method": "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri":         "file:///test.go",
			"diagnostics": []map[string]any{},
		},
	}

	data, err := json.Marshal(notification)
	require.NoError(t, err)

	tool.handler.processNotification(data)

	// Version should be incremented
	assert.Equal(t, int64(1), tool.handler.diagnosticsVersion.Load())
}

func TestPositionArgs_SimplifiedParameters(t *testing.T) {
	t.Parallel()

	// Test that we can unmarshal simple flat parameters
	jsonInput := `{"file": "/path/to/file.go", "line": 42, "character": 15}`

	var args PositionArgs
	err := json.Unmarshal([]byte(jsonInput), &args)
	require.NoError(t, err)

	assert.Equal(t, "/path/to/file.go", args.File)
	assert.Equal(t, 42, args.Line)
	assert.Equal(t, 15, args.Character)
}

func TestReferencesArgs_DefaultIncludeDeclaration(t *testing.T) {
	t.Parallel()

	// Test that include_declaration is optional and defaults appropriately
	jsonInput := `{"file": "/path/to/file.go", "line": 10, "character": 5}`

	var args ReferencesArgs
	err := json.Unmarshal([]byte(jsonInput), &args)
	require.NoError(t, err)

	// When not specified, pointer should be nil (default to true in handler)
	assert.Equal(t, "/path/to/file.go", args.File)
	assert.Equal(t, 10, args.Line)
	assert.Equal(t, 5, args.Character)
	assert.Nil(t, args.IncludeDeclaration) // nil means "use default (true)"

	// Test explicit false
	jsonInputFalse := `{"file": "/path/to/file.go", "line": 10, "character": 5, "include_declaration": false}`
	var argsFalse ReferencesArgs
	err = json.Unmarshal([]byte(jsonInputFalse), &argsFalse)
	require.NoError(t, err)
	assert.NotNil(t, argsFalse.IncludeDeclaration)
	assert.False(t, *argsFalse.IncludeDeclaration)
}

func TestWorkspaceSymbolsArgs_EmptyQuery(t *testing.T) {
	t.Parallel()

	// Empty query should be valid (returns all symbols)
	jsonInput := `{"query": ""}`

	var args WorkspaceSymbolsArgs
	err := json.Unmarshal([]byte(jsonInput), &args)
	require.NoError(t, err)

	assert.Empty(t, args.Query)
}

func TestApplyTextEdit_SingleLineReplacement(t *testing.T) {
	t.Parallel()

	lines := []string{"hello world", "foo bar", "baz qux"}
	edit := lspTextEdit{
		Range: lspRange{
			Start: lspPosition{Line: 1, Character: 4},
			End:   lspPosition{Line: 1, Character: 7},
		},
		NewText: "replaced",
	}

	result := applyTextEdit(lines, edit)
	assert.Equal(t, []string{"hello world", "foo replaced", "baz qux"}, result)
}

func TestApplyTextEdit_MultiLineReplacement(t *testing.T) {
	t.Parallel()

	lines := []string{"line 0", "line 1", "line 2", "line 3"}
	edit := lspTextEdit{
		Range: lspRange{
			Start: lspPosition{Line: 1, Character: 5},
			End:   lspPosition{Line: 2, Character: 5},
		},
		NewText: "REPLACED",
	}

	result := applyTextEdit(lines, edit)
	assert.Equal(t, []string{"line 0", "line REPLACED2", "line 3"}, result)
}

func TestApplyTextEdit_InsertNewLine(t *testing.T) {
	t.Parallel()

	lines := []string{"hello", "world"}
	edit := lspTextEdit{
		Range: lspRange{
			Start: lspPosition{Line: 0, Character: 5},
			End:   lspPosition{Line: 0, Character: 5},
		},
		NewText: "\nnew line\n",
	}

	result := applyTextEdit(lines, edit)
	assert.Equal(t, []string{"hello", "new line", "", "world"}, result)
}

func TestFormatCodeActions_Empty(t *testing.T) {
	t.Parallel()

	result := formatCodeActions("/path/to/file.go", 42, []byte("[]"))
	assert.Equal(t, "No code actions available for /path/to/file.go:42", result)
}

func TestFormatCodeActions_WithActions(t *testing.T) {
	t.Parallel()

	actions := `[{"title": "Add import", "kind": "quickfix", "isPreferred": true}, {"title": "Extract function", "kind": "refactor.extract"}]`
	result := formatCodeActions("/path/to/file.go", 42, []byte(actions))

	assert.Contains(t, result, "Available code actions for /path/to/file.go:42:")
	assert.Contains(t, result, "[quickfix] Add import (preferred)")
	assert.Contains(t, result, "[refactor.extract] Extract function")
}

func TestFormatIncomingCalls_Empty(t *testing.T) {
	t.Parallel()

	result := formatIncomingCalls("myFunc", []byte("[]"))
	assert.Equal(t, "No incoming calls to 'myFunc'", result)
}

func TestFormatIncomingCalls_WithCalls(t *testing.T) {
	t.Parallel()

	calls := `[{
		"from": {
			"name": "caller",
			"kind": 12,
			"uri": "file:///path/to/caller.go",
			"range": {"start": {"line": 10, "character": 0}, "end": {"line": 10, "character": 10}},
			"selectionRange": {"start": {"line": 10, "character": 0}, "end": {"line": 10, "character": 10}}
		},
		"fromRanges": [{"start": {"line": 15, "character": 0}, "end": {"line": 15, "character": 10}}]
	}]`

	result := formatIncomingCalls("myFunc", []byte(calls))
	assert.Contains(t, result, "Incoming calls to 'myFunc':")
	assert.Contains(t, result, "Function caller")
	assert.Contains(t, result, "/path/to/caller.go:11")
}

func TestFormatOutgoingCalls_Empty(t *testing.T) {
	t.Parallel()

	result := formatOutgoingCalls("myFunc", []byte("[]"))
	assert.Equal(t, "No outgoing calls from 'myFunc'", result)
}

func TestFormatTypeHierarchy_Empty(t *testing.T) {
	t.Parallel()

	result := formatTypeHierarchy("MyType", "Supertypes", []byte("[]"))
	assert.Equal(t, "No supertypes for 'MyType'", result)
}

func TestFormatTypeHierarchy_WithTypes(t *testing.T) {
	t.Parallel()

	types := `[{
		"name": "ParentType",
		"kind": 5,
		"uri": "file:///path/to/parent.go",
		"range": {"start": {"line": 5, "character": 0}, "end": {"line": 5, "character": 10}},
		"selectionRange": {"start": {"line": 5, "character": 0}, "end": {"line": 5, "character": 10}}
	}]`

	result := formatTypeHierarchy("MyType", "Supertypes", []byte(types))
	assert.Contains(t, result, "Supertypes of 'MyType':")
	assert.Contains(t, result, "Class ParentType")
	assert.Contains(t, result, "/path/to/parent.go:6")
}

func TestFormatSignatureHelp_Empty(t *testing.T) {
	t.Parallel()

	help := lspSignatureHelp{Signatures: []lspSignatureInformation{}}
	result := formatSignatureHelp(help)
	assert.Equal(t, "No signature help available", result)
}

func TestFormatSignatureHelp_WithSignature(t *testing.T) {
	t.Parallel()

	help := lspSignatureHelp{
		Signatures: []lspSignatureInformation{
			{
				Label: "func(a int, b string) error",
				Parameters: []lspParameterInformation{
					{Label: "a int"},
					{Label: "b string"},
				},
			},
		},
		ActiveSignature: 0,
		ActiveParameter: 1,
	}

	result := formatSignatureHelp(help)
	assert.Contains(t, result, "Function: func(a int, b string) error")
	assert.Contains(t, result, "Parameters:")
	assert.Contains(t, result, "1. a int")
	assert.Contains(t, result, "2. b string [ACTIVE]")
	assert.Contains(t, result, "Currently typing parameter 2 of 2")
}

func TestFormatInlayHints_Empty(t *testing.T) {
	t.Parallel()

	result := formatInlayHints("/path/to/file.go", 1, 100, []lspInlayHint{})
	assert.Equal(t, "No inlay hints for /path/to/file.go:1-100", result)
}

func TestFormatInlayHints_WithHints(t *testing.T) {
	t.Parallel()

	hints := []lspInlayHint{
		{
			Position: lspPosition{Line: 9, Character: 5},
			Label:    ": string",
			Kind:     1,
		},
		{
			Position: lspPosition{Line: 14, Character: 10},
			Label:    "ctx:",
			Kind:     2,
		},
	}

	result := formatInlayHints("/path/to/file.go", 1, 50, hints)
	assert.Contains(t, result, "Inlay hints for /path/to/file.go:1-50:")
	assert.Contains(t, result, "Line 10, Col 6: ': string' (type)")
	assert.Contains(t, result, "Line 15, Col 11: 'ctx:' (parameter)")
}

func TestInlayHintKindName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "type", inlayHintKindName(1))
	assert.Equal(t, "parameter", inlayHintKindName(2))
	assert.Equal(t, "hint", inlayHintKindName(0))
	assert.Equal(t, "hint", inlayHintKindName(99))
}

func TestCapabilityStatus(t *testing.T) {
	t.Parallel()

	// nil returns No
	assert.Equal(t, "No", capabilityStatus(nil))

	// bool true returns Yes
	assert.Equal(t, "Yes", capabilityStatus(true))

	// bool false returns No
	assert.Equal(t, "No", capabilityStatus(false))

	// Non-nil, non-bool (options object) returns Yes
	assert.Equal(t, "Yes", capabilityStatus(map[string]any{"option": true}))
	assert.Equal(t, "Yes", capabilityStatus("some string"))
	assert.Equal(t, "Yes", capabilityStatus(123))
}

func TestLSPHandler_Workspace(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("gopls", []string{"-remote=auto"}, nil, "/tmp/project")
	tool.SetFileTypes([]string{".go", ".mod"})

	// Mark as initialized and set server info/capabilities
	tool.handler.initialized.Store(true)
	tool.handler.cmd = exec.CommandContext(t.Context(), "true")
	tool.handler.serverInfo = &lspServerInfo{
		Name:    "gopls",
		Version: "v0.14.0",
	}
	tool.handler.capabilities = &lspServerCapabilities{
		HoverProvider:              true,
		DefinitionProvider:         true,
		ReferencesProvider:         true,
		DocumentSymbolProvider:     true,
		WorkspaceSymbolProvider:    true,
		CodeActionProvider:         map[string]any{"codeActionKinds": []string{"quickfix"}},
		DocumentFormattingProvider: true,
		RenameProvider:             true,
		CallHierarchyProvider:      true,
		TypeHierarchyProvider:      nil, // Not supported
		ImplementationProvider:     true,
		SignatureHelpProvider:      map[string]any{},
		InlayHintProvider:          false, // Explicitly disabled
	}

	ctx := t.Context()
	result, err := tool.handler.workspace(ctx, WorkspaceArgs{})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	// Check workspace information
	assert.Contains(t, result.Output, "Workspace Information:")
	assert.Contains(t, result.Output, "Root: /tmp/project")
	assert.Contains(t, result.Output, "LSP Command: gopls")
	assert.Contains(t, result.Output, "Server: gopls v0.14.0")
	assert.Contains(t, result.Output, "File types: .go, .mod")

	// Check capabilities
	assert.Contains(t, result.Output, "Available Capabilities:")
	assert.Contains(t, result.Output, "Hover: Yes")
	assert.Contains(t, result.Output, "Go to Definition: Yes")
	assert.Contains(t, result.Output, "Type Hierarchy: No") // nil capability
	assert.Contains(t, result.Output, "Inlay Hints: No")    // false capability
}
