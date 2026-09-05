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
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	tabWidth     = 4
	lineNumWidth = 5
	minWidth     = 80

	// renderCacheSize bounds the number of cached edit_file renderings to
	// avoid unbounded memory growth in long sessions where the agent makes
	// many edits. Each entry holds a fully rendered diff string keyed by the
	// tool call ID, which is unique per call.
	renderCacheSize = 64
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

func renderEditFile(toolCall tools.ToolCall, width int, splitView bool, toolStatus types.ToolStatus) string {
	c := getOrCreateCache(toolCall.ID)

	cacheMu.Lock()
	if c.renderCached &&
		c.renderedWidth == width &&
		c.renderedSplit == splitView &&
		c.renderedStatus == toolStatus {
		result := c.rendered
		cacheMu.Unlock()
		return result
	}
	cacheMu.Unlock()

	result := renderEditFileUncached(toolCall, width, splitView, toolStatus)

	cacheMu.Lock()
	defer cacheMu.Unlock()
	c.rendered = result
	c.renderCached = true
	c.renderedWidth = width
	c.renderedSplit = splitView
	c.renderedStatus = toolStatus

	return result
}

func renderEditFileUncached(toolCall tools.ToolCall, width int, splitView bool, toolStatus types.ToolStatus) string {
	args, err := parseEditFileArgs(toolCall.Function.Arguments)
	if err != nil {
		return ""
	}

	if notice := refusalNotice(args, toolStatus); notice != "" {
		return notice
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

// refusalNotice returns the message to show in place of a diff when the edit
// the user is being asked to approve will be refused, or an empty string when
// the call is applicable.
//
// Without this, an ambiguous or empty oldText is previewed as a
// first-occurrence diff — strings.Replace(..., 1) always produces one — so the
// user approves a change the tool then declines, and the diff they saw was
// never the change that would have been made.
//
// Only meaningful before execution: once the tool has run, the file on disk is
// the outcome and there is nothing left to predict.
func refusalNotice(args filesystem.EditFileArgs, toolStatus types.ToolStatus) string {
	if toolStatus != types.ToolStatusConfirmation {
		return ""
	}

	content, err := os.ReadFile(args.Path)
	if err != nil {
		// The tool reports unreadable paths itself; a preview must not
		// second-guess it.
		return ""
	}

	// Evaluated against the running content exactly as handleEditFile does, so
	// a preview never refuses a call the tool would accept: an earlier edit may
	// legitimately remove a duplicate that made a later one ambiguous.
	running := string(content)
	for i, edit := range args.Edits {
		if reason := filesystem.EditFailureReason(running, edit); reason != "" {
			return styles.ErrorStyle.Render(
				fmt.Sprintf("This call will be refused — Edit %d: %s", i+1, reason))
		}
		running = strings.Replace(running, edit.OldText, edit.NewText, 1)
	}
	return ""
}

// countDiffLines returns the number of added and removed lines for the edit.
// Results are cached per tool call since arguments are immutable.
func countDiffLines(toolCall tools.ToolCall, _ types.ToolStatus) (added, removed int) {
	c := getOrCreateCache(toolCall.ID)

	cacheMu.Lock()
	if c.lineCounted {
		added, removed = c.added, c.removed
		cacheMu.Unlock()
		return added, removed
	}
	cacheMu.Unlock()

	added, removed = countDiffLinesUncached(toolCall)

	cacheMu.Lock()
	defer cacheMu.Unlock()
	c.added = added
	c.removed = removed
	c.lineCounted = true

	return added, removed
}

func countDiffLinesUncached(toolCall tools.ToolCall) (added, removed int) {
	args, err := parseEditFileArgs(toolCall.Function.Arguments)
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
	contentWidth := width - lineNumWidth

	for _, hunk := range diff {
		// Build word-diff lookups for paired delete/insert lines so we can
		// emphasize the precise tokens that changed.
		wordDiffs := buildLineWordDiffs(hunk.Lines)

		oldLineNum := hunk.FromLine
		newLineNum := hunk.ToLine

		for li, line := range hunk.Lines {
			lineNum := getDisplayLineNumber(&line, &oldLineNum, &newLineNum)
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
				var lineNumStr string
				if i == 0 {
					// Show line number only on first wrapped line
					lineNumStr = styles.LineNumberStyle.Render(fmt.Sprintf("%4d ", lineNum))
				} else {
					// Use continuation indicator for wrapped lines
					lineNumStr = styles.LineNumberStyle.Render("   → ")
				}
				rendered := renderTokensWithStyle(tokenLine, lineStyle, line.Kind)
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
			// Word-diff is only meaningful when both halves are present
			// and represent a delete/insert pair. Inputs go through
			// prepareContent so the segment byte offsets line up with the
			// chroma tokens produced for rendering (which also receive
			// tab-expanded content).
			var oldSegs, newSegs []wordSegment
			if pair.old != nil && pair.new != nil &&
				pair.old.Kind == udiff.Delete && pair.new.Kind == udiff.Insert {
				oldSegs, newSegs = diffWords(
					prepareContent(pair.old.Content),
					prepareContent(pair.new.Content),
				)
			}

			leftLines := renderSplitSide(pair.old, pair.oldLineNum, filePath, contentWidth, oldSegs)
			rightLines := renderSplitSide(pair.new, pair.newLineNum, filePath, contentWidth, newSegs)

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

// lineWordDiff holds the per-side segment arrays computed for one
// delete/insert pair. It is keyed by the *delete* line index — the matching
// insert sits directly after it.
type lineWordDiff struct {
	old []wordSegment
	new []wordSegment
}

func buildLineWordDiffs(lines []udiff.Line) map[int]lineWordDiff {
	out := map[int]lineWordDiff{}
	for i := range len(lines) - 1 {
		if lines[i].Kind != udiff.Delete || lines[i+1].Kind != udiff.Insert {
			continue
		}
		// Use prepareContent so segment offsets align with the chroma
		// tokens (which are produced from the same tab-expanded text).
		oldText := prepareContent(lines[i].Content)
		newText := prepareContent(lines[i+1].Content)
		oldSegs, newSegs := diffWords(oldText, newText)
		wd := lineWordDiff{old: oldSegs, new: newSegs}
		out[i] = wd
		out[i+1] = wd
	}
	return out
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
func renderSplitSide(line *udiff.Line, lineNum int, filePath string, width int, wordSegs []wordSegment) []string {
	if line == nil {
		return []string{renderEmptySplitSide(width)}
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
		var lineNumStr string
		if i == 0 {
			// Show line number only on first wrapped line
			lineNumStr = formatLineNum(line, lineNum)
		} else {
			// Use continuation indicator for wrapped lines
			lineNumStr = "   → "
		}
		rendered := renderTokensWithStyle(tokenLine, lineStyle, line.Kind)
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

func renderTokensWithStyle(tokens []chromaToken, lineStyle lipgloss.Style, kind udiff.OpKind) string {
	var output strings.Builder

	emph := emphasisStyleFor(kind)
	for _, token := range tokens {
		if token.Emphasized && (kind == udiff.Delete || kind == udiff.Insert) {
			// Keep the chroma foreground so syntax colors carry through into
			// the emphasized block, but override the background with the
			// stronger emphasis tint and add bold for extra weight.
			style := token.Style.Background(emph.GetBackground()).Bold(true)
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
