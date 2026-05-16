package lsp

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

const (
	ToolNameLSPWorkspace        = "lsp_workspace"
	ToolNameLSPHover            = "lsp_hover"
	ToolNameLSPDefinition       = "lsp_definition"
	ToolNameLSPReferences       = "lsp_references"
	ToolNameLSPDocumentSymbols  = "lsp_document_symbols"
	ToolNameLSPWorkspaceSymbols = "lsp_workspace_symbols"
	ToolNameLSPDiagnostics      = "lsp_diagnostics"
	ToolNameLSPRename           = "lsp_rename"
	ToolNameLSPCodeActions      = "lsp_code_actions"
	ToolNameLSPFormat           = "lsp_format"
	ToolNameLSPCallHierarchy    = "lsp_call_hierarchy"
	ToolNameLSPTypeHierarchy    = "lsp_type_hierarchy"
	ToolNameLSPImplementations  = "lsp_implementations"
	ToolNameLSPSignatureHelp    = "lsp_signature_help"
	ToolNameLSPInlayHints       = "lsp_inlay_hints"
)

// Tool implements tools.ToolSet for connecting to any LSP server.
// It provides stateless code intelligence tools that automatically manage
// the LSP server lifecycle and document state.
type Tool struct {
	handler *lspHandler
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Startable    = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

type lspHandler struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	cancel      context.CancelFunc // cancels the process-lifetime context
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	initialized atomic.Bool
	requestID   atomic.Int64

	// supervisor manages process lifecycle (start, watcher goroutine,
	// auto-restart on crash, graceful Stop). Per-request methods consult
	// it via ensureInitialized to lazy-start on first use.
	supervisor *lifecycle.Supervisor

	// toolsChangedHandler is called after the supervisor's Connect has
	// populated h.capabilities, so the runtime can re-fetch the (now
	// capability-filtered) tool list. nil until SetToolsChangedHandler is
	// called.
	toolsChangedHandler func()

	// Configuration
	command    string
	args       []string
	env        []string
	workingDir string
	fileTypes  []string // Empty = all files

	// State tracking
	diagnosticsMu      sync.RWMutex
	diagnostics        map[string][]lspDiagnostic
	diagnosticsVersion atomic.Int64
	openFilesMu        sync.RWMutex
	openFiles          map[string]int // URI -> version

	// Server info from initialization
	serverInfo   *lspServerInfo
	capabilities *lspServerCapabilities
}

// lspServerInfo holds information about the LSP server.
type lspServerInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// lspServerCapabilities holds the capabilities reported by the LSP server.
type lspServerCapabilities struct {
	TextDocumentSync           any `json:"textDocumentSync,omitempty"`
	HoverProvider              any `json:"hoverProvider,omitempty"`
	CompletionProvider         any `json:"completionProvider,omitempty"`
	DefinitionProvider         any `json:"definitionProvider,omitempty"`
	ReferencesProvider         any `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider     any `json:"documentSymbolProvider,omitempty"`
	WorkspaceSymbolProvider    any `json:"workspaceSymbolProvider,omitempty"`
	CodeActionProvider         any `json:"codeActionProvider,omitempty"`
	DocumentFormattingProvider any `json:"documentFormattingProvider,omitempty"`
	RenameProvider             any `json:"renameProvider,omitempty"`
	CallHierarchyProvider      any `json:"callHierarchyProvider,omitempty"`
	TypeHierarchyProvider      any `json:"typeHierarchyProvider,omitempty"`
	ImplementationProvider     any `json:"implementationProvider,omitempty"`
	SignatureHelpProvider      any `json:"signatureHelpProvider,omitempty"`
	InlayHintProvider          any `json:"inlayHintProvider,omitempty"`
}

// LSP message types
type lspRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type lspNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type lspResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspError       `json:"error,omitempty"`
}

type lspError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// PositionArgs is the base for all position-based tool arguments.
type PositionArgs struct {
	File      string `json:"file" jsonschema:"Absolute path to the source file"`
	Line      int    `json:"line" jsonschema:"Line number (1-based)"`
	Character int    `json:"character" jsonschema:"Character position on the line (1-based)"`
}

// ReferencesArgs extends PositionArgs with an include_declaration option.
type ReferencesArgs struct {
	PositionArgs

	IncludeDeclaration *bool `json:"include_declaration,omitempty" jsonschema:"Include the declaration in results (default: true)"`
}

// FileArgs is for tools that only need a file path.
type FileArgs struct {
	File string `json:"file" jsonschema:"Absolute path to the source file"`
}

// WorkspaceSymbolsArgs for searching symbols across the workspace.
type WorkspaceSymbolsArgs struct {
	Query string `json:"query" jsonschema:"Search query to filter symbols (supports fuzzy matching)"`
}

// RenameArgs extends PositionArgs with the new name.
type RenameArgs struct {
	PositionArgs

	NewName string `json:"new_name" jsonschema:"The new name for the symbol"`
}

// CodeActionsArgs for getting available code actions.
type CodeActionsArgs struct {
	File      string `json:"file" jsonschema:"Absolute path to the source file"`
	StartLine int    `json:"start_line" jsonschema:"Start line of the range (1-based)"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"End line of the range (1-based, defaults to start_line)"`
}

// CallHierarchyArgs for getting call hierarchy.
type CallHierarchyArgs struct {
	PositionArgs

	Direction string `json:"direction" jsonschema:"Direction: 'incoming' (who calls this) or 'outgoing' (what this calls)"`
}

// TypeHierarchyArgs for getting type hierarchy.
type TypeHierarchyArgs struct {
	PositionArgs

	Direction string `json:"direction" jsonschema:"Direction: 'supertypes' (parent types) or 'subtypes' (child types)"`
}

// InlayHintsArgs for getting inlay hints.
type InlayHintsArgs struct {
	File      string `json:"file" jsonschema:"Absolute path to the source file"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"Start line of range (1-based, default: 1)"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"End line of range (1-based, default: end of file)"`
}

// LSP result types
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspHover struct {
	Contents any       `json:"contents"`
	Range    *lspRange `json:"range,omitempty"`
}

type lspSymbolInformation struct {
	Name          string      `json:"name"`
	Kind          int         `json:"kind"`
	Location      lspLocation `json:"location"`
	ContainerName string      `json:"containerName,omitempty"`
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children,omitempty"`
}

type lspDiagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity,omitempty"`
	Code     any      `json:"code,omitempty"`
	Source   string   `json:"source,omitempty"`
	Message  string   `json:"message"`
}

type lspWorkspaceEdit struct {
	Changes         map[string][]lspTextEdit `json:"changes,omitempty"`
	DocumentChanges []lspTextDocumentEdit    `json:"documentChanges,omitempty"`
}

type lspTextEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

type lspTextDocumentEdit struct {
	TextDocument lspVersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []lspTextEdit                      `json:"edits"`
}

type lspVersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version *int   `json:"version"`
}

type lspCodeAction struct {
	Title       string            `json:"title"`
	Kind        string            `json:"kind,omitempty"`
	Diagnostics []lspDiagnostic   `json:"diagnostics,omitempty"`
	IsPreferred bool              `json:"isPreferred,omitempty"`
	Edit        *lspWorkspaceEdit `json:"edit,omitempty"`
	Command     *lspCommand       `json:"command,omitempty"`
}

type lspCommand struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments,omitempty"`
}

type lspCallHierarchyItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail,omitempty"`
	URI            string   `json:"uri"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
}

type lspCallHierarchyIncomingCall struct {
	From       lspCallHierarchyItem `json:"from"`
	FromRanges []lspRange           `json:"fromRanges"`
}

type lspCallHierarchyOutgoingCall struct {
	To         lspCallHierarchyItem `json:"to"`
	FromRanges []lspRange           `json:"fromRanges"`
}

type lspTypeHierarchyItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail,omitempty"`
	URI            string   `json:"uri"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
}

type lspSignatureHelp struct {
	Signatures      []lspSignatureInformation `json:"signatures"`
	ActiveSignature int                       `json:"activeSignature,omitempty"`
	ActiveParameter int                       `json:"activeParameter,omitempty"`
}

type lspSignatureInformation struct {
	Label           string                    `json:"label"`
	Documentation   any                       `json:"documentation,omitempty"`
	Parameters      []lspParameterInformation `json:"parameters,omitempty"`
	ActiveParameter int                       `json:"activeParameter,omitempty"`
}

type lspParameterInformation struct {
	Label         any `json:"label"`
	Documentation any `json:"documentation,omitempty"`
}

type lspInlayHint struct {
	Position     lspPosition `json:"position"`
	Label        any         `json:"label"`
	Kind         int         `json:"kind,omitempty"`
	PaddingLeft  bool        `json:"paddingLeft,omitempty"`
	PaddingRight bool        `json:"paddingRight,omitempty"`
}

// NewLSPTool creates a new LSP tool that connects to an LSP server.
//
// The optional policy lets callers tune restart/backoff behaviour. When
// the zero value is passed the supervisor uses its built-in defaults
// (RestartOnFailure, 5 attempts, 1s..32s backoff). Internal callbacks
// (OnDisconnect, Logger) are always set by the constructor.
func NewLSPTool(command string, args, env []string, workingDir string, policy ...lifecycle.Policy) *Tool {
	h := &lspHandler{
		command:     command,
		args:        args,
		env:         env,
		workingDir:  workingDir,
		diagnostics: make(map[string][]lspDiagnostic),
		openFiles:   make(map[string]int),
	}
	base := lifecycle.Policy{}
	if len(policy) > 0 {
		base = policy[0]
	}
	base.Logger = slog.With("component", "lsp.supervisor", "command", command)
	base.OnDisconnect = func(error) {
		// Reset diagnostics on disconnect: the next server may not
		// re-emit them and stale data is worse than nothing.
		h.diagnosticsMu.Lock()
		h.diagnostics = make(map[string][]lspDiagnostic)
		h.diagnosticsMu.Unlock()
	}
	h.supervisor = lifecycle.New("lsp/"+command, &lspConnector{h: h}, base)
	return &Tool{handler: h}
}

// SetFileTypes sets the file types (extensions) that this LSP server handles.
func (t *Tool) SetFileTypes(fileTypes []string) {
	t.handler.fileTypes = fileTypes
}

// WorkingDir returns the working directory of the LSP server process.
// This is intended for testing only.
func (t *Tool) WorkingDir() string {
	return t.handler.workingDir
}

// HandlesFile checks if this LSP handles the given file based on its extension.
func (t *Tool) HandlesFile(path string) bool {
	return t.handler.handlesFile(path)
}

func (t *Tool) Start(ctx context.Context) error {
	return t.handler.supervisor.Start(ctx)
}

func (t *Tool) Stop(ctx context.Context) error {
	return t.handler.supervisor.Stop(ctx)
}

// State returns a snapshot of the underlying supervisor's lifecycle state,
// suitable for the /tools dialog and lifecycle log messages.
func (t *Tool) State() lifecycle.StateInfo {
	return t.handler.supervisor.State()
}

// Restart brings the LSP server back up regardless of state. Failed or
// Stopped supervisors are recovered via Start; otherwise the current
// session is dropped and we wait for the supervisor to reconnect.
// Blocks up to 35s (matching the MCP toolset).
func (t *Tool) Restart(ctx context.Context) error {
	if t.handler.supervisor.State().State.IsTerminal() {
		return t.handler.supervisor.Start(ctx)
	}
	return t.handler.supervisor.RestartAndWait(ctx, 35*time.Second)
}

// Kind returns the user-facing classification of this toolset. Used by
// status surfaces such as the /tools dialog so they can label the
// toolset without leaking Go type names.
func (t *Tool) Kind() string { return "LSP" }

// Name returns the basename of the configured command ("gopls",
// "rust-analyzer", …). It's the most useful identifier in the absence
// of a YAML name: field on LSP toolsets, and lets the /tools dialog
// distinguish multiple language servers in the same agent.
func (t *Tool) Name() string {
	return filepath.Base(t.handler.command)
}

func (t *Tool) Instructions() string {
	return `# LSP Code Intelligence Tools

Stateless code intelligence tools via Language Server Protocol. Just provide file path and position.

## Getting Started

Use lsp_workspace at the start of every session to learn about the workspace and available capabilities.

## Read Workflow

1. **Find symbols**: Use lsp_workspace_symbols for fuzzy search. Example: lsp_workspace_symbols({"query":"server"})
2. **Understand file structure**: Use lsp_document_symbols for a hierarchical symbol list
3. **Inspect symbols**: Use lsp_hover for type signatures and documentation
4. **Navigate**: Use lsp_definition to jump to definitions
5. **Understand dependencies**: Use lsp_call_hierarchy (outgoing) or lsp_type_hierarchy (supertypes)

## Edit Workflow

1. **Read first**: Follow the Read Workflow to understand relevant code
2. **Find references**: Before modifying any symbol definition, you MUST use lsp_references to find all usages. Example: lsp_references({"file":"/path/to/file.go", "line": 42, "character": 15})
3. **Check implementations**: Before modifying interfaces, use lsp_implementations to find all concrete implementations
4. **Make edits**: Apply all planned changes
5. **Check errors**: After every modification, you MUST call lsp_diagnostics on edited files. Use lsp_code_actions for suggested fixes. Ignore irrelevant hint/info diagnostics
6. **Format**: Once error-free, use lsp_format for consistent style

## Position Format

Line and character positions are 1-based.`
}

// WorkspaceArgs is empty - the workspace tool takes no arguments.
type WorkspaceArgs struct{}

// lspTool is a shorthand for constructing a tools.Tool with common LSP defaults.
func lspTool(name, title, description string, readOnly bool, params any, handler tools.ToolHandler) tools.Tool {
	return tools.Tool{
		Name:        name,
		Category:    "lsp",
		Description: description,
		Parameters:  params,
		Handler:     handler,
		Annotations: tools.ToolAnnotations{
			Title:        title,
			ReadOnlyHint: readOnly,
		},
	}
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
	h := t.handler
	all := allLSPTools(h)

	caps := h.snapshotCapabilities()
	if caps == nil {
		// The server has not been initialised yet. Advertise the full
		// catalogue: the runtime will refresh after the first Connect
		// completes (via toolsChangedHandler), at which point we filter.
		return all, nil
	}
	return filterByCapabilities(all, caps), nil
}

// snapshotCapabilities returns the LSP server capabilities under h.mu,
// or nil if the server has not yet completed initialize.
func (h *lspHandler) snapshotCapabilities() *lspServerCapabilities {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.capabilities
}

// SetToolsChangedHandler registers a callback that is invoked after the
// supervisor reaches Ready and the server's capability matrix becomes
// available. The runtime uses this to re-query Tools() and pick up the
// capability-filtered list.
func (t *Tool) SetToolsChangedHandler(handler func()) {
	t.handler.mu.Lock()
	t.handler.toolsChangedHandler = handler
	t.handler.mu.Unlock()
}

// allLSPTools returns the full catalogue of LSP tools backed by h. It is
// extracted from Tools() so that capability-aware filtering can decide
// which entries to emit without rebuilding the slice each call.
func allLSPTools(h *lspHandler) []tools.Tool {
	return []tools.Tool{
		lspTool(ToolNameLSPWorkspace, "Get Workspace Info",
			`Get workspace info and LSP server capabilities. Use at session start to discover available features. Takes no arguments.`,
			true, tools.MustSchemaFor[WorkspaceArgs](), tools.NewHandler(h.workspace)),
		lspTool(ToolNameLSPHover, "Get Symbol Info",
			`Get type signature, documentation, and hover info for a symbol at a given position.`,
			true, tools.MustSchemaFor[PositionArgs](), tools.NewHandler(h.hover)),
		lspTool(ToolNameLSPDefinition, "Go to Definition",
			`Find the definition location of a symbol. Returns file path and line number.`,
			true, tools.MustSchemaFor[PositionArgs](), tools.NewHandler(h.definition)),
		lspTool(ToolNameLSPReferences, "Find References",
			`Find all references to a symbol across the codebase. IMPORTANT: You MUST use this before modifying any symbol definition. Set include_declaration to false to exclude the definition itself.`,
			true, tools.MustSchemaFor[ReferencesArgs](), tools.NewHandler(h.references)),
		lspTool(ToolNameLSPDocumentSymbols, "List File Symbols",
			`List all symbols (functions, types, methods, variables, etc.) defined in a file as a hierarchical list.`,
			true, tools.MustSchemaFor[FileArgs](), tools.NewHandler(h.documentSymbols)),
		lspTool(ToolNameLSPWorkspaceSymbols, "Search Workspace Symbols",
			`Search for symbols across the workspace using fuzzy matching. Primary tool for locating symbols.`,
			true, tools.MustSchemaFor[WorkspaceSymbolsArgs](), tools.NewHandler(h.workspaceSymbols)),
		lspTool(ToolNameLSPDiagnostics, "Get Diagnostics",
			`Get compiler errors, warnings, and hints for a file. IMPORTANT: You MUST call this after every code modification on edited files. Use lsp_code_actions for suggested fixes.`,
			true, tools.MustSchemaFor[FileArgs](), tools.NewHandler(h.getDiagnostics)),
		lspTool(ToolNameLSPRename, "Rename Symbol",
			`Rename a symbol across the entire workspace. WRITE operation - modifies files on disk. Run lsp_diagnostics on modified files afterward.`,
			false, tools.MustSchemaFor[RenameArgs](), tools.NewHandler(h.rename)),
		lspTool(ToolNameLSPCodeActions, "Get Code Actions",
			`Get available code actions (quick fixes, refactorings) for a line or range. Use after lsp_diagnostics reports errors.`,
			true, tools.MustSchemaFor[CodeActionsArgs](), tools.NewHandler(h.codeActions)),
		lspTool(ToolNameLSPFormat, "Format File",
			`Format a file according to language standards. WRITE operation - modifies the file on disk. Only format after lsp_diagnostics reports no errors.`,
			false, tools.MustSchemaFor[FileArgs](), tools.NewHandler(h.format)),
		lspTool(ToolNameLSPCallHierarchy, "Call Hierarchy",
			`Analyze the call hierarchy of a function or method. Direction: 'incoming' (who calls this) or 'outgoing' (what this calls).`,
			true, tools.MustSchemaFor[CallHierarchyArgs](), tools.NewHandler(h.callHierarchy)),
		lspTool(ToolNameLSPTypeHierarchy, "Type Hierarchy",
			`Analyze the type hierarchy. Direction: 'supertypes' (parent types) or 'subtypes' (child types).`,
			true, tools.MustSchemaFor[TypeHierarchyArgs](), tools.NewHandler(h.typeHierarchy)),
		lspTool(ToolNameLSPImplementations, "Find Implementations",
			`Find all concrete implementations of an interface or abstract method. IMPORTANT: You MUST use this before modifying interfaces to find all implementations needing updates.`,
			true, tools.MustSchemaFor[PositionArgs](), tools.NewHandler(h.implementations)),
		lspTool(ToolNameLSPSignatureHelp, "Signature Help",
			`Get function signature and parameter information at a call site. Position the cursor inside a function call's parentheses.`,
			true, tools.MustSchemaFor[PositionArgs](), tools.NewHandler(h.signatureHelp)),
		lspTool(ToolNameLSPInlayHints, "Inlay Hints",
			`Get inlay hints (type annotations, parameter names) for a file or line range. Omit start_line/end_line to get hints for the entire file.`,
			true, tools.MustSchemaFor[InlayHintsArgs](), tools.NewHandler(h.inlayHints)),
	}
}

// filterByCapabilities returns only the entries from all whose required
// LSP server capability is advertised by caps.
//
// Tools without a 1:1 capability mapping (lsp_workspace, lsp_diagnostics)
// are always retained: lsp_workspace surfaces the capability matrix
// itself, and diagnostics arrive as server-pushed notifications and so
// are usable even when no document* provider is advertised.
func filterByCapabilities(all []tools.Tool, caps *lspServerCapabilities) []tools.Tool {
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		if !capabilitySupports(t.Name, caps) {
			slog.Debug("LSP tool hidden: server does not advertise capability", "tool", t.Name)
			continue
		}
		out = append(out, t)
	}
	return out
}

// capabilitySupports reports whether the LSP server with the given
// capabilities advertises support for the tool name.
//
// LSP capability values are loosely typed: each provider field is either
// missing, false, true, or an options object. We treat missing/false as
// "unsupported" and any non-false value as "supported".
func capabilitySupports(toolName string, caps *lspServerCapabilities) bool {
	if caps == nil {
		return true
	}
	switch toolName {
	case ToolNameLSPWorkspace, ToolNameLSPDiagnostics:
		return true
	case ToolNameLSPHover:
		return isProviderEnabled(caps.HoverProvider)
	case ToolNameLSPDefinition:
		return isProviderEnabled(caps.DefinitionProvider)
	case ToolNameLSPReferences:
		return isProviderEnabled(caps.ReferencesProvider)
	case ToolNameLSPDocumentSymbols:
		return isProviderEnabled(caps.DocumentSymbolProvider)
	case ToolNameLSPWorkspaceSymbols:
		return isProviderEnabled(caps.WorkspaceSymbolProvider)
	case ToolNameLSPCodeActions:
		return isProviderEnabled(caps.CodeActionProvider)
	case ToolNameLSPFormat:
		return isProviderEnabled(caps.DocumentFormattingProvider)
	case ToolNameLSPRename:
		return isProviderEnabled(caps.RenameProvider)
	case ToolNameLSPCallHierarchy:
		return isProviderEnabled(caps.CallHierarchyProvider)
	case ToolNameLSPTypeHierarchy:
		return isProviderEnabled(caps.TypeHierarchyProvider)
	case ToolNameLSPImplementations:
		return isProviderEnabled(caps.ImplementationProvider)
	case ToolNameLSPSignatureHelp:
		return isProviderEnabled(caps.SignatureHelpProvider)
	case ToolNameLSPInlayHints:
		return isProviderEnabled(caps.InlayHintProvider)
	default:
		// Unknown tool name: be permissive rather than silently hiding
		// future tools that haven't been added to this switch yet.
		return true
	}
}

// isProviderEnabled returns true when an LSP capability value advertises
// support: any non-nil, non-false value (including options objects) is
// considered "yes".
func isProviderEnabled(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}

// lspHandler implementation
//
// Process lifecycle (spawn, watch, auto-restart, graceful close) lives in
// lspConnector / lspSession (see lsp_lifecycle.go). The handler is
// responsible for the per-request JSON-RPC protocol and the persistent
// state (open files, diagnostics) that survives reconnects.

func (h *lspHandler) ensureInitialized() error {
	// Fast path: rely on the atomic initialized flag. lspConnector.Connect
	// only sets initialized=true after publishing h.cmd / h.stdin /
	// h.stdout under h.mu, and lspSession.Close clears initialized BEFORE
	// nilling those fields, so observing initialized=true here implies
	// the per-request methods can safely take h.mu and find a live
	// session. We deliberately do NOT read h.cmd here: that field is
	// guarded by h.mu, and reading it without the lock would race with
	// Connect/Close.
	if h.initialized.Load() {
		return nil
	}

	// Lazy-start through the supervisor. Concurrent ensureInitialized
	// callers serialize inside Supervisor.Start.
	if !h.supervisor.IsReady() {
		if err := h.supervisor.Start(context.Background()); err != nil {
			return fmt.Errorf("failed to start LSP server: %w", err)
		}
	}

	// After Start returns, Connect has populated h.cmd and run the
	// initialize+initialized handshake under h.mu. Verify under the
	// lock to avoid races with concurrent disconnect.
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.initialized.Load() || h.cmd == nil {
		return lifecycle.ErrNotStarted
	}
	return nil
}

// initializeLocked performs the LSP initialize/initialized handshake.
// The caller must hold h.mu and the process must be running.
func (h *lspHandler) initializeLocked() error {
	rootURI := "file://" + h.workingDir

	result, err := h.sendRequestLocked("initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"hover":              map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"definition":         map[string]any{},
				"references":         map[string]any{},
				"implementation":     map[string]any{},
				"documentSymbol":     map[string]any{},
				"publishDiagnostics": map[string]any{},
				"rename":             map[string]any{"prepareSupport": true},
				"codeAction": map[string]any{
					"codeActionLiteralSupport": map[string]any{
						"codeActionKind": map[string]any{
							"valueSet": []string{"quickfix", "refactor", "refactor.extract", "refactor.inline", "refactor.rewrite", "source", "source.organizeImports"},
						},
					},
				},
				"formatting":    map[string]any{},
				"callHierarchy": map[string]any{"dynamicRegistration": true},
				"typeHierarchy": map[string]any{"dynamicRegistration": true},
				"signatureHelp": map[string]any{
					"signatureInformation": map[string]any{
						"documentationFormat":  []string{"markdown", "plaintext"},
						"parameterInformation": map[string]any{"labelOffsetSupport": true},
					},
				},
				"inlayHint": map[string]any{"dynamicRegistration": true},
			},
			"workspace": map[string]any{
				"symbol":        map[string]any{},
				"applyEdit":     true,
				"workspaceEdit": map[string]any{"documentChanges": true},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to initialize LSP: %w", err)
	}

	var initResult struct {
		Capabilities lspServerCapabilities `json:"capabilities"`
		ServerInfo   *lspServerInfo        `json:"serverInfo,omitempty"`
	}
	if err := json.Unmarshal(result, &initResult); err != nil {
		slog.Debug("Failed to parse initialize result", "error", err)
	} else {
		h.capabilities = &initResult.Capabilities
		h.serverInfo = initResult.ServerInfo
	}

	if err := h.sendNotificationLocked("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("failed to send initialized notification: %w", err)
	}

	h.initialized.Store(true)
	slog.Debug("LSP server initialized", "rootUri", rootURI)
	return nil
}

// prepareFileRequest handles common setup for file-based requests
func (h *lspHandler) prepareFileRequest(ctx context.Context, file string) (string, error) {
	if err := h.ensureInitialized(); err != nil {
		return "", fmt.Errorf("LSP initialization failed: %w", err)
	}
	uri := pathToURI(file)
	if err := h.openFileOnDemand(ctx, uri); err != nil {
		slog.DebugContext(ctx, "Failed to auto-open file", "file", file, "error", err)
	}
	return uri, nil
}

// Tool handler implementations

func (h *lspHandler) workspace(ctx context.Context, _ WorkspaceArgs) (*tools.ToolCallResult, error) {
	if err := h.ensureInitialized(); err != nil {
		return tools.ResultError(fmt.Sprintf("LSP initialization failed: %s", err)), nil
	}

	var result strings.Builder
	result.WriteString("Workspace Information:\n")
	fmt.Fprintf(&result, "- Root: %s\n", h.workingDir)
	fmt.Fprintf(&result, "- LSP Command: %s\n", h.command)

	if h.serverInfo != nil {
		if h.serverInfo.Name != "" {
			serverStr := h.serverInfo.Name
			if h.serverInfo.Version != "" {
				serverStr += " " + h.serverInfo.Version
			}
			fmt.Fprintf(&result, "- Server: %s\n", serverStr)
		}
	}

	if len(h.fileTypes) > 0 {
		fmt.Fprintf(&result, "- File types: %s\n", strings.Join(h.fileTypes, ", "))
	} else {
		result.WriteString("- File types: all\n")
	}

	fmt.Fprintf(&result, "\nAvailable Capabilities:\n")
	if h.capabilities != nil {
		fmt.Fprintf(&result, "- Hover: %s\n", capabilityStatus(h.capabilities.HoverProvider))
		fmt.Fprintf(&result, "- Go to Definition: %s\n", capabilityStatus(h.capabilities.DefinitionProvider))
		fmt.Fprintf(&result, "- Find References: %s\n", capabilityStatus(h.capabilities.ReferencesProvider))
		fmt.Fprintf(&result, "- Find Implementations: %s\n", capabilityStatus(h.capabilities.ImplementationProvider))
		fmt.Fprintf(&result, "- Document Symbols: %s\n", capabilityStatus(h.capabilities.DocumentSymbolProvider))
		fmt.Fprintf(&result, "- Workspace Symbols: %s\n", capabilityStatus(h.capabilities.WorkspaceSymbolProvider))
		fmt.Fprintf(&result, "- Code Actions: %s\n", capabilityStatus(h.capabilities.CodeActionProvider))
		fmt.Fprintf(&result, "- Formatting: %s\n", capabilityStatus(h.capabilities.DocumentFormattingProvider))
		fmt.Fprintf(&result, "- Rename: %s\n", capabilityStatus(h.capabilities.RenameProvider))
		fmt.Fprintf(&result, "- Call Hierarchy: %s\n", capabilityStatus(h.capabilities.CallHierarchyProvider))
		fmt.Fprintf(&result, "- Type Hierarchy: %s\n", capabilityStatus(h.capabilities.TypeHierarchyProvider))
		fmt.Fprintf(&result, "- Signature Help: %s\n", capabilityStatus(h.capabilities.SignatureHelpProvider))
		fmt.Fprintf(&result, "- Inlay Hints: %s\n", capabilityStatus(h.capabilities.InlayHintProvider))
	} else {
		fmt.Fprintf(&result, "- (capabilities not available)\n")
	}

	return tools.ResultSuccess(result.String()), nil
}

// capabilityStatus returns "Yes" or "No" based on whether a capability is enabled.
func capabilityStatus(capability any) string {
	if capability == nil {
		return "No"
	}
	switch v := capability.(type) {
	case bool:
		if v {
			return "Yes"
		}
		return "No"
	default:
		// Non-nil, non-bool means the capability is available (could be options object)
		return "Yes"
	}
}

func (h *lspHandler) hover(ctx context.Context, args PositionArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": args.Line - 1, "character": args.Character - 1},
	}

	result, err := h.sendRequestLocked("textDocument/hover", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Hover request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" {
		return tools.ResultSuccess("No information available at this position"), nil
	}

	var hover lspHover
	if err := json.Unmarshal(result, &hover); err != nil {
		return tools.ResultSuccess(string(result)), nil
	}

	return tools.ResultSuccess(formatHoverContents(hover.Contents)), nil
}

// locationRequest issues a textDocument/<method> position request and formats
// the result as locations. Used by definition and implementations which share
// exactly the same shape.
func (h *lspHandler) locationRequest(ctx context.Context, method, file string, line, character int, emptyMsg string) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, file)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	result, err := h.sendRequestLocked("textDocument/"+method, map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line - 1, "character": character - 1},
	})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("%s request failed: %s", method, err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		return tools.ResultSuccess(emptyMsg), nil
	}

	return tools.ResultSuccess(formatLocations(result)), nil
}

func (h *lspHandler) definition(ctx context.Context, args PositionArgs) (*tools.ToolCallResult, error) {
	return h.locationRequest(ctx, "definition", args.File, args.Line, args.Character, "No definition found at this position")
}

func (h *lspHandler) implementations(ctx context.Context, args PositionArgs) (*tools.ToolCallResult, error) {
	return h.locationRequest(ctx, "implementation", args.File, args.Line, args.Character, "No implementations found")
}

func (h *lspHandler) references(ctx context.Context, args ReferencesArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	includeDeclaration := args.IncludeDeclaration == nil || *args.IncludeDeclaration

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": args.Line - 1, "character": args.Character - 1},
		"context":      map[string]any{"includeDeclaration": includeDeclaration},
	}

	result, err := h.sendRequestLocked("textDocument/references", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("References request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		return tools.ResultSuccess("No references found"), nil
	}

	return tools.ResultSuccess(formatLocations(result)), nil
}

func (h *lspHandler) documentSymbols(ctx context.Context, args FileArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}

	result, err := h.sendRequestLocked("textDocument/documentSymbol", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Document symbols request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		return tools.ResultSuccess("No symbols found in file"), nil
	}

	return tools.ResultSuccess(formatSymbols(result)), nil
}

func (h *lspHandler) workspaceSymbols(ctx context.Context, args WorkspaceSymbolsArgs) (*tools.ToolCallResult, error) {
	if err := h.ensureInitialized(); err != nil {
		return tools.ResultError(fmt.Sprintf("LSP initialization failed: %s", err)), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	result, err := h.sendRequestLocked("workspace/symbol", map[string]any{"query": args.Query})
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Workspace symbols request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		if args.Query == "" {
			return tools.ResultSuccess("No symbols found in workspace"), nil
		}
		return tools.ResultSuccess(fmt.Sprintf("No symbols found matching '%s'", args.Query)), nil
	}

	return tools.ResultSuccess(formatSymbols(result)), nil
}

func (h *lspHandler) getDiagnostics(ctx context.Context, args FileArgs) (*tools.ToolCallResult, error) {
	if err := h.ensureInitialized(); err != nil {
		return tools.ResultError(fmt.Sprintf("LSP initialization failed: %s", err)), nil
	}

	uri := pathToURI(args.File)
	wasOpen := h.isFileOpen(uri)
	if err := h.openFileOnDemand(ctx, uri); err != nil {
		slog.DebugContext(ctx, "Failed to auto-open file for diagnostics", "file", args.File, "error", err)
	}

	if !wasOpen {
		h.waitForDiagnostics(ctx, 2*time.Second)
	}

	h.diagnosticsMu.RLock()
	diags, ok := h.diagnostics[uri]
	h.diagnosticsMu.RUnlock()

	if !ok || len(diags) == 0 {
		return tools.ResultSuccess("No diagnostics for " + args.File), nil
	}

	return tools.ResultSuccess(formatDiagnostics(args.File, diags)), nil
}

func (h *lspHandler) rename(ctx context.Context, args RenameArgs) (*tools.ToolCallResult, error) {
	if args.NewName == "" {
		return tools.ResultError("new_name is required"), nil
	}

	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": args.Line - 1, "character": args.Character - 1},
		"newName":      args.NewName,
	}

	result, err := h.sendRequestLocked("textDocument/rename", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Rename failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" {
		return tools.ResultError("Cannot rename symbol at this position"), nil
	}

	var edit lspWorkspaceEdit
	if err := json.Unmarshal(result, &edit); err != nil {
		return tools.ResultError(fmt.Sprintf("Failed to parse rename result: %s", err)), nil
	}

	return h.applyWorkspaceEdit(&edit, args.NewName), nil
}

func (h *lspHandler) codeActions(ctx context.Context, args CodeActionsArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	endLine := cmp.Or(args.EndLine, args.StartLine)

	h.mu.Lock()
	defer h.mu.Unlock()

	h.diagnosticsMu.RLock()
	fileDiags := h.diagnostics[uri]
	h.diagnosticsMu.RUnlock()

	rangeDiags := make([]lspDiagnostic, 0)
	for _, d := range fileDiags {
		diagLine := d.Range.Start.Line + 1
		if diagLine >= args.StartLine && diagLine <= endLine {
			rangeDiags = append(rangeDiags, d)
		}
	}

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"range": map[string]any{
			"start": map[string]any{"line": args.StartLine - 1, "character": 0},
			"end":   map[string]any{"line": endLine - 1, "character": 999999},
		},
		"context": map[string]any{"diagnostics": rangeDiags},
	}

	result, err := h.sendRequestLocked("textDocument/codeAction", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Code actions request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		return tools.ResultSuccess(fmt.Sprintf("No code actions available for %s:%d", args.File, args.StartLine)), nil
	}

	return tools.ResultSuccess(formatCodeActions(args.File, args.StartLine, result)), nil
}

func (h *lspHandler) format(ctx context.Context, args FileArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"options":      map[string]any{"tabSize": 4, "insertSpaces": false},
	}

	result, err := h.sendRequestLocked("textDocument/formatting", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Format request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		return tools.ResultSuccess("No formatting changes needed for " + args.File), nil
	}

	var edits []lspTextEdit
	if err := json.Unmarshal(result, &edits); err != nil {
		return tools.ResultError(fmt.Sprintf("Failed to parse format result: %s", err)), nil
	}

	if len(edits) == 0 {
		return tools.ResultSuccess("No formatting changes needed for " + args.File), nil
	}

	if err := applyTextEditsToFile(args.File, edits); err != nil {
		return tools.ResultError(fmt.Sprintf("Failed to apply formatting: %s", err)), nil
	}

	if err := h.notifyFileChangeLocked(uri); err != nil {
		slog.DebugContext(ctx, "Failed to notify LSP of format changes", "error", err)
	}

	return tools.ResultSuccess(fmt.Sprintf("Formatted %s\nApplied %d formatting change(s)", args.File, len(edits))), nil
}

func (h *lspHandler) callHierarchy(ctx context.Context, args CallHierarchyArgs) (*tools.ToolCallResult, error) {
	if args.Direction != "incoming" && args.Direction != "outgoing" {
		return tools.ResultError("direction must be 'incoming' or 'outgoing'"), nil
	}

	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	prepareParams := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": args.Line - 1, "character": args.Character - 1},
	}

	prepareResult, err := h.sendRequestLocked("textDocument/prepareCallHierarchy", prepareParams)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Call hierarchy preparation failed: %s", err)), nil
	}

	if len(prepareResult) == 0 || string(prepareResult) == "null" || string(prepareResult) == "[]" {
		return tools.ResultSuccess("No call hierarchy information available at this position"), nil
	}

	var items []lspCallHierarchyItem
	if err := json.Unmarshal(prepareResult, &items); err != nil {
		return tools.ResultError(fmt.Sprintf("Failed to parse call hierarchy: %s", err)), nil
	}

	if len(items) == 0 {
		return tools.ResultSuccess("No call hierarchy information available at this position"), nil
	}

	var result strings.Builder
	for _, item := range items {
		var method string
		var formatter func(string, json.RawMessage) string
		if args.Direction == "incoming" {
			method = "callHierarchy/incomingCalls"
			formatter = formatIncomingCalls
		} else {
			method = "callHierarchy/outgoingCalls"
			formatter = formatOutgoingCalls
		}

		callResult, err := h.sendRequestLocked(method, map[string]any{"item": item})
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Failed to get %s calls: %s", args.Direction, err)), nil
		}
		result.WriteString(formatter(item.Name, callResult))
	}

	return tools.ResultSuccess(result.String()), nil
}

func (h *lspHandler) typeHierarchy(ctx context.Context, args TypeHierarchyArgs) (*tools.ToolCallResult, error) {
	if args.Direction != "supertypes" && args.Direction != "subtypes" {
		return tools.ResultError("direction must be 'supertypes' or 'subtypes'"), nil
	}

	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	prepareParams := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": args.Line - 1, "character": args.Character - 1},
	}

	prepareResult, err := h.sendRequestLocked("textDocument/prepareTypeHierarchy", prepareParams)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Type hierarchy preparation failed: %s", err)), nil
	}

	if len(prepareResult) == 0 || string(prepareResult) == "null" || string(prepareResult) == "[]" {
		return tools.ResultSuccess("No type hierarchy information available at this position"), nil
	}

	var items []lspTypeHierarchyItem
	if err := json.Unmarshal(prepareResult, &items); err != nil {
		return tools.ResultError(fmt.Sprintf("Failed to parse type hierarchy: %s", err)), nil
	}

	if len(items) == 0 {
		return tools.ResultSuccess("No type hierarchy information available at this position"), nil
	}

	var result strings.Builder
	for _, item := range items {
		method := "typeHierarchy/" + args.Direction
		// Capitalize first letter for direction label
		directionLabel := strings.ToUpper(args.Direction[:1]) + args.Direction[1:]

		typeResult, err := h.sendRequestLocked(method, map[string]any{"item": item})
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Failed to get %s: %s", args.Direction, err)), nil
		}
		result.WriteString(formatTypeHierarchy(item.Name, directionLabel, typeResult))
	}

	return tools.ResultSuccess(result.String()), nil
}

func (h *lspHandler) signatureHelp(ctx context.Context, args PositionArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": args.Line - 1, "character": args.Character - 1},
	}

	result, err := h.sendRequestLocked("textDocument/signatureHelp", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Signature help request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" {
		return tools.ResultSuccess("No signature help available at this position"), nil
	}

	var sigHelp lspSignatureHelp
	if err := json.Unmarshal(result, &sigHelp); err != nil {
		return tools.ResultSuccess(string(result)), nil
	}

	return tools.ResultSuccess(formatSignatureHelp(sigHelp)), nil
}

func (h *lspHandler) inlayHints(ctx context.Context, args InlayHintsArgs) (*tools.ToolCallResult, error) {
	uri, err := h.prepareFileRequest(ctx, args.File)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	startLine := cmp.Or(args.StartLine, 1)
	endLine := cmp.Or(args.EndLine, 100000)

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"range": map[string]any{
			"start": map[string]any{"line": startLine - 1, "character": 0},
			"end":   map[string]any{"line": endLine - 1, "character": 999999},
		},
	}

	result, err := h.sendRequestLocked("textDocument/inlayHint", params)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Inlay hints request failed: %s", err)), nil
	}

	if len(result) == 0 || string(result) == "null" || string(result) == "[]" {
		return tools.ResultSuccess(fmt.Sprintf("No inlay hints for %s:%d-%d", args.File, startLine, endLine)), nil
	}

	var hints []lspInlayHint
	if err := json.Unmarshal(result, &hints); err != nil {
		return tools.ResultSuccess(string(result)), nil
	}

	return tools.ResultSuccess(formatInlayHints(args.File, startLine, endLine, hints)), nil
}

// applyWorkspaceEdit applies a workspace edit to files on disk and notifies
// the LSP server of the changes so its in-memory state stays in sync.
// The caller must hold h.mu.
func (h *lspHandler) applyWorkspaceEdit(edit *lspWorkspaceEdit, newName string) *tools.ToolCallResult {
	var totalChanges int
	var modifiedFiles []string
	fileChangeCounts := make(map[string]int)

	if len(edit.DocumentChanges) > 0 {
		for _, docEdit := range edit.DocumentChanges {
			filePath := strings.TrimPrefix(docEdit.TextDocument.URI, "file://")
			if err := applyTextEditsToFile(filePath, docEdit.Edits); err != nil {
				return tools.ResultError(fmt.Sprintf("Failed to apply changes to %s: %s", filePath, err))
			}
			fileChangeCounts[filePath] = len(docEdit.Edits)
			totalChanges += len(docEdit.Edits)
			modifiedFiles = append(modifiedFiles, filePath)
		}
	}

	if len(edit.Changes) > 0 {
		for uri, edits := range edit.Changes {
			filePath := strings.TrimPrefix(uri, "file://")
			if err := applyTextEditsToFile(filePath, edits); err != nil {
				return tools.ResultError(fmt.Sprintf("Failed to apply changes to %s: %s", filePath, err))
			}
			fileChangeCounts[filePath] = len(edits)
			totalChanges += len(edits)
			modifiedFiles = append(modifiedFiles, filePath)
		}
	}

	if totalChanges == 0 {
		return tools.ResultSuccess("No changes were needed")
	}

	// Notify the LSP server about each modified file that it has open,
	// so subsequent operations (diagnostics, hover, etc.) see the new content.
	for _, file := range modifiedFiles {
		uri := pathToURI(file)
		if h.isFileOpen(uri) {
			if err := h.notifyFileChangeLocked(uri); err != nil {
				slog.Debug("Failed to notify LSP of rename changes", "file", file, "error", err)
			}
		}
	}

	var result strings.Builder
	fmt.Fprintf(&result, "Renamed to '%s'\n", newName)
	fmt.Fprintf(&result, "Modified %d file(s):\n", len(modifiedFiles))
	for _, file := range modifiedFiles {
		fmt.Fprintf(&result, "- %s (%d change(s))\n", file, fileChangeCounts[file])
	}

	return tools.ResultSuccess(result.String())
}

// applyTextEditsToFile applies LSP text edits to a file on disk
func applyTextEditsToFile(filePath string, edits []lspTextEdit) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")

	sortedEdits := make([]lspTextEdit, len(edits))
	copy(sortedEdits, edits)
	slices.SortFunc(sortedEdits, func(a, b lspTextEdit) int {
		if a.Range.Start.Line != b.Range.Start.Line {
			return cmp.Compare(b.Range.Start.Line, a.Range.Start.Line)
		}
		return cmp.Compare(b.Range.Start.Character, a.Range.Start.Character)
	})

	for _, edit := range sortedEdits {
		lines = applyTextEdit(lines, edit)
	}

	newContent := strings.Join(lines, "\n")
	if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil { //nolint:gosec // file pre-exists; mode is only applied on creation
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func applyTextEdit(lines []string, edit lspTextEdit) []string {
	startLine := edit.Range.Start.Line
	startChar := edit.Range.Start.Character
	endLine := edit.Range.End.Line
	endChar := edit.Range.End.Character

	if startLine >= len(lines) {
		return lines
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
		endChar = len(lines[endLine])
	}

	startChar = min(startChar, len(lines[startLine]))
	endChar = min(endChar, len(lines[endLine]))

	prefix := ""
	if startLine < len(lines) && startChar <= len(lines[startLine]) {
		prefix = lines[startLine][:startChar]
	}
	suffix := ""
	if endLine < len(lines) && endChar <= len(lines[endLine]) {
		suffix = lines[endLine][endChar:]
	}

	newText := prefix + edit.NewText + suffix
	newLines := strings.Split(newText, "\n")

	result := make([]string, 0, len(lines)-(endLine-startLine)+len(newLines)-1)
	result = append(result, lines[:startLine]...)
	result = append(result, newLines...)
	if endLine+1 < len(lines) {
		result = append(result, lines[endLine+1:]...)
	}

	return result
}

func formatCodeActions(file string, line int, data json.RawMessage) string {
	var actions []lspCodeAction
	if err := json.Unmarshal(data, &actions); err != nil {
		return string(data)
	}

	if len(actions) == 0 {
		return fmt.Sprintf("No code actions available for %s:%d", file, line)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Available code actions for %s:%d:", file, line))
	for i, action := range actions {
		kind := cmp.Or(action.Kind, "action")
		preferred := ""
		if action.IsPreferred {
			preferred = " (preferred)"
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s%s", i+1, kind, action.Title, preferred))
	}
	return strings.Join(lines, "\n")
}

// LSP protocol helpers

func (h *lspHandler) sendRequestLocked(method string, params any) (json.RawMessage, error) {
	id := h.requestID.Add(1)
	req := lspRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}

	if err := h.writeMessageLocked(req); err != nil {
		return nil, err
	}

	return h.readResponseLocked(id)
}

func (h *lspHandler) sendNotificationLocked(method string, params any) error {
	return h.writeMessageLocked(lspNotification{JSONRPC: "2.0", Method: method, Params: params})
}

func (h *lspHandler) writeMessageLocked(msg any) error {
	// Defend against a Close that ran between ensureInitialized's atomic
	// fast path and our acquisition of h.mu: clearSessionLocked nils
	// h.stdin under the same lock, so under h.mu a nil stdin means "the
	// session has been torn down".
	if h.stdin == nil {
		return lifecycle.ErrNotStarted
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := h.stdin.Write([]byte(header)); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}
	if _, err := h.stdin.Write(data); err != nil {
		return fmt.Errorf("failed to write body: %w", err)
	}

	slog.Debug("LSP request sent", "message", string(data))
	return nil
}

func (h *lspHandler) readResponseLocked(expectedID int64) (json.RawMessage, error) {
	for {
		msg, err := h.readMessageLocked()
		if err != nil {
			return nil, err
		}

		var resp lspResponse
		if err := json.Unmarshal(msg, &resp); err == nil && resp.ID == expectedID {
			if resp.Error != nil {
				return nil, fmt.Errorf("LSP error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}

		h.processNotification(msg)
	}
}

func (h *lspHandler) readMessageLocked() ([]byte, error) {
	if h.stdout == nil {
		return nil, lifecycle.ErrNotStarted
	}
	var contentLength int
	for {
		line, err := h.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read header: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if after, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			lengthStr := strings.TrimSpace(after)
			contentLength, err = strconv.Atoi(lengthStr)
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
		}
	}

	if contentLength == 0 {
		return nil, errors.New("missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(h.stdout, body); err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	slog.Debug("LSP response received", "message", string(body))
	return body, nil
}

func (h *lspHandler) readNotifications(ctx context.Context, stderrBuf *concurrent.Buffer) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if content := stderrBuf.Drain(); content != "" {
				slog.DebugContext(ctx, "LSP stderr", "content", content)
			}
		}
	}
}

func (h *lspHandler) processNotification(msg []byte) {
	var notif struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msg, &notif); err != nil {
		return
	}

	if notif.Method == "textDocument/publishDiagnostics" {
		var params struct {
			URI         string          `json:"uri"`
			Diagnostics []lspDiagnostic `json:"diagnostics"`
		}
		if err := json.Unmarshal(notif.Params, &params); err != nil {
			return
		}
		h.diagnosticsMu.Lock()
		h.diagnostics[params.URI] = params.Diagnostics
		h.diagnosticsVersion.Add(1)
		h.diagnosticsMu.Unlock()
		slog.Debug("Received diagnostics", "uri", params.URI, "count", len(params.Diagnostics))
	}
}

func (h *lspHandler) handlesFile(path string) bool {
	if len(h.fileTypes) == 0 {
		return true
	}

	ext := strings.ToLower(filepath.Ext(path))
	for _, ft := range h.fileTypes {
		pattern := strings.ToLower(ft)
		if !strings.HasPrefix(pattern, ".") {
			pattern = "." + pattern
		}
		if ext == pattern {
			return true
		}
	}
	return false
}

func (h *lspHandler) isFileOpen(uri string) bool {
	h.openFilesMu.RLock()
	defer h.openFilesMu.RUnlock()
	_, ok := h.openFiles[uri]
	return ok
}

func (h *lspHandler) openFileOnDemand(_ context.Context, uri string) error {
	if h.isFileOpen(uri) {
		return nil
	}

	filePath := strings.TrimPrefix(uri, "file://")

	if !h.handlesFile(filePath) {
		return fmt.Errorf("LSP does not handle file type: %s", filepath.Ext(filePath))
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	languageID := detectLanguageID(filePath)

	h.mu.Lock()
	defer h.mu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       string(content),
		},
	}

	if err := h.sendNotificationLocked("textDocument/didOpen", params); err != nil {
		return fmt.Errorf("failed to open document: %w", err)
	}

	h.openFilesMu.Lock()
	h.openFiles[uri] = 1
	h.openFilesMu.Unlock()

	slog.Debug("Auto-opened file for LSP", "uri", uri, "languageId", languageID)
	return nil
}

func (h *lspHandler) NotifyFileChange(_ context.Context, uri string) error {
	if !h.isFileOpen(uri) {
		return fmt.Errorf("file not open: %s", uri)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	return h.notifyFileChangeLocked(uri)
}

// notifyFileChangeLocked re-reads a file from disk and sends a
// textDocument/didChange notification. The caller must hold h.mu.
func (h *lspHandler) notifyFileChangeLocked(uri string) error {
	filePath := strings.TrimPrefix(uri, "file://")

	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	h.openFilesMu.Lock()
	h.openFiles[uri]++
	version := h.openFiles[uri]
	h.openFilesMu.Unlock()

	changeParams := map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": string(content)}},
	}

	return h.sendNotificationLocked("textDocument/didChange", changeParams)
}

func (h *lspHandler) waitForDiagnostics(ctx context.Context, timeout time.Duration) {
	initialVersion := h.diagnosticsVersion.Load()
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			if h.diagnosticsVersion.Load() != initialVersion {
				return
			}
		}
	}
}

func pathToURI(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "file://" + path
	}
	return "file://" + absPath
}

func detectLanguageID(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	languageMap := map[string]string{
		".go":         "go",
		".py":         "python",
		".js":         "javascript",
		".jsx":        "javascriptreact",
		".ts":         "typescript",
		".tsx":        "typescriptreact",
		".rs":         "rust",
		".c":          "c",
		".cpp":        "cpp",
		".cxx":        "cpp",
		".cc":         "cpp",
		".c++":        "cpp",
		".h":          "c",
		".hpp":        "cpp",
		".hxx":        "cpp",
		".hh":         "cpp",
		".h++":        "cpp",
		".java":       "java",
		".rb":         "ruby",
		".php":        "php",
		".cs":         "csharp",
		".swift":      "swift",
		".kt":         "kotlin",
		".kts":        "kotlin",
		".scala":      "scala",
		".lua":        "lua",
		".r":          "r",
		".sh":         "shellscript",
		".bash":       "shellscript",
		".zsh":        "shellscript",
		".ps1":        "powershell",
		".psm1":       "powershell",
		".sql":        "sql",
		".html":       "html",
		".htm":        "html",
		".css":        "css",
		".scss":       "scss",
		".sass":       "sass",
		".less":       "less",
		".json":       "json",
		".yaml":       "yaml",
		".yml":        "yaml",
		".xml":        "xml",
		".md":         "markdown",
		".markdown":   "markdown",
		".dockerfile": "dockerfile",
		".vue":        "vue",
		".svelte":     "svelte",
		".ex":         "elixir",
		".exs":        "elixir",
		".erl":        "erlang",
		".hrl":        "erlang",
		".hs":         "haskell",
		".ml":         "ocaml",
		".mli":        "ocaml",
		".fs":         "fsharp",
		".fsi":        "fsharp",
		".fsx":        "fsharp",
		".clj":        "clojure",
		".cljs":       "clojure",
		".cljc":       "clojure",
		".dart":       "dart",
		".groovy":     "groovy",
		".pl":         "perl",
		".pm":         "perl",
		".tf":         "terraform",
		".tfvars":     "terraform",
		".zig":        "zig",
		".nim":        "nim",
		".v":          "v",
		".odin":       "odin",
	}

	if lang, ok := languageMap[ext]; ok {
		return lang
	}

	base := strings.ToLower(filepath.Base(path))
	specialFiles := map[string]string{
		"dockerfile":     "dockerfile",
		"makefile":       "makefile",
		"gnumakefile":    "makefile",
		"cmakelists.txt": "cmake",
	}
	if lang, ok := specialFiles[base]; ok {
		return lang
	}

	return "plaintext"
}

// Formatting helpers

func formatHoverContents(contents any) string {
	switch c := contents.(type) {
	case string:
		return c
	case map[string]any:
		if value, ok := c["value"].(string); ok {
			return value
		}
		data, _ := json.Marshal(c)
		return string(data)
	case []any:
		var parts []string
		for _, item := range c {
			parts = append(parts, formatHoverContents(item))
		}
		return strings.Join(parts, "\n\n")
	default:
		data, _ := json.Marshal(contents)
		return string(data)
	}
}

func formatLocations(data json.RawMessage) string {
	var loc lspLocation
	if err := json.Unmarshal(data, &loc); err == nil && loc.URI != "" {
		return formatLocation(loc)
	}

	var locs []lspLocation
	if err := json.Unmarshal(data, &locs); err == nil {
		var lines []string
		for _, l := range locs {
			lines = append(lines, formatLocation(l))
		}
		if len(lines) == 0 {
			return "No locations found"
		}
		return fmt.Sprintf("Found %d location(s):\n%s", len(lines), strings.Join(lines, "\n"))
	}

	return string(data)
}

func formatLocation(loc lspLocation) string {
	return fmt.Sprintf("- %s:%d:%d",
		strings.TrimPrefix(loc.URI, "file://"),
		loc.Range.Start.Line+1,
		loc.Range.Start.Character+1)
}

func formatSymbols(data json.RawMessage) string {
	var docSymbols []lspDocumentSymbol
	if err := json.Unmarshal(data, &docSymbols); err == nil && len(docSymbols) > 0 {
		if docSymbols[0].Range.Start.Line > 0 || docSymbols[0].Range.End.Line > 0 {
			var lines []string
			formatDocumentSymbols(docSymbols, "", &lines)
			return strings.Join(lines, "\n")
		}
	}

	var symbols []lspSymbolInformation
	if err := json.Unmarshal(data, &symbols); err == nil {
		var lines []string
		for _, s := range symbols {
			kind := symbolKindName(s.Kind)
			loc := strings.TrimPrefix(s.Location.URI, "file://")
			line := fmt.Sprintf("- %s %s (%s:%d)", kind, s.Name, loc, s.Location.Range.Start.Line+1)
			if s.ContainerName != "" {
				line += fmt.Sprintf(" [in %s]", s.ContainerName)
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			return "No symbols found"
		}
		return strings.Join(lines, "\n")
	}

	return string(data)
}

func formatDocumentSymbols(symbols []lspDocumentSymbol, indent string, lines *[]string) {
	for _, s := range symbols {
		kind := symbolKindName(s.Kind)
		*lines = append(*lines, fmt.Sprintf("%s- %s %s (line %d)", indent, kind, s.Name, s.Range.Start.Line+1))
		if len(s.Children) > 0 {
			formatDocumentSymbols(s.Children, indent+"  ", lines)
		}
	}
}

func formatDiagnostics(file string, diags []lspDiagnostic) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Diagnostics for %s:", file))
	for _, d := range diags {
		severity := diagnosticSeverityName(d.Severity)
		lines = append(lines, fmt.Sprintf("- [%s] Line %d: %s", severity, d.Range.Start.Line+1, d.Message))
	}
	return strings.Join(lines, "\n")
}

var symbolKindNames = map[int]string{
	1: "File", 2: "Module", 3: "Namespace", 4: "Package",
	5: "Class", 6: "Method", 7: "Property", 8: "Field",
	9: "Constructor", 10: "Enum", 11: "Interface", 12: "Function",
	13: "Variable", 14: "Constant", 15: "String", 16: "Number",
	17: "Boolean", 18: "Array", 19: "Object", 20: "Key",
	21: "Null", 22: "EnumMember", 23: "Struct", 24: "Event",
	25: "Operator", 26: "TypeParameter",
}

func symbolKindName(kind int) string {
	if name, ok := symbolKindNames[kind]; ok {
		return name
	}
	return fmt.Sprintf("Kind%d", kind)
}

func diagnosticSeverityName(severity int) string {
	switch severity {
	case 1:
		return "Error"
	case 2:
		return "Warning"
	case 3:
		return "Info"
	case 4:
		return "Hint"
	default:
		return "Unknown"
	}
}

func formatIncomingCalls(targetName string, data json.RawMessage) string {
	var calls []lspCallHierarchyIncomingCall
	if err := json.Unmarshal(data, &calls); err != nil {
		return string(data)
	}

	if len(calls) == 0 {
		return fmt.Sprintf("No incoming calls to '%s'", targetName)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Incoming calls to '%s':", targetName))
	for _, call := range calls {
		filePath := strings.TrimPrefix(call.From.URI, "file://")
		line := call.From.Range.Start.Line + 1
		detail := ""
		if call.From.Detail != "" {
			detail = fmt.Sprintf(" [%s]", call.From.Detail)
		}

		callLines := make([]string, 0, len(call.FromRanges))
		for _, r := range call.FromRanges {
			callLines = append(callLines, strconv.Itoa(r.Start.Line+1))
		}

		lines = append(lines, fmt.Sprintf("- %s %s (%s:%d)%s calls at line(s) %s",
			symbolKindName(call.From.Kind), call.From.Name, filePath, line, detail, strings.Join(callLines, ", ")))
	}
	return strings.Join(lines, "\n")
}

func formatOutgoingCalls(sourceName string, data json.RawMessage) string {
	var calls []lspCallHierarchyOutgoingCall
	if err := json.Unmarshal(data, &calls); err != nil {
		return string(data)
	}

	if len(calls) == 0 {
		return fmt.Sprintf("No outgoing calls from '%s'", sourceName)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Outgoing calls from '%s':", sourceName))
	for _, call := range calls {
		filePath := strings.TrimPrefix(call.To.URI, "file://")
		line := call.To.Range.Start.Line + 1
		detail := ""
		if call.To.Detail != "" {
			detail = fmt.Sprintf(" [%s]", call.To.Detail)
		}
		lines = append(lines, fmt.Sprintf("- %s %s (%s:%d)%s",
			symbolKindName(call.To.Kind), call.To.Name, filePath, line, detail))
	}
	return strings.Join(lines, "\n")
}

func formatTypeHierarchy(typeName, direction string, data json.RawMessage) string {
	var items []lspTypeHierarchyItem
	if err := json.Unmarshal(data, &items); err != nil {
		return string(data)
	}

	if len(items) == 0 {
		return fmt.Sprintf("No %s for '%s'", strings.ToLower(direction), typeName)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("%s of '%s':", direction, typeName))
	for _, item := range items {
		filePath := strings.TrimPrefix(item.URI, "file://")
		line := item.Range.Start.Line + 1
		detail := ""
		if item.Detail != "" {
			detail = fmt.Sprintf(" [%s]", item.Detail)
		}
		lines = append(lines, fmt.Sprintf("- %s %s (%s:%d)%s",
			symbolKindName(item.Kind), item.Name, filePath, line, detail))
	}
	return strings.Join(lines, "\n")
}

func formatSignatureHelp(help lspSignatureHelp) string {
	if len(help.Signatures) == 0 {
		return "No signature help available"
	}

	var lines []string

	for i, sig := range help.Signatures {
		if i > 0 {
			lines = append(lines, "")
		}

		active := ""
		if i == help.ActiveSignature {
			active = " [ACTIVE]"
		}
		lines = append(lines, fmt.Sprintf("Function: %s%s", sig.Label, active))

		if sig.Documentation != nil {
			doc := formatHoverContents(sig.Documentation)
			if doc != "" {
				lines = append(lines, "", doc)
			}
		}

		if len(sig.Parameters) > 0 {
			lines = append(lines, "", "Parameters:")

			activeParam := help.ActiveParameter
			if sig.ActiveParameter > 0 {
				activeParam = sig.ActiveParameter
			}

			for j, param := range sig.Parameters {
				label := formatParameterLabel(param.Label)
				paramActive := ""
				if j == activeParam {
					paramActive = " [ACTIVE]"
				}

				paramLine := fmt.Sprintf("%d. %s%s", j+1, label, paramActive)

				if param.Documentation != nil {
					doc := formatHoverContents(param.Documentation)
					if doc != "" {
						paramLine += " - " + doc
					}
				}

				lines = append(lines, paramLine)
			}

			lines = append(lines, "", fmt.Sprintf("Currently typing parameter %d of %d", activeParam+1, len(sig.Parameters)))
		}
	}

	return strings.Join(lines, "\n")
}

func formatParameterLabel(label any) string {
	switch l := label.(type) {
	case string:
		return l
	case []any:
		if len(l) == 2 {
			return fmt.Sprintf("[%v:%v]", l[0], l[1])
		}
	}
	return fmt.Sprintf("%v", label)
}

func formatInlayHints(file string, startLine, endLine int, hints []lspInlayHint) string {
	if len(hints) == 0 {
		return fmt.Sprintf("No inlay hints for %s:%d-%d", file, startLine, endLine)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Inlay hints for %s:%d-%d:", file, startLine, endLine))

	for _, hint := range hints {
		label := formatInlayHintLabel(hint.Label)
		kind := inlayHintKindName(hint.Kind)

		lines = append(lines, fmt.Sprintf("- Line %d, Col %d: '%s' (%s)",
			hint.Position.Line+1, hint.Position.Character+1, label, kind))
	}

	return strings.Join(lines, "\n")
}

func formatInlayHintLabel(label any) string {
	switch l := label.(type) {
	case string:
		return l
	case []any:
		var parts []string
		for _, part := range l {
			if partMap, ok := part.(map[string]any); ok {
				if value, ok := partMap["value"].(string); ok {
					parts = append(parts, value)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return fmt.Sprintf("%v", label)
}

func inlayHintKindName(kind int) string {
	switch kind {
	case 1:
		return "type"
	case 2:
		return "parameter"
	default:
		return "hint"
	}
}
