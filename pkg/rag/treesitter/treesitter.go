//go:build cgo

package treesitter

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"

	"github.com/docker/docker-agent/pkg/rag/chunk"
)

// DocumentProcessor uses tree-sitter to build syntax trees for source
// files and produce semantically aligned chunks (e.g., whole functions) while
// still respecting a maximum chunk size where possible.
//
// NOTE: To keep the initial implementation minimal, this currently supports
// Go source files via the golang grammar. The design is intentionally generic
// so we can add more languages incrementally.
//
// The processor is thread-safe: it creates a new parser for each Process()
// call since the underlying tree-sitter C library is not thread-safe.
type DocumentProcessor struct {
	chunkSize    int
	chunkOverlap int
	langByExt    map[string]*sitter.Language
	functionNode map[string]func(*sitter.Node) bool
	textFallback *chunk.TextDocumentProcessor
}

// NewDocumentProcessor creates a new document processor instance with a
// language mapping that can be expanded over time. Falls back to text chunking
// for unsupported file types.
func NewDocumentProcessor(chunkSize, chunkOverlap int, respectWordBoundaries bool) *DocumentProcessor {
	// Currently only Go is wired; more languages can be added later.
	langByExt := map[string]*sitter.Language{
		".go": golang.GetLanguage(),
	}

	functionNode := map[string]func(*sitter.Node) bool{
		".go": isGoFunctionLike,
	}

	return &DocumentProcessor{
		chunkSize:    chunkSize,
		chunkOverlap: chunkOverlap,
		langByExt:    langByExt,
		functionNode: functionNode,
		textFallback: chunk.NewTextDocumentProcessor(chunkSize, chunkOverlap, respectWordBoundaries),
	}
}

// Process implements chunk.DocumentProcessor.
func (p *DocumentProcessor) Process(path string, content []byte) ([]chunk.Chunk, error) {
	slog.Debug("[TreeSitter] Starting to process file",
		"path", path,
		"content_size", len(content),
		"chunk_size", p.chunkSize,
		"chunk_overlap", p.chunkOverlap)

	ext := strings.ToLower(filepath.Ext(path))
	lang, ok := p.langByExt[ext]
	if !ok {
		slog.Debug("[TreeSitter] Unsupported file extension, falling back to text chunking",
			"path", path,
			"extension", ext)
		return p.textFallback.Process(path, content)
	}

	slog.Debug("[TreeSitter] Language detected",
		"path", path,
		"extension", ext)

	// Create a new parser for each call to ensure thread-safety
	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	slog.Debug("[TreeSitter] Parsing source code with tree-sitter",
		"path", path)

	// Use ParseCtx instead of deprecated Parse
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil || tree.RootNode() == nil {
		slog.Debug("[TreeSitter] Parsing failed, falling back to text chunking",
			"path", path,
			"error", err)
		return p.textFallback.Process(path, content)
	}

	slog.Debug("[TreeSitter] Successfully parsed source code",
		"path", path)

	root := tree.RootNode()
	packageName := extractPackageName(root, content)
	fnFilter, ok := p.functionNode[ext]
	if !ok {
		slog.Debug("[TreeSitter] No function filter defined for extension, falling back to text chunking",
			"path", path,
			"extension", ext)
		return p.textFallback.Process(path, content)
	}

	// Extract function-like nodes.
	var funcNodes []*sitter.Node
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if fnFilter(n) {
			funcNodes = append(funcNodes, n)
			return
		}
		for i := range int(n.ChildCount()) {
			child := n.Child(i)
			if child == nil {
				continue
			}
			walk(child)
		}
	}
	walk(root)

	slog.Debug("[TreeSitter] Extracted function nodes from syntax tree",
		"path", path,
		"function_count", len(funcNodes))

	// If we didn't find any function-like nodes, fall back to text chunking.
	if len(funcNodes) == 0 {
		slog.Debug("[TreeSitter] No function nodes found, falling back to text chunking",
			"path", path)
		return p.textFallback.Process(path, content)
	}

	// Group functions into chunks under the size budget where possible, without
	// ever splitting a single function across chunks.
	text := string(content)
	var chunksOut []chunk.Chunk
	index := 0

	var buf strings.Builder
	currentLen := 0
	var chunkFunctions []functionMetadata

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		c := strings.TrimSpace(buf.String())
		if c == "" {
			buf.Reset()
			currentLen = 0
			return
		}
		chunksOut = append(chunksOut, chunk.Chunk{
			Index:    index,
			Content:  c,
			Metadata: buildChunkMetadata(chunkFunctions),
		})
		slog.Debug("[TreeSitter] Created code-aware chunk",
			"chunk_index", index,
			"chunk_content", c)
		index++
		buf.Reset()
		currentLen = 0
		chunkFunctions = nil
	}

	for funcIdx, fn := range funcNodes {
		// Find any comments that precede this function
		start := int(findPrecedingComments(fn, content))
		end := int(fn.EndByte())
		if start < 0 || end <= start || end > len(text) {
			slog.Debug("[TreeSitter] Skipping function node with invalid byte range",
				"path", path,
				"function_index", funcIdx,
				"start_byte", start,
				"end_byte", end)
			continue
		}

		fnText := strings.TrimSpace(text[start:end])
		if fnText == "" {
			slog.Debug("[TreeSitter] Skipping empty function node",
				"path", path,
				"function_index", funcIdx)
			continue
		}

		fnLen := utf8.RuneCountInString(fnText)
		fnType := fn.Type()

		docText := ""
		funcStart := int(fn.StartByte())
		if start >= 0 && funcStart <= len(content) && start < funcStart {
			docText = string(content[start:funcStart])
		}

		slog.Debug("[TreeSitter] Processing function node",
			"path", path,
			"function_index", funcIdx,
			"function_type", fnType,
			"function_length", fnLen,
			"current_chunk_length", currentLen,
			"chunk_size_limit", p.chunkSize)

		// If the function alone is larger than chunkSize, emit it as its own
		// chunk to avoid splitting function bodies.
		if p.chunkSize > 0 && fnLen > p.chunkSize {
			slog.Debug("[TreeSitter] Function exceeds chunk size, creating dedicated chunk",
				"path", path,
				"function_index", funcIdx,
				"function_length", fnLen,
				"chunk_size_limit", p.chunkSize,
				"chunk_index", index)
			flush()
			meta := buildFunctionMetadata(fn, content, packageName, docText)
			chunksOut = append(chunksOut, chunk.Chunk{
				Index:    index,
				Content:  fnText,
				Metadata: buildChunkMetadata([]functionMetadata{meta}),
			})
			slog.Debug("[TreeSitter] Created code-aware chunk for large function",
				"chunk_index", index,
				"chunk_content", fnText)
			index++
			continue
		}

		// If adding this function would exceed the budget, flush and start new.
		if p.chunkSize > 0 && currentLen > 0 && currentLen+fnLen > p.chunkSize {
			slog.Debug("[TreeSitter] Adding function would exceed chunk size, flushing current chunk",
				"path", path,
				"function_index", funcIdx,
				"current_chunk_length", currentLen,
				"function_length", fnLen,
				"total_would_be", currentLen+fnLen,
				"chunk_size_limit", p.chunkSize,
				"chunk_index", index)
			flush()
		}

		slog.Debug("[TreeSitter] Adding function to current chunk",
			"path", path,
			"function_index", funcIdx,
			"function_length", fnLen,
			"new_chunk_length", currentLen+fnLen)

		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(fnText)
		currentLen += fnLen
		chunkFunctions = append(chunkFunctions, buildFunctionMetadata(fn, content, packageName, docText))
	}

	flush()

	if len(chunksOut) == 0 {
		slog.Debug("[TreeSitter] No chunks produced after processing, falling back to text chunking",
			"path", path)
		return p.textFallback.Process(path, content)
	}

	// Calculate statistics
	totalChars := 0
	minChunkSize := int(^uint(0) >> 1) // max int
	maxChunkSize := 0
	for _, c := range chunksOut {
		chunkLen := utf8.RuneCountInString(c.Content)
		totalChars += chunkLen
		if chunkLen < minChunkSize {
			minChunkSize = chunkLen
		}
		if chunkLen > maxChunkSize {
			maxChunkSize = chunkLen
		}
	}
	avgChunkSize := 0
	if len(chunksOut) > 0 {
		avgChunkSize = totalChars / len(chunksOut)
	}

	slog.Debug("[TreeSitter] Successfully chunked file using syntax tree",
		"path", path,
		"total_functions", len(funcNodes),
		"total_chunks", len(chunksOut),
		"avg_chunk_size", avgChunkSize,
		"min_chunk_size", minChunkSize,
		"max_chunk_size", maxChunkSize,
		"total_content_size", totalChars)

	return chunksOut, nil
}

// isGoFunctionLike returns true for nodes that represent top-level functions
// or methods in Go. The exact node types are determined by the golang grammar.
func isGoFunctionLike(n *sitter.Node) bool {
	switch n.Type() {
	case "function_declaration", "method_declaration":
		return true
	default:
		return false
	}
}

// findPrecedingComments finds all comment nodes that immediately precede a function
// in the source code. This includes godoc-style comments and any other comments
// that are part of the function's documentation.
func findPrecedingComments(fn *sitter.Node, content []byte) (startByte uint32) {
	startByte = fn.StartByte()
	parent := fn.Parent()
	if parent == nil {
		return startByte
	}

	// Find the index of our function node among its siblings
	fnIndex := -1
	for i := range int(parent.ChildCount()) {
		if parent.Child(i) == fn {
			fnIndex = i
			break
		}
	}

	if fnIndex <= 0 {
		// No siblings before this function
		return startByte
	}

	// Walk backwards through siblings to find comments
	var commentNodes []*sitter.Node
	for i := fnIndex - 1; i >= 0; i-- {
		sibling := parent.Child(i)
		if sibling == nil {
			break
		}

		// Check if this is a comment node
		if sibling.Type() == "comment" {
			commentNodes = append([]*sitter.Node{sibling}, commentNodes...)
			continue
		}

		// If we hit a non-comment node that's not just whitespace, stop
		// Check if the node is empty or only contains whitespace
		nodeStart := int(sibling.StartByte())
		nodeEnd := int(sibling.EndByte())
		if nodeStart >= 0 && nodeEnd <= len(content) && nodeEnd > nodeStart {
			nodeText := strings.TrimSpace(string(content[nodeStart:nodeEnd]))
			if nodeText != "" {
				// Hit a non-comment, non-whitespace node
				break
			}
		}
	}

	// If we found comments, use the start of the first one
	if len(commentNodes) > 0 {
		// Check if there are blank lines between comments and function
		// We want to include comments that are directly adjacent to the function
		lastComment := commentNodes[len(commentNodes)-1]
		commentEnd := int(lastComment.EndByte())
		functionStart := int(fn.StartByte())

		if commentEnd < functionStart && functionStart <= len(content) {
			// Check the gap between comment and function
			gap := string(content[commentEnd:functionStart])
			// Count newlines in the gap
			newlines := strings.Count(gap, "\n")
			// If there's more than one blank line, don't include comments
			// (1 newline = same line, 2 newlines = one blank line)
			if newlines > 2 {
				return startByte
			}
		}

		return commentNodes[0].StartByte()
	}

	return startByte
}

type functionMetadata struct {
	Name      string
	Kind      string
	Receiver  string
	Signature string
	Doc       string
	Package   string
	StartLine int
	EndLine   int
}

func buildChunkMetadata(functions []functionMetadata) map[string]string {
	if len(functions) == 0 {
		return nil
	}

	meta := make(map[string]string, 10)
	meta["symbol_count"] = strconv.Itoa(len(functions))

	primary := functions[0]
	if primary.Name != "" {
		meta["symbol_name"] = primary.Name
	}
	if primary.Kind != "" {
		meta["symbol_kind"] = primary.Kind
	}
	if primary.Receiver != "" {
		meta["receiver"] = primary.Receiver
	}
	if primary.Signature != "" {
		meta["signature"] = primary.Signature
	}
	if primary.Doc != "" {
		meta["doc"] = primary.Doc
	}
	if primary.Package != "" {
		meta["package"] = primary.Package
	}
	if primary.StartLine > 0 {
		meta["start_line"] = strconv.Itoa(primary.StartLine)
	}
	if primary.EndLine > 0 {
		meta["end_line"] = strconv.Itoa(primary.EndLine)
	}

	if len(functions) > 1 {
		names := make([]string, 0, len(functions)-1)
		for _, fn := range functions[1:] {
			if fn.Name != "" {
				names = append(names, fn.Name)
			}
		}
		if len(names) > 0 {
			meta["additional_symbols"] = strings.Join(names, ", ")
		}
	}

	return meta
}

func buildFunctionMetadata(fn *sitter.Node, content []byte, pkgName, docText string) functionMetadata {
	meta := functionMetadata{
		Name:      strings.TrimSpace(nodeText(content, fn.ChildByFieldName("name"))),
		Kind:      mapFunctionKind(fn.Type()),
		Receiver:  strings.TrimSpace(nodeText(content, fn.ChildByFieldName("receiver"))),
		Signature: buildGoSignature(content, fn),
		Doc:       truncateMetadataValue(strings.TrimSpace(docText), 400),
		Package:   pkgName,
		StartLine: int(fn.StartPoint().Row) + 1,
		EndLine:   int(fn.EndPoint().Row) + 1,
	}

	return meta
}

func mapFunctionKind(nodeType string) string {
	if nodeType == "method_declaration" {
		return "method"
	}
	return "function"
}

func buildGoSignature(content []byte, fn *sitter.Node) string {
	if fn == nil {
		return ""
	}

	text := strings.TrimSpace(string(content[fn.StartByte():fn.EndByte()]))
	if text == "" {
		return ""
	}

	if braceIdx := strings.Index(text, "{"); braceIdx != -1 {
		text = strings.TrimSpace(text[:braceIdx])
	}
	if newlineIdx := strings.Index(text, "\n"); newlineIdx != -1 {
		text = strings.TrimSpace(text[:newlineIdx])
	}

	return truncateMetadataValue(text, 240)
}

func truncateMetadataValue(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func extractPackageName(root *sitter.Node, content []byte) string {
	if root == nil {
		return ""
	}

	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		if child == nil {
			continue
		}
		if child.Type() != "package_clause" {
			continue
		}
		if name := child.ChildByFieldName("name"); name != nil {
			return strings.TrimSpace(nodeText(content, name))
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "package "); ok {
			return strings.TrimSpace(after)
		}
	}

	return ""
}

func nodeText(content []byte, node *sitter.Node) string {
	if node == nil {
		return ""
	}
	start := node.StartByte()
	end := node.EndByte()
	if end <= start || int(end) > len(content) {
		return ""
	}
	return string(content[start:end])
}
