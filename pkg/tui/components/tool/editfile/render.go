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

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/lrucache"
	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	tabWidth            = 4
	minLineNumWidth     = 4
	signWidth           = 2 // "- ", "+ ", or "  "
	unifiedDualMinWidth = 60
	minWidth            = 80

	// renderCacheSize bounds the number of cached edit_file renderings to
	// avoid unbounded memory growth in long sessions where the agent makes
	// many edits. Each entry holds a fully rendered diff string keyed by the
	// tool call ID, which is unique per call.
	renderCacheSize = 64
)

type toolRenderCache struct {
	// Rendered output - invalidated when width/splitView/status changes
	rendered       string
	renderCached   bool
	renderedWidth  int
	renderedSplit  bool
	renderedStatus types.ToolStatus
	renderedResult *tools.ToolCallResult
}

var (
	// cacheMu guards both the LRU and the per-entry fields. A regular Mutex
	// (not RWMutex) is used because LRU.Get mutates the recency list.
	cache   = lrucache.New[string, *toolRenderCache](renderCacheSize)
	cacheMu sync.Mutex

	lexerCache concurrent.Map[string, chroma.Lexer]
)

// InvalidateCaches clears all render caches.
// Call this when the theme changes to pick up new colors.
func InvalidateCaches() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache.Range(func(_ string, c *toolRenderCache) bool {
		c.renderCached = false
		return true
	})
}

type chromaToken struct {
	Text  string
	Style lipgloss.Style
	// Emphasized marks tokens whose underlying text was identified as part
	// of a word-level diff. Renderers paint these with a stronger background
	// so users can locate the precise edit within a long line.
	Emphasized bool
}

type linePair struct {
	old        *udiff.Line
	new        *udiff.Line
	oldLineNum int
	newLineNum int
}

func getOrCreateCache(toolCallID string) *toolRenderCache {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if c, ok := cache.Get(toolCallID); ok {
		return c
	}
	c := &toolRenderCache{}
	cache.Put(toolCallID, c)
	return c
}

func renderEditFile(msg *types.Message, width int, splitView bool) string {
	c := getOrCreateCache(msg.ToolCall.ID)

	cacheMu.Lock()
	if c.renderCached &&
		c.renderedWidth == width &&
		c.renderedSplit == splitView &&
		c.renderedStatus == msg.ToolStatus &&
		c.renderedResult == msg.ToolResult {
		result := c.rendered
		cacheMu.Unlock()
		return result
	}
	cacheMu.Unlock()

	result := renderEditFileUncached(msg, width, splitView)

	cacheMu.Lock()
	defer cacheMu.Unlock()
	c.rendered = result
	c.renderCached = true
	c.renderedWidth = width
	c.renderedSplit = splitView
	c.renderedStatus = msg.ToolStatus
	c.renderedResult = msg.ToolResult

	return result
}

func renderEditFileUncached(msg *types.Message, width int, splitView bool) string {
	meta, ok := editFileDiff(msg)
	if !ok {
		return ""
	}
	var result string
	if splitView {
		result = renderSplitDiffWithSyntaxHighlight(meta.Hunks, meta.Path, width)
	} else {
		result = renderDiffWithSyntaxHighlight(meta.Hunks, meta.Path, width)
	}
	if meta.Truncated {
		if result != "" {
			result += "\n"
		}
		result += styles.ToolMessageStyle.Render("Diff truncated")
	}
	return result
}

func countDiffLines(msg *types.Message) (added, removed int) {
	meta, ok := editFileDiff(msg)
	if !ok {
		return 0, 0
	}
	if meta.Truncated {
		return meta.Added, meta.Removed
	}
	for _, hunk := range meta.Hunks {
		for _, line := range hunk.Lines {
			switch line.Kind {
			case udiff.Insert:
				added++
			case udiff.Delete:
				removed++
			}
		}
	}
	return added, removed
}

func editFileDiff(msg *types.Message) (filesystem.EditFileMeta, bool) {
	if msg.ToolResult != nil {
		return filesystem.DecodeEditFileMeta(msg.ToolResult.Meta)
	}
	if msg.ToolStatus != types.ToolStatusConfirmation {
		return filesystem.EditFileMeta{}, false
	}

	args, err := parseEditFileArgs(msg.ToolCall.Function.Arguments)
	if err != nil {
		return filesystem.EditFileMeta{}, false
	}
	content, err := os.ReadFile(args.Path)
	if err != nil {
		return filesystem.EditFileMeta{}, false
	}
	after, err := filesystem.ApplyEdits(string(content), args.Edits)
	if err != nil {
		return filesystem.EditFileMeta{}, false
	}
	meta, err := filesystem.NewEditFileMeta(args.Path, string(content), after)
	return meta, err == nil
}

func syntaxHighlight(code, filePath string) []chromaToken {
	ext := filepath.Ext(filePath)

	lexer, ok := lexerCache.Load(ext)
	if !ok {
		lexer = lexers.Match(filePath)
		if lexer == nil {
			lexer = lexers.Fallback
		}
		lexer = chroma.Coalesce(lexer)
		lexerCache.Store(ext, lexer)
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

// segRange records the byte extent of a single word-diff segment within the
// full line, plus whether that extent represents a change.
type segRange struct {
	start, end int
	changed    bool
}

// applyWordEmphasis re-tags chroma tokens so that any portion of the token
// text that falls inside a "changed" word-diff segment is split off into its
// own emphasized token. Token boundaries from chroma and from the word-diff
// rarely line up, so each chroma token is sliced against the segment cursor
// to produce correctly emphasized sub-tokens.
func applyWordEmphasis(tokens []chromaToken, segs []wordSegment) []chromaToken {
	if len(segs) == 0 {
		return tokens
	}

	ranges := make([]segRange, 0, len(segs))
	pos := 0
	for _, s := range segs {
		ranges = append(ranges, segRange{start: pos, end: pos + len(s.Text), changed: s.Changed})
		pos += len(s.Text)
	}

	out := make([]chromaToken, 0, len(tokens))
	bytePos := 0
	for _, tok := range tokens {
		text := tok.Text
		tokStart := bytePos
		tokEnd := bytePos + len(text)

		cursor := tokStart
		for cursor < tokEnd {
			r := findSegRange(ranges, cursor)
			if r == nil {
				out = append(out, chromaToken{
					Text:  text[cursor-tokStart:],
					Style: tok.Style,
				})
				break
			}
			end := min(tokEnd, r.end)
			sub := text[cursor-tokStart : end-tokStart]
			if sub != "" {
				out = append(out, chromaToken{
					Text:       sub,
					Style:      tok.Style,
					Emphasized: r.changed,
				})
			}
			cursor = end
		}
		bytePos = tokEnd
	}

	return out
}

func findSegRange(ranges []segRange, pos int) *segRange {
	for i := range ranges {
		if pos >= ranges[i].start && pos < ranges[i].end {
			return &ranges[i]
		}
	}
	return nil
}

// emphasisStyleFor returns the per-side emphasis style (with a stronger
// background tint) for the row's diff kind. Returns the unchanged style for
// kinds that should never carry word-level emphasis.
func emphasisStyleFor(kind udiff.OpKind) lipgloss.Style {
	switch kind {
	case udiff.Delete:
		return styles.DiffRemoveEmphStyle
	case udiff.Insert:
		return styles.DiffAddEmphStyle
	default:
		return styles.DiffUnchangedStyle
	}
}

func renderDiffWithSyntaxHighlight(diff []*udiff.Hunk, filePath string, width int) string {
	var output strings.Builder
	numWidth := maxLineNumWidth(diff)
	dualNumbers := width >= unifiedDualMinWidth
	gutterWidth := numWidth + 1 + signWidth
	if dualNumbers {
		gutterWidth = 2*(numWidth+1) + signWidth
	}
	contentWidth := max(width-gutterWidth, 1)

	for hi, hunk := range diff {
		if hi > 0 {
			output.WriteString(renderHunkHeader(hunk, width) + "\n")
		}

		// Build word-diff lookups for paired delete/insert lines so we can
		// emphasize the precise tokens that changed.
		wordDiffs := buildLineWordDiffs(hunk.Lines)

		oldLineNum := hunk.FromLine
		newLineNum := hunk.ToLine

		for li, line := range hunk.Lines {
			oldNum, newNum := advanceLineNumbers(&line, &oldLineNum, &newLineNum)
			content := prepareContent(line.Content)
			tokens := syntaxHighlight(content, filePath)
			if wd, ok := wordDiffs[li]; ok {
				switch line.Kind {
				case udiff.Delete:
					tokens = applyWordEmphasis(tokens, wd.old)
				case udiff.Insert:
					tokens = applyWordEmphasis(tokens, wd.new)
				}
			}
			lineStyle := getLineStyle(line.Kind)
			wrappedTokens := wrapTokens(tokens, contentWidth)

			for i, tokenLine := range wrappedTokens {
				gutter := renderUnifiedGutter(line.Kind, oldNum, newNum, numWidth, i > 0, dualNumbers)
				rendered := renderTokensWithStyle(tokenLine, lineStyle, line.Kind)
				padded := padToWidth(rendered, contentWidth, lineStyle)
				output.WriteString(gutter + padded + "\n")
			}
		}
	}

	return strings.TrimSuffix(output.String(), "\n")
}

func renderSplitDiffWithSyntaxHighlight(diff []*udiff.Hunk, filePath string, width int) string {
	// Fall back to unified diff if terminal is too narrow
	numWidth := maxLineNumWidth(diff)
	gutterWidth := numWidth + 1 + signWidth
	separator := styles.SeparatorStyle.Render("│")
	separatorWidth := lipgloss.Width(separator)
	contentWidth := (width - separatorWidth - (gutterWidth * 2)) / 2

	if width < minWidth || contentWidth < 10 {
		return renderDiffWithSyntaxHighlight(diff, filePath, width)
	}

	var output strings.Builder

	for hi, hunk := range diff {
		if hi > 0 {
			output.WriteString(renderHunkHeader(hunk, width) + "\n")
		}

		for _, pair := range pairDiffLines(hunk.Lines, hunk.FromLine, hunk.ToLine) {
			// Word-diff is only meaningful when both halves are present
			// and represent a similar delete/insert pair. Inputs go through
			// prepareContent so the segment byte offsets line up with the
			// chroma tokens produced for rendering (which also receive
			// tab-expanded content).
			var oldSegs, newSegs []wordSegment
			if pair.old != nil && pair.new != nil &&
				pair.old.Kind == udiff.Delete && pair.new.Kind == udiff.Insert {
				oldSegs, newSegs = diffWordsForDisplay(
					prepareContent(pair.old.Content),
					prepareContent(pair.new.Content),
				)
			}

			leftLines := renderSplitSide(pair.old, pair.oldLineNum, numWidth, filePath, contentWidth, oldSegs)
			rightLines := renderSplitSide(pair.new, pair.newLineNum, numWidth, filePath, contentWidth, newSegs)

			// Ensure both sides have the same number of lines for alignment
			maxLines := max(len(rightLines), len(leftLines))

			// Pad shorter side with empty lines
			for len(leftLines) < maxLines {
				leftLines = append(leftLines, renderEmptySplitSide(numWidth, contentWidth))
			}
			for len(rightLines) < maxLines {
				rightLines = append(rightLines, renderEmptySplitSide(numWidth, contentWidth))
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

// lineWordDiff holds the per-side segment arrays for a paired replacement.
type lineWordDiff struct {
	old []wordSegment
	new []wordSegment
}

func buildLineWordDiffs(lines []udiff.Line) map[int]lineWordDiff {
	out := map[int]lineWordDiff{}
	indices := make(map[*udiff.Line]int, len(lines))
	for i := range lines {
		indices[&lines[i]] = i
	}
	for _, pair := range pairDiffLines(lines, 0, 0) {
		if pair.old == nil || pair.new == nil || pair.old.Kind != udiff.Delete || pair.new.Kind != udiff.Insert {
			continue
		}
		oldSegs, newSegs := diffWordsForDisplay(prepareContent(pair.old.Content), prepareContent(pair.new.Content))
		if len(oldSegs) == 0 && len(newSegs) == 0 {
			continue
		}
		wd := lineWordDiff{old: oldSegs, new: newSegs}
		out[indices[pair.old]] = wd
		out[indices[pair.new]] = wd
	}
	return out
}

func advanceLineNumbers(line *udiff.Line, oldLineNum, newLineNum *int) (oldNum, newNum int) {
	switch line.Kind {
	case udiff.Delete:
		oldNum = *oldLineNum
		*oldLineNum++
	case udiff.Insert:
		newNum = *newLineNum
		*newLineNum++
	case udiff.Equal:
		oldNum = *oldLineNum
		newNum = *newLineNum
		*oldLineNum++
		*newLineNum++
	}
	return oldNum, newNum
}

func renderHunkHeader(hunk *udiff.Hunk, width int) string {
	if width <= 0 {
		return ""
	}
	var fromCount, toCount int
	for _, l := range hunk.Lines {
		switch l.Kind {
		case udiff.Delete:
			fromCount++
		case udiff.Insert:
			toCount++
		case udiff.Equal:
			fromCount++
			toCount++
		}
	}
	header := fmt.Sprintf(" @@ -%d,%d +%d,%d @@", hunk.FromLine, fromCount, hunk.ToLine, toCount)
	header = runewidth.Truncate(header, width, "…")
	return padToWidth(styles.DiffHunkHeaderStyle.Render(header), width, styles.DiffHunkHeaderStyle)
}

func maxLineNumWidth(diff []*udiff.Hunk) int {
	maxNum := 0
	for _, hunk := range diff {
		oldNum, newNum := hunk.FromLine, hunk.ToLine
		for _, line := range hunk.Lines {
			switch line.Kind {
			case udiff.Delete:
				maxNum = max(maxNum, oldNum)
				oldNum++
			case udiff.Insert:
				maxNum = max(maxNum, newNum)
				newNum++
			case udiff.Equal:
				maxNum = max(maxNum, oldNum, newNum)
				oldNum++
				newNum++
			}
		}
	}
	width := minLineNumWidth
	for n := maxNum; n >= 10000; n /= 10 {
		width++
	}
	return width
}

func renderUnifiedGutter(kind udiff.OpKind, oldNum, newNum, numWidth int, continuation, dual bool) string {
	gutterStyle := getGutterStyle(kind)
	lineStyle := getLineStyle(kind)

	var numPart string
	switch {
	case continuation && dual:
		numPart = strings.Repeat(" ", 2*numWidth) + "↪ "
	case continuation:
		numPart = strings.Repeat(" ", numWidth-1) + "↪ "
	case dual:
		blank := strings.Repeat(" ", numWidth+1)
		oldStr, newStr := blank, blank
		if oldNum > 0 {
			oldStr = fmt.Sprintf("%*d ", numWidth, oldNum)
		}
		if newNum > 0 {
			newStr = fmt.Sprintf("%*d ", numWidth, newNum)
		}
		numPart = oldStr + newStr
	default:
		n := newNum
		if kind == udiff.Delete {
			n = oldNum
		}
		numPart = fmt.Sprintf("%*d ", numWidth, n)
	}

	sign := "  "
	if !continuation {
		sign = diffSign(kind)
	}
	return gutterStyle.Render(numPart) + lineStyle.Render(sign)
}

func renderSplitGutter(kind udiff.OpKind, lineNum, numWidth int, continuation bool) string {
	gutterStyle := getGutterStyle(kind)
	lineStyle := getLineStyle(kind)

	if continuation {
		return gutterStyle.Render(strings.Repeat(" ", numWidth-1)+"↪ ") + lineStyle.Render("  ")
	}
	return gutterStyle.Render(fmt.Sprintf("%*d ", numWidth, lineNum)) + lineStyle.Render(diffSign(kind))
}

func diffSign(kind udiff.OpKind) string {
	switch kind {
	case udiff.Delete:
		return "- "
	case udiff.Insert:
		return "+ "
	default:
		return "  "
	}
}

func getGutterStyle(kind udiff.OpKind) lipgloss.Style {
	switch kind {
	case udiff.Delete:
		return styles.DiffRemoveGutterStyle
	case udiff.Insert:
		return styles.DiffAddGutterStyle
	default:
		return styles.LineNumberStyle
	}
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

			// Find how much of the text fits. Decode explicitly: `range`
			// reports an invalid byte as U+FFFD while consuming one source
			// byte, and utf8.RuneLen(U+FFFD) is 3, which would overrun text.
			fitLen, fitWidth := 0, 0
			for fitLen < len(text) {
				r, size := utf8.DecodeRuneInString(text[fitLen:])
				rw := runewidth.RuneWidth(r)
				if fitWidth+rw > spaceLeft {
					break
				}
				fitLen += size
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

			currentLine = append(currentLine, chromaToken{
				Text:       text[:fitLen],
				Style:      token.Style,
				Emphasized: token.Emphasized,
			})
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

// renderSplitSide renders a split side with text wrapping support.
// When wordSegs is non-nil and the line is part of a delete/insert pair, the
// chroma tokens are re-tagged so the changed substrings render with a
// stronger background tint.
func renderSplitSide(line *udiff.Line, lineNum, numWidth int, filePath string, width int, wordSegs []wordSegment) []string {
	if line == nil {
		return []string{renderEmptySplitSide(numWidth, width)}
	}

	content := prepareContent(line.Content)
	tokens := syntaxHighlight(content, filePath)
	if len(wordSegs) > 0 {
		tokens = applyWordEmphasis(tokens, wordSegs)
	}
	lineStyle := getLineStyle(line.Kind)
	wrappedTokens := wrapTokens(tokens, width)

	var result []string
	for i, tokenLine := range wrappedTokens {
		gutter := renderSplitGutter(line.Kind, lineNum, numWidth, i > 0)
		rendered := renderTokensWithStyle(tokenLine, lineStyle, line.Kind)
		padded := padToWidth(rendered, width, lineStyle)
		result = append(result, gutter+padded)
	}

	return result
}

// renderEmptySplitSide renders an empty line for split view alignment
func renderEmptySplitSide(numWidth, width int) string {
	gutter := styles.LineNumberStyle.Render(strings.Repeat(" ", numWidth+1+signWidth))
	emptySpace := styles.DiffUnchangedStyle.Render(strings.Repeat(" ", width))
	return gutter + emptySpace
}

func renderTokensWithStyle(tokens []chromaToken, lineStyle lipgloss.Style, kind udiff.OpKind) string {
	var output strings.Builder

	emph := emphasisStyleFor(kind)
	for _, token := range tokens {
		if token.Emphasized && (kind == udiff.Delete || kind == udiff.Insert) {
			style := token.Style.Background(emph.GetBackground())
			output.WriteString(style.Render(token.Text))
			continue
		}
		style := token.Style.Background(lineStyle.GetBackground())
		output.WriteString(style.Render(token.Text))
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
			deleteEnd := i
			for deleteEnd < len(lines) && lines[deleteEnd].Kind == udiff.Delete {
				deleteEnd++
			}
			insertEnd := deleteEnd
			for insertEnd < len(lines) && lines[insertEnd].Kind == udiff.Insert {
				insertEnd++
			}
			for offset := range max(deleteEnd-i, insertEnd-deleteEnd) {
				var pair linePair
				if i+offset < deleteEnd {
					pair.old = &lines[i+offset]
					pair.oldLineNum = oldLineNum
					oldLineNum++
				}
				if deleteEnd+offset < insertEnd {
					pair.new = &lines[deleteEnd+offset]
					pair.newLineNum = newLineNum
					newLineNum++
				}
				pairs = append(pairs, pair)
			}
			i = insertEnd - 1

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
