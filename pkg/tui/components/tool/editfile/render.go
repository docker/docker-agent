package editfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/aymanbagabas/go-udiff"
	"github.com/mattn/go-runewidth"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	tabWidth     = 4
	lineNumWidth = 5
	minWidth     = 80
)

type toolRenderCache struct {
	// Line counts - computed once, never change
	added       int
	removed     int
	lineCounted bool

	// Rendered output - invalidated when width/splitView/status changes
	rendered       string
	renderCached   bool
	renderedWidth  int
	renderedSplit  bool
	renderedStatus types.ToolStatus
}

var (
	cache   = make(map[string]*toolRenderCache) // keyed by toolCallID
	cacheMu sync.RWMutex

	lexerCache   = make(map[string]chroma.Lexer)
	lexerCacheMu sync.RWMutex
)

// InvalidateCaches clears all render caches.
// Call this when the theme changes to pick up new colors.
func InvalidateCaches() {
	cacheMu.Lock()
	for _, c := range cache {
		c.renderCached = false
	}
	cacheMu.Unlock()
}

type chromaToken struct {
	Text  string
	Style lipgloss.Style
}

type linePair struct {
	old        *udiff.Line
	new        *udiff.Line
	oldLineNum int
	newLineNum int
}

func getOrCreateCache(toolCallID string) *toolRenderCache {
	cacheMu.RLock()
	if c, ok := cache[toolCallID]; ok {
		cacheMu.RUnlock()
		return c
	}
	cacheMu.RUnlock()

	cacheMu.Lock()
	defer cacheMu.Unlock()
	// Double-check after acquiring write lock
	if c, ok := cache[toolCallID]; ok {
		return c
	}
	c := &toolRenderCache{}
	cache[toolCallID] = c
	return c
}

func renderEditFile(toolCall tools.ToolCall, width int, splitView bool, toolStatus types.ToolStatus) string {
	c := getOrCreateCache(toolCall.ID)

	cacheMu.RLock()
	if c.renderCached &&
		c.renderedWidth == width &&
		c.renderedSplit == splitView &&
		c.renderedStatus == toolStatus {
		result := c.rendered
		cacheMu.RUnlock()
		return result
	}
	cacheMu.RUnlock()

	result := renderEditFileUncached(toolCall, width, splitView, toolStatus)

	cacheMu.Lock()
	c.rendered = result
	c.renderCached = true
	c.renderedWidth = width
	c.renderedSplit = splitView
	c.renderedStatus = toolStatus
	cacheMu.Unlock()

	return result
}

func renderEditFileUncached(toolCall tools.ToolCall, width int, splitView bool, toolStatus types.ToolStatus) string {
	args, err := filesystem.ParseEditFileArgs([]byte(toolCall.Function.Arguments))
	if err != nil {
		return ""
	}

	var output strings.Builder
	for i, edit := range args.Edits {
		if i > 0 {
			output.WriteString("\n\n")
		}

		if len(args.Edits) > 1 {
			fmt.Fprintf(&output, "Edit #%d:\n", i+1)
		}

		diff := computeDiff(args.Path, edit.OldText, edit.NewText, toolStatus)
		if splitView {
			output.WriteString(renderSplitDiffWithSyntaxHighlight(diff, args.Path, width))
		} else {
			output.WriteString(renderDiffWithSyntaxHighlight(diff, args.Path, width))
		}
	}

	return output.String()
}

// countDiffLines returns the number of added and removed lines for the edit.
// Results are cached per tool call since arguments are immutable.
func countDiffLines(toolCall tools.ToolCall, _ types.ToolStatus) (added, removed int) {
	c := getOrCreateCache(toolCall.ID)

	cacheMu.RLock()
	if c.lineCounted {
		added, removed = c.added, c.removed
		cacheMu.RUnlock()
		return added, removed
	}
	cacheMu.RUnlock()

	added, removed = countDiffLinesUncached(toolCall)

	cacheMu.Lock()
	c.added = added
	c.removed = removed
	c.lineCounted = true
	cacheMu.Unlock()

	return added, removed
}

func countDiffLinesUncached(toolCall tools.ToolCall) (added, removed int) {
	args, err := filesystem.ParseEditFileArgs([]byte(toolCall.Function.Arguments))
	if err != nil {
		return 0, 0
	}

	for _, edit := range args.Edits {
		edits := udiff.Strings(edit.OldText, edit.NewText)
		diff, err := udiff.ToUnifiedDiff("old", "new", edit.OldText, edits, 0)
		if err != nil {
			continue
		}
		for _, hunk := range diff.Hunks {
			for _, line := range hunk.Lines {
				switch line.Kind {
				case udiff.Insert:
					added++
				case udiff.Delete:
					removed++
				}
			}
		}
	}
	return added, removed
}

func computeDiff(path, oldText, newText string, toolStatus types.ToolStatus) []*udiff.Hunk {
	currentContent, err := os.ReadFile(path)
	if err != nil {
		return []*udiff.Hunk{}
	}

	var oldContent, newContent string

	if toolStatus == types.ToolStatusConfirmation {
		// During confirmation: file hasn't been modified yet
		// currentContent is the old content, we need to compute what new would be
		oldContent = string(currentContent)
		newContent = strings.Replace(oldContent, oldText, newText, 1)
	} else {
		// After execution: file has been modified
		// currentContent is the new content, we need to reconstruct old
		newContent = string(currentContent)
		oldContent = strings.Replace(newContent, newText, oldText, 1)
	}

	edits := udiff.Strings(oldContent, newContent)

	diff, err := udiff.ToUnifiedDiff("old", "new", oldContent, edits, 3)
	if err != nil {
		return []*udiff.Hunk{}
	}

	return normalizeDiff(diff.Hunks)
}

func normalizeDiff(diff []*udiff.Hunk) []*udiff.Hunk {
	for _, hunk := range diff {
		if len(hunk.Lines) == 0 {
			continue
		}

		normalized := make([]udiff.Line, 0, len(hunk.Lines))
		for i := 0; i < len(hunk.Lines); i++ {
			line := hunk.Lines[i]

			if line.Kind == udiff.Delete && i+1 < len(hunk.Lines) {
				next := hunk.Lines[i+1]
				if next.Kind == udiff.Insert && line.Content == next.Content {
					normalized = append(normalized, udiff.Line{
						Kind:    udiff.Equal,
						Content: line.Content,
					})
					i++
					continue
				}
			}

			normalized = append(normalized, line)
		}

		hunk.Lines = normalized
	}

	return diff
}

func syntaxHighlight(code, filePath string) []chromaToken {
	ext := filepath.Ext(filePath)

	// Try to get lexer from cache
	lexerCacheMu.RLock()
	lexer, ok := lexerCache[ext]
	lexerCacheMu.RUnlock()

	if !ok {
		// Cache miss - compute and store
		lexer = lexers.Match(filePath)
		if lexer == nil {
			lexer = lexers.Fallback
		}
		lexer = chroma.Coalesce(lexer)

		lexerCacheMu.Lock()
		lexerCache[ext] = lexer
		lexerCacheMu.Unlock()
	}

	style := styles.ChromaStyle()
	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return []chromaToken{{Text: code, Style: lipgloss.NewStyle()}}
	}

	var tokens []chromaToken
	for _, token := range iterator.Tokens() {
		text := strings.TrimSuffix(token.Value, "\n")
		if text == "" {
			continue
		}
		tokens = append(tokens, chromaToken{
			Text:  text,
			Style: chromaToLipgloss(token.Type, style),
		})
	}

	return tokens
}

func chromaToLipgloss(tokenType chroma.TokenType, style *chroma.Style) lipgloss.Style {
	entry := style.Get(tokenType)
	lipStyle := lipgloss.NewStyle()

	if entry.Colour.IsSet() {
		lipStyle = lipStyle.Foreground(lipgloss.Color(entry.Colour.String()))
	}
	if entry.Bold == chroma.Yes {
		lipStyle = lipStyle.Bold(true)
	}
	if entry.Italic == chroma.Yes {
		lipStyle = lipStyle.Italic(true)
	}
	if entry.Underline == chroma.Yes {
		lipStyle = lipStyle.Underline(true)
	}

	return lipStyle
}

func renderDiffWithSyntaxHighlight(diff []*udiff.Hunk, filePath string, width int) string {
	var output strings.Builder
	contentWidth := width - lineNumWidth

	for _, hunk := range diff {
		oldLineNum := hunk.FromLine
		newLineNum := hunk.ToLine

		for _, line := range hunk.Lines {
			lineNum := getDisplayLineNumber(&line, &oldLineNum, &newLineNum)
			content := prepareContent(line.Content)
			tokens := syntaxHighlight(content, filePath)
			lineStyle := getLineStyle(line.Kind)
			wrappedTokens := wrapTokens(tokens, contentWidth)

			for i, tokenLine := range wrappedTokens {
				var lineNumStr string
				if i == 0 {
					// Show line number only on first wrapped line
					lineNumStr = styles.LineNumberStyle.Render(fmt.Sprintf("%4d ", lineNum))
				} else {
					// Use continuation indicator for wrapped lines
					lineNumStr = styles.LineNumberStyle.Render("   → ")
				}
				rendered := renderTokensWithStyle(tokenLine, lineStyle)
				padded := padToWidth(rendered, contentWidth, lineStyle)
				output.WriteString(lineNumStr + padded + "\n")
			}
		}
	}

	return strings.TrimSuffix(output.String(), "\n")
}

func renderSplitDiffWithSyntaxHighlight(diff []*udiff.Hunk, filePath string, width int) string {
	// Fall back to unified diff if terminal is too narrow
	separator := styles.SeparatorStyle.Render(" │ ")
	separatorWidth := lipgloss.Width(separator)
	contentWidth := (width - separatorWidth - (lineNumWidth * 2)) / 2

	if width < minWidth || contentWidth < 10 {
		return renderDiffWithSyntaxHighlight(diff, filePath, width)
	}

	var output strings.Builder

	for _, hunk := range diff {
		for _, pair := range pairDiffLines(hunk.Lines, hunk.FromLine, hunk.ToLine) {
			leftLines := renderSplitSide(pair.old, pair.oldLineNum, filePath, contentWidth)
			rightLines := renderSplitSide(pair.new, pair.newLineNum, filePath, contentWidth)

			// Ensure both sides have the same number of lines for alignment
			maxLines := max(len(rightLines), len(leftLines))

			// Pad shorter side with empty lines
			for len(leftLines) < maxLines {
				leftLines = append(leftLines, renderEmptySplitSide(contentWidth))
			}
			for len(rightLines) < maxLines {
				rightLines = append(rightLines, renderEmptySplitSide(contentWidth))
			}

			for i := range maxLines {
				line := leftLines[i] + separator + rightLines[i]
				line = ensureWidth(line, width)
				output.WriteString(line + "\n")
			}
		}
	}

	return strings.TrimSuffix(output.String(), "\n")
}

func getDisplayLineNumber(line *udiff.Line, oldLineNum, newLineNum *int) int {
	switch line.Kind {
	case udiff.Delete:
		num := *oldLineNum
		*oldLineNum++
		return num
	case udiff.Insert:
		num := *newLineNum
		*newLineNum++
		return num
	case udiff.Equal:
		num := *oldLineNum
		*oldLineNum++
		*newLineNum++
		return num
	}
	return 0
}

func prepareContent(content string) string {
	content = strings.ReplaceAll(content, "\t", strings.Repeat(" ", tabWidth))
	content = strings.TrimRight(content, "\r\n")
	return content
}

// wrapTokens wraps syntax-highlighted tokens into multiple lines
// while preserving syntax highlighting across line breaks.
func wrapTokens(tokens []chromaToken, maxWidth int) [][]chromaToken {
	if maxWidth <= 0 || len(tokens) == 0 {
		return [][]chromaToken{tokens}
	}

	var lines [][]chromaToken
	var currentLine []chromaToken
	currentWidth := 0

	for _, token := range tokens {
		text := token.Text
		for text != "" {
			// Calculate how many runes fit in remaining space
			spaceLeft := maxWidth - currentWidth
			if spaceLeft <= 0 {
				lines = append(lines, currentLine)
				currentLine = nil
				currentWidth = 0
				spaceLeft = maxWidth
			}

			// Find how much of the text fits
			fitLen, fitWidth := 0, 0
			for _, r := range text {
				rw := runewidth.RuneWidth(r)
				if fitWidth+rw > spaceLeft {
					break
				}
				fitLen += utf8.RuneLen(r)
				fitWidth += rw
			}

			if fitLen == 0 {
				// Current line has content but can't fit even one char - wrap first
				if currentWidth > 0 {
					lines = append(lines, currentLine)
					currentLine = nil
					currentWidth = 0
					continue
				}
				// Edge case: single char wider than maxWidth - take it anyway
				r, size := utf8.DecodeRuneInString(text)
				fitLen = size
				fitWidth = runewidth.RuneWidth(r)
			}

			currentLine = append(currentLine, chromaToken{Text: text[:fitLen], Style: token.Style})
			currentWidth += fitWidth
			text = text[fitLen:]
		}
	}

	if len(currentLine) > 0 {
		lines = append(lines, currentLine)
	}

	if len(lines) == 0 {
		return [][]chromaToken{tokens}
	}

	return lines
}

// renderSplitSide renders a split side with text wrapping support
func renderSplitSide(line *udiff.Line, lineNum int, filePath string, width int) []string {
	if line == nil {
		return []string{renderEmptySplitSide(width)}
	}

	content := prepareContent(line.Content)
	tokens := syntaxHighlight(content, filePath)
	lineStyle := getLineStyle(line.Kind)
	wrappedTokens := wrapTokens(tokens, width)

	var result []string
	for i, tokenLine := range wrappedTokens {
		var lineNumStr string
		if i == 0 {
			// Show line number only on first wrapped line
			lineNumStr = formatLineNum(line, lineNum)
		} else {
			// Use continuation indicator for wrapped lines
			lineNumStr = "   → "
		}
		rendered := renderTokensWithStyle(tokenLine, lineStyle)
		padded := padToWidth(rendered, width, lineStyle)
		result = append(result, styles.LineNumberStyle.Render(lineNumStr)+padded)
	}

	return result
}

// renderEmptySplitSide renders an empty line for split view alignment
func renderEmptySplitSide(width int) string {
	lineNumStr := strings.Repeat(" ", lineNumWidth)
	emptySpace := styles.DiffUnchangedStyle.Render(strings.Repeat(" ", width))
	return styles.LineNumberStyle.Render(lineNumStr) + emptySpace
}

func renderTokensWithStyle(tokens []chromaToken, lineStyle lipgloss.Style) string {
	var output strings.Builder

	for _, token := range tokens {
		styledToken := token.Style.Background(lineStyle.GetBackground())
		output.WriteString(styledToken.Render(token.Text))
	}

	return output.String()
}

func padToWidth(content string, width int, style lipgloss.Style) string {
	currentWidth := lipgloss.Width(content)
	if paddingNeeded := width - currentWidth; paddingNeeded > 0 {
		padding := strings.Repeat(" ", paddingNeeded)
		return content + style.Render(padding)
	}
	return content
}

func ensureWidth(line string, width int) string {
	if lineWidth := lipgloss.Width(line); lineWidth < width {
		padding := styles.DiffUnchangedStyle.Render(strings.Repeat(" ", width-lineWidth))
		return line + padding
	}
	return line
}

func getLineStyle(kind udiff.OpKind) lipgloss.Style {
	switch kind {
	case udiff.Delete:
		return styles.DiffRemoveStyle
	case udiff.Insert:
		return styles.DiffAddStyle
	default:
		return styles.DiffUnchangedStyle
	}
}

func formatLineNum(line *udiff.Line, lineNum int) string {
	if line == nil {
		return strings.Repeat(" ", lineNumWidth)
	}
	return fmt.Sprintf("%4d ", lineNum)
}

func pairDiffLines(lines []udiff.Line, fromLine, toLine int) []linePair {
	var pairs []linePair
	oldLineNum, newLineNum := fromLine, toLine

	for i := 0; i < len(lines); i++ {
		line := &lines[i]

		switch line.Kind {
		case udiff.Equal:
			pairs = append(pairs, linePair{
				old:        line,
				new:        line,
				oldLineNum: oldLineNum,
				newLineNum: newLineNum,
			})
			oldLineNum++
			newLineNum++

		case udiff.Delete:
			// Check if next line is an insert to pair them
			if i+1 < len(lines) && lines[i+1].Kind == udiff.Insert {
				pairs = append(pairs, linePair{
					old:        line,
					new:        &lines[i+1],
					oldLineNum: oldLineNum,
					newLineNum: newLineNum,
				})
				oldLineNum++
				newLineNum++
				i++ // Skip the paired insert
			} else {
				// Unpaired delete
				pairs = append(pairs, linePair{
					old:        line,
					new:        nil,
					oldLineNum: oldLineNum,
				})
				oldLineNum++
			}

		case udiff.Insert:
			// Unpaired insert (paired inserts are handled above)
			pairs = append(pairs, linePair{
				old:        nil,
				new:        line,
				newLineNum: newLineNum,
			})
			newLineNum++
		}
	}

	return pairs
}
