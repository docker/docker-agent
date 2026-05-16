// Package markdown provides a high-performance markdown renderer for terminal output.
// This is a custom implementation optimized for speed, replacing glamour for TUI rendering.
package markdown

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	xansi "github.com/charmbracelet/x/ansi"
	runewidth "github.com/mattn/go-runewidth"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ansiStyle holds pre-computed ANSI escape sequences for fast rendering.
// This avoids the overhead of lipgloss.Style.Render() which copies large structs.
// Flags track formatting attributes to avoid string scanning in hot paths.
type ansiStyle struct {
	prefix    string // ANSI codes to start the style
	suffix    string // ANSI codes to end the style (reset)
	hasBold   bool   // Whether this style includes bold
	hasStrike bool   // Whether this style includes strikethrough
	hasItalic bool   // Whether this style includes italic
}

func (s ansiStyle) render(text string) string {
	if s.prefix == "" {
		return text
	}
	return s.prefix + text + s.suffix
}

// renderTo writes styled text directly to a builder, avoiding intermediate allocations
func (s ansiStyle) renderTo(b *strings.Builder, text string) {
	if s.prefix == "" {
		b.WriteString(text)
		return
	}
	b.WriteString(s.prefix)
	b.WriteString(text)
	b.WriteString(s.suffix)
}

// withAttribute returns a new style with the given ANSI attribute layered on
// top of s while preserving s's color/background. on is the SGR sequence that
// turns the attribute on (e.g. "\x1b[1m" for bold); off is the matching off
// sequence (e.g. "\x1b[22m"). When s has no styling the off sequence is
// replaced with a full reset, matching the behaviour of a fresh style.
//
// Format attributes are written before the parent's color/background so that
// terminals which auto-brighten bold colors do not override our color choice.
func (s ansiStyle) withAttribute(on, off string) ansiStyle {
	r := s
	if s.prefix == "" {
		r.prefix = on
		r.suffix = "\x1b[m"
	} else {
		r.prefix = on + s.prefix
		r.suffix = off + s.prefix
	}
	return r
}

func (s ansiStyle) withBold() ansiStyle {
	r := s.withAttribute("\x1b[1m", "\x1b[22m")
	r.hasBold = true
	return r
}

func (s ansiStyle) withItalic() ansiStyle {
	r := s.withAttribute("\x1b[3m", "\x1b[23m")
	r.hasItalic = true
	return r
}

func (s ansiStyle) withBoldItalic() ansiStyle {
	r := s.withAttribute("\x1b[1;3m", "\x1b[22;23m")
	r.hasBold = true
	r.hasItalic = true
	return r
}

func (s ansiStyle) withStrikethrough() ansiStyle {
	r := s.withAttribute("\x1b[9m", "\x1b[29m")
	r.hasStrike = true
	return r
}

// buildAnsiStyle extracts ANSI codes from a lipgloss style by rendering an empty marker.
func buildAnsiStyle(style lipgloss.Style) ansiStyle {
	// Render a marker to extract the ANSI prefix/suffix
	const marker = "\x00"
	rendered := style.Render(marker)
	before, after, ok := strings.Cut(rendered, marker)
	if !ok {
		return ansiStyle{}
	}

	return ansiStyle{
		prefix: before,
		suffix: after,
	}
}

// cachedStyles holds pre-computed styles to avoid repeated MarkdownStyle() calls.
// Everything that can be styled once is pre-rendered to ANSI strings, so the
// hot path never touches lipgloss.
type cachedStyles struct {
	styleCodeBg lipgloss.Style // kept only because chroma styles inherit its bg color

	// ANSI styles (for fast inline rendering)
	ansiBold              ansiStyle
	ansiItalic            ansiStyle
	ansiBoldItal          ansiStyle
	ansiStrike            ansiStyle
	ansiCode              ansiStyle
	ansiLink              ansiStyle
	ansiLinkText          ansiStyle
	ansiText              ansiStyle    // base document text style
	ansiHeadings          [6]ansiStyle // heading styles for inline restoration
	ansiBlockquote        ansiStyle    // blockquote style for inline restoration
	ansiFootnote          ansiStyle    // footnote reference style
	ansiCodeBg            ansiStyle    // code block background (cached to avoid repeated buildAnsiStyle)
	ansiCodeBlockCopyIcon ansiStyle    // muted foreground on code block background, used for the per-block copy icon

	// Pre-rendered chrome (computed once, reused across renders)
	headingPrefixes         [6]string // raw prefix strings (e.g. "## ") for width math
	styledHeadingPrefixes   [6]string // ANSI-styled prefix used at start of heading
	styledHeadingContIndent [6]string // ANSI-styled spaces for heading continuation lines
	styledHR                string    // pre-rendered horizontal rule (with trailing blank line)
	styledTableSep          string    // styled " │ " for table columns

	styleTaskTicked  string
	styleTaskUntick  string
	listIndent       int
	blockquoteIndent int
	chromaStyle      *chroma.Style
}

var (
	globalStyles     *cachedStyles
	globalStylesOnce sync.Once
	globalStylesMu   sync.Mutex
)

// ResetStyles resets the cached markdown styles so they will be rebuilt on next use.
// Call this when the theme changes to pick up new colors.
func ResetStyles() {
	globalStylesMu.Lock()
	globalStyles = nil
	globalStylesOnce = sync.Once{}
	globalStylesMu.Unlock()

	// Also clear chroma syntax highlighting caches
	chromaStyleCacheMu.Lock()
	chromaStyleCache = make(map[chroma.TokenType]ansiStyle)
	chromaStyleCacheMu.Unlock()

	syntaxHighlightCacheMu.Lock()
	syntaxHighlightCache.clear()
	syntaxHighlightCacheMu.Unlock()
}

func getGlobalStyles() *cachedStyles {
	globalStylesMu.Lock()
	defer globalStylesMu.Unlock()

	globalStylesOnce.Do(func() {
		mdStyle := styles.MarkdownStyle()

		styleBold := buildStylePrimitive(mdStyle.Strong)
		styleItalic := buildStylePrimitive(mdStyle.Emph)

		textStyle := buildStylePrimitive(mdStyle.Document.StylePrimitive)

		// Build heading lipgloss styles - always include bold for consistency
		headingLipStyles := [6]lipgloss.Style{
			buildStylePrimitive(mdStyle.H1.StylePrimitive).Bold(true),
			buildStylePrimitive(mdStyle.H2.StylePrimitive).Bold(true),
			buildStylePrimitive(mdStyle.H3.StylePrimitive).Bold(true),
			buildStylePrimitive(mdStyle.H4.StylePrimitive).Bold(true),
			buildStylePrimitive(mdStyle.H5.StylePrimitive).Bold(true),
			buildStylePrimitive(mdStyle.H6.StylePrimitive).Bold(true),
		}

		// Build blockquote lipgloss style
		blockquoteLipStyle := buildStylePrimitive(mdStyle.BlockQuote.StylePrimitive)

		// Heading prefixes used both for display and width math. H1 deliberately
		// uses the same "## " prefix as H2 so a single hash in the source does not
		// dominate the TUI with an oversized banner heading.
		// Note: H1 uses "# " (single hash) for proper visual hierarchy and correct
		// continuation-indent width (2 spaces instead of 3).
		headingPrefixes := [6]string{"# ", "## ", "### ", "#### ", "##### ", "###### "}
		ansiHeadings := [6]ansiStyle{
			buildAnsiStyle(headingLipStyles[0]),
			buildAnsiStyle(headingLipStyles[1]),
			buildAnsiStyle(headingLipStyles[2]),
			buildAnsiStyle(headingLipStyles[3]),
			buildAnsiStyle(headingLipStyles[4]),
			buildAnsiStyle(headingLipStyles[5]),
		}
		for i := range ansiHeadings {
			ansiHeadings[i].hasBold = true
		}

		var styledPrefixes, styledContIndents [6]string
		for i, p := range headingPrefixes {
			styledPrefixes[i] = ansiHeadings[i].render(p)
			styledContIndents[i] = ansiHeadings[i].render(spaces(len(p)))
		}

		codeBg := lipgloss.NewStyle()
		if mdStyle.CodeBlock.BackgroundColor != nil {
			codeBg = codeBg.Background(lipgloss.Color(*mdStyle.CodeBlock.BackgroundColor))
		}
		ansiText := buildAnsiStyle(textStyle)

		blockquoteIndent := 1
		if mdStyle.BlockQuote.Indent != nil {
			blockquoteIndent = int(*mdStyle.BlockQuote.Indent)
		}

		globalStyles = &cachedStyles{
			styleCodeBg:             codeBg,
			ansiBold:                buildAnsiStyle(styleBold),
			ansiItalic:              buildAnsiStyle(styleItalic),
			ansiBoldItal:            buildAnsiStyle(styleBold.Inherit(styleItalic)),
			ansiStrike:              buildAnsiStyle(buildStylePrimitive(mdStyle.Strikethrough)),
			ansiCode:                buildAnsiStyle(buildStylePrimitive(mdStyle.Code.StylePrimitive)),
			ansiLink:                buildAnsiStyle(buildStylePrimitive(mdStyle.Link)),
			ansiLinkText:            buildAnsiStyle(buildStylePrimitive(mdStyle.LinkText)),
			ansiText:                ansiText,
			ansiHeadings:            ansiHeadings,
			ansiBlockquote:          buildAnsiStyle(blockquoteLipStyle),
			ansiFootnote:            buildAnsiStyle(lipgloss.NewStyle().Foreground(styles.TextSecondary).Italic(true)),
			ansiCodeBg:              buildAnsiStyle(codeBg),
			ansiCodeBlockCopyIcon:   buildAnsiStyle(codeBg.Foreground(styles.TextMutedGray)),
			headingPrefixes:         headingPrefixes,
			styledHeadingPrefixes:   styledPrefixes,
			styledHeadingContIndent: styledContIndents,
			styledHR:                buildAnsiStyle(buildStylePrimitive(mdStyle.HorizontalRule)).render("--------") + "\n\n",
			styledTableSep:          ansiText.render(" │ "),
			styleTaskTicked:         mdStyle.Task.Ticked,
			styleTaskUntick:         mdStyle.Task.Unticked,
			listIndent:              int(mdStyle.List.LevelIndent),
			blockquoteIndent:        blockquoteIndent,
			chromaStyle:             styles.ChromaStyle(),
		}
	})
	return globalStyles
}

// FastRenderer is a high-performance markdown renderer optimized for terminal output.
// It directly parses and renders markdown without building an intermediate AST.
type FastRenderer struct {
	width int
}

// NewFastRenderer creates a new fast markdown renderer with the given width.
func NewFastRenderer(width int) *FastRenderer {
	return &FastRenderer{width: width}
}

var parserPool = sync.Pool{
	New: func() any {
		return &parser{
			out: strings.Builder{},
		}
	},
}

// Render parses and renders markdown content to styled terminal output.
func (r *FastRenderer) Render(input string) (string, error) {
	out, _, err := r.RenderWithCodeBlocks(input)
	return out, err
}

// RenderWithCodeBlocks renders markdown content and returns both the styled
// terminal output and the list of fenced code blocks emitted, in document
// order. Each entry's Line points at the rendered line that carries the
// clickable copy label for that block.
func (r *FastRenderer) RenderWithCodeBlocks(input string) (string, []CodeBlock, error) {
	if input == "" {
		return "", nil, nil
	}

	input = sanitizeForTerminal(input)

	p := parserPool.Get().(*parser)
	p.reset(input, r.width)
	result := p.parse()
	var blocks []CodeBlock
	if len(p.codeBlocks) > 0 {
		blocks = make([]CodeBlock, len(p.codeBlocks))
		copy(blocks, p.codeBlocks)
	}
	parserPool.Put(p)
	return finalizeOutput(result, r.width), blocks, nil
}

// parser holds the state for parsing markdown.
type parser struct {
	input      string
	width      int
	styles     *cachedStyles
	out        strings.Builder
	lines      []string
	lineIdx    int
	codeBlocks []CodeBlock
}

func (p *parser) reset(input string, width int) {
	p.input = input
	p.width = width
	p.styles = getGlobalStyles()
	// Reuse lines slice capacity to avoid allocation
	p.lines = p.lines[:0]
	for line := range strings.SplitSeq(input, "\n") {
		p.lines = append(p.lines, line)
	}
	p.lineIdx = 0
	p.codeBlocks = p.codeBlocks[:0]
	p.out.Reset()
	p.out.Grow(len(input) * 2) // Pre-allocate for styled output
}

func (p *parser) parse() string {
	for p.lineIdx < len(p.lines) {
		line := p.lines[p.lineIdx]

		switch {
		case p.tryCodeBlock(line):
			// handled inside
		case p.tryHeading(line):
			// handled inside
		case p.tryHorizontalRule(line):
			// handled inside
		case p.tryBlockquote(line):
			// handled inside
		case p.tryTable(line):
			// handled inside
		case p.tryList(line):
			// handled inside
		case p.tryFootnoteDefinition(line):
			// handled inside
		default:
			// Regular paragraph
			p.renderParagraph()
		}
	}

	return strings.TrimRight(p.out.String(), "\n")
}

// tryCodeBlock checks for fenced code blocks (``` or ~~~)
func (p *parser) tryCodeBlock(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") && !strings.HasPrefix(trimmed, "~~~") {
		return false
	}

	fence := trimmed[:3]
	lang := strings.TrimSpace(trimmed[3:])
	p.lineIdx++

	// Build code directly into a builder to avoid slice + Join allocation
	var code strings.Builder
	first := true
	for p.lineIdx < len(p.lines) {
		codeLine := p.lines[p.lineIdx]
		if strings.HasPrefix(strings.TrimSpace(codeLine), fence) {
			p.lineIdx++
			break
		}
		if !first {
			code.WriteByte('\n')
		}
		code.WriteString(codeLine)
		first = false
		p.lineIdx++
	}

	p.renderCodeBlock(code.String(), lang)
	return true
}

// headingLevel returns the ATX heading level (1-6) for line, or 0 if line is
// not a valid ATX heading. A valid heading has 1-6 '#' characters followed by
// a space, tab, or end of line.
func headingLevel(line string) int {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0
	}
	if level == len(line) {
		return level
	}
	if c := line[level]; c == ' ' || c == '\t' {
		return level
	}
	return 0
}

// tryHeading checks for ATX-style headings (# through ######)
func (p *parser) tryHeading(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	level := headingLevel(trimmed)
	if level == 0 {
		return false
	}

	content := strings.TrimSpace(trimmed[level:])
	// Remove trailing #s
	content = strings.TrimRight(content, "# \t")

	ansiStyle := p.styles.ansiHeadings[level-1]
	prefix := p.styles.headingPrefixes[level-1]
	styledPrefix := p.styles.styledHeadingPrefixes[level-1]
	styledContinuationIndent := p.styles.styledHeadingContIndent[level-1]

	// Headings are bold by default. If the entire heading is wrapped in emphasis,
	// strip the wrapper and only apply italics when requested.
	headingItalic := false
	switch {
	case strings.HasPrefix(content, "***") && strings.HasSuffix(content, "***") && len(content) > 6:
		content = content[3 : len(content)-3]
		headingItalic = true
	case strings.HasPrefix(content, "**") && strings.HasSuffix(content, "**") && len(content) > 4:
		content = content[2 : len(content)-2]
	case strings.HasPrefix(content, "__") && strings.HasSuffix(content, "__") && len(content) > 4:
		content = content[2 : len(content)-2]
	case strings.HasPrefix(content, "*") && strings.HasSuffix(content, "*") && len(content) > 2:
		content = content[1 : len(content)-1]
		headingItalic = true
	case strings.HasPrefix(content, "_") && strings.HasSuffix(content, "_") && len(content) > 2:
		content = content[1 : len(content)-1]
		headingItalic = true
	}
	if headingItalic {
		ansiStyle = ansiStyle.withItalic()
	}

	// Use heading-aware inline rendering so styled elements restore to heading style
	rendered := p.renderInlineWithStyle(content, ansiStyle)
	// Calculate available width for content (accounting for prefix)
	// Note: prefix is always ASCII (e.g., "## "), so len() == visual width
	prefixWidth := len(prefix)
	contentWidth := p.width - prefixWidth
	if contentWidth < 10 {
		contentWidth = p.width
	}

	// Wrap the rendered content and style each line
	wrapped := p.wrapText(rendered, contentWidth)
	first := true
	for l := range strings.SplitSeq(wrapped, "\n") {
		if first {
			p.out.WriteString(styledPrefix)
			first = false
		} else {
			p.out.WriteString(styledContinuationIndent)
		}
		p.out.WriteString(l)
		p.out.WriteByte('\n')
	}
	p.out.WriteByte('\n')
	p.lineIdx++
	return true
}

// tryHorizontalRule checks for horizontal rules (---, ***, ___)
func (p *parser) tryHorizontalRule(line string) bool {
	if !isHorizontalRule(line) {
		return false
	}
	p.out.WriteString(p.styles.styledHR)
	p.lineIdx++
	return true
}

// tryBlockquote checks for blockquotes (>)
func (p *parser) tryBlockquote(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, ">") {
		return false
	}

	var quoteLines []string
	for p.lineIdx < len(p.lines) {
		l := strings.TrimLeft(p.lines[p.lineIdx], " \t")
		if !strings.HasPrefix(l, ">") {
			break
		}
		// Remove the > and optional space
		content := strings.TrimPrefix(l, ">")
		content = strings.TrimPrefix(content, " ")
		quoteLines = append(quoteLines, content)
		p.lineIdx++
	}

	// Render blockquote content with indent
	indent := spaces(p.styles.blockquoteIndent)
	availableWidth := p.width - p.styles.blockquoteIndent
	p.renderBlockquoteContent(quoteLines, indent, availableWidth)
	p.out.WriteString("\n")
	return true
}

// renderBlockquoteContent renders the content of a blockquote, handling fenced code blocks and nested blockquotes
func (p *parser) renderBlockquoteContent(lines []string, indent string, availableWidth int) {
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Check for nested blockquote (line starts with >)
		if strings.HasPrefix(trimmed, ">") {
			// Collect all consecutive nested blockquote lines
			var nestedLines []string
			for i < len(lines) {
				l := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(l, ">") {
					break
				}
				// Strip the > and optional space
				content := strings.TrimPrefix(l, ">")
				content = strings.TrimPrefix(content, " ")
				nestedLines = append(nestedLines, content)
				i++
			}

			// Render the nested blockquote with additional indentation
			nestedIndent := indent + spaces(p.styles.blockquoteIndent)
			nestedWidth := max(availableWidth-p.styles.blockquoteIndent,
				// Minimum content width
				10)
			p.renderBlockquoteContent(nestedLines, nestedIndent, nestedWidth)
			continue
		}

		// Check for fenced code block start
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence := trimmed[:3]
			lang := strings.TrimSpace(trimmed[3:])
			i++

			// Collect code lines until fence end
			var codeLines []string
			for i < len(lines) {
				codeLine := lines[i]
				if strings.HasPrefix(strings.TrimSpace(codeLine), fence) {
					i++
					break
				}
				codeLines = append(codeLines, codeLine)
				i++
			}

			// Render the code block within blockquote context
			code := strings.Join(codeLines, "\n")
			p.renderBlockquoteCodeBlock(code, lang, indent, availableWidth)
			continue
		}

		// Regular line - render with blockquote-aware inline styling
		rendered := p.renderInlineWithStyle(line, p.styles.ansiBlockquote)
		wrapped := p.wrapText(rendered, availableWidth)
		for wl := range strings.SplitSeq(wrapped, "\n") {
			p.out.WriteString(indent)
			p.styles.ansiBlockquote.renderTo(&p.out, wl)
			p.out.WriteByte('\n')
		}
		i++
	}
}

// renderBlockquoteCodeBlock renders a fenced code block within a blockquote
func (p *parser) renderBlockquoteCodeBlock(code, lang, indent string, availableWidth int) {
	// Add spacing before code block
	p.out.WriteString("\n")

	if code == "" {
		return
	}

	p.renderCodeBlockWithIndent(code, lang, indent, availableWidth)
}

// tableCell holds pre-rendered cell data to avoid re-rendering
type tableCell struct {
	rendered    string // rendered with inline styles
	width       int    // visual width (excluding ANSI codes)
	longestWord int    // width of longest single word (for minimum column width)
}

// tableLayout holds the computed table layout parameters
type tableLayout struct {
	colWidths  []int  // width for each column
	sepWidth   int    // visual width of column separator (1 or 3)
	sep        string // separator string (" │ " or "│")
	dividerSep string // divider join string ("─┼─" or "┼")
	needsWrap  bool   // whether cells need wrapping
}

// computeTableLayout calculates viewport-fit column widths using proportional distribution.
// It returns the column widths and separator configuration that fit within viewportWidth.
func computeTableLayout(desired, headerWidths []int, viewportWidth int) tableLayout {
	numCols := len(desired)
	if numCols == 0 {
		return tableLayout{}
	}

	// Standard separator: " │ " (width 3), divider: "─┼─" (width 3)
	// Compact separator: "│" (width 1), divider: "┼" (width 1)
	const (
		wideSepWidth    = 3
		compactSepWidth = 1
	)

	// Calculate total natural width with wide separators
	totalDesired := 0
	for _, w := range desired {
		totalDesired += w
	}
	totalWithWideSep := totalDesired + (numCols-1)*wideSepWidth

	// Fast path: table fits with natural widths
	if totalWithWideSep <= viewportWidth {
		colWidths := make([]int, numCols)
		copy(colWidths, desired)
		return tableLayout{
			colWidths:  colWidths,
			sepWidth:   wideSepWidth,
			sep:        " │ ",
			dividerSep: "─┼─",
			needsWrap:  false,
		}
	}

	// Try with wide separators first
	sepWidth := wideSepWidth
	sep := " │ "
	dividerSep := "─┼─"
	availableForCells := viewportWidth - (numCols-1)*sepWidth

	// If wide separators don't leave enough room, try compact
	minColWidth := 1
	if availableForCells < numCols*minColWidth {
		sepWidth = compactSepWidth
		sep = "│"
		dividerSep = "┼"
		availableForCells = viewportWidth - (numCols-1)*sepWidth
	}

	// If even compact mode can't fit, use minimal widths
	if availableForCells < numCols*minColWidth {
		availableForCells = numCols * minColWidth
	}

	colWidths := distributeWidth(desired, headerWidths, availableForCells)

	return tableLayout{
		colWidths:  colWidths,
		sepWidth:   sepWidth,
		sep:        sep,
		dividerSep: dividerSep,
		needsWrap:  true,
	}
}

// distributeWidth distributes available width proportionally to desired widths.
// Columns that need less than their proportional share keep their desired width,
// and the excess is redistributed to columns that need more.
// minWidths specifies the minimum width for each column (e.g., longest word).
func distributeWidth(desired, minWidths []int, available int) []int {
	numCols := len(desired)
	if numCols == 0 {
		return nil
	}

	colWidths := make([]int, numCols)

	// Calculate total desired width
	totalDesired := 0
	for _, w := range desired {
		totalDesired += w
	}

	// If we have enough space for everything, use desired widths
	if totalDesired <= available {
		copy(colWidths, desired)
		return colWidths
	}

	// Start with minimum widths (longest word in column), capped at desired
	remaining := available
	for i, d := range desired {
		minW := 1
		if i < len(minWidths) && minWidths[i] > minW {
			minW = minWidths[i]
		}
		colWidths[i] = min(d, minW)
		remaining -= colWidths[i]
	}

	// Distribute remaining width proportionally to columns that still need more
	for remaining > 0 {
		// Find columns that can still grow
		totalNeed := 0
		for i, d := range desired {
			if colWidths[i] < d {
				totalNeed += d - colWidths[i]
			}
		}
		if totalNeed == 0 {
			break
		}

		// Distribute proportionally using largest remainder method
		distributed := 0
		remainders := make([]struct {
			idx       int
			remainder float64
		}, 0, numCols)

		for i, d := range desired {
			need := d - colWidths[i]
			if need <= 0 {
				continue
			}
			// Proportional share of remaining width
			share := float64(remaining) * float64(need) / float64(totalNeed)
			intPart := int(share)
			fracPart := share - float64(intPart)

			// Cap at what the column actually needs
			if intPart > need {
				intPart = need
			}
			colWidths[i] += intPart
			distributed += intPart

			// Track remainder for later distribution
			if colWidths[i] < d {
				remainders = append(remainders, struct {
					idx       int
					remainder float64
				}{i, fracPart})
			}
		}

		remaining -= distributed

		// Distribute leftover 1-by-1 to columns with largest remainders
		// Sort by remainder descending
		slices.SortFunc(remainders, func(a, b struct {
			idx       int
			remainder float64
		},
		) int {
			return cmp.Compare(b.remainder, a.remainder) // descending order
		})

		for _, r := range remainders {
			if remaining <= 0 {
				break
			}
			if colWidths[r.idx] < desired[r.idx] {
				colWidths[r.idx]++
				remaining--
			}
		}
	}

	// Ensure minimum of 1 for all columns
	for i := range colWidths {
		if colWidths[i] < 1 {
			colWidths[i] = 1
		}
	}

	return colWidths
}

// tryTable checks for markdown tables
func (p *parser) tryTable(line string) bool {
	// Tables start with | or have | in them
	if !strings.Contains(line, "|") {
		return false
	}

	// Count table lines first to avoid slice growth
	startIdx := p.lineIdx
	numLines := 0
	for i := p.lineIdx; i < len(p.lines); i++ {
		if !strings.Contains(p.lines[i], "|") {
			break
		}
		numLines++
	}

	if numLines < 2 {
		// Need at least header and separator
		return false
	}

	// Check if second line is a separator (contains only -, |, :, and spaces)
	separator := p.lines[p.lineIdx+1]
	for _, c := range separator {
		if c != '-' && c != '|' && c != ':' && c != ' ' && c != '\t' {
			// Not a valid table
			return false
		}
	}

	// Parse and render cells in one pass
	// Pre-allocate rows slice (numLines - 1 because we skip the separator)
	rows := make([][]tableCell, 0, numLines-1)
	numCols := 0

	for i := range numLines {
		if i == 1 {
			// Skip separator line
			continue
		}
		cells := p.parseAndRenderTableRow(p.lines[p.lineIdx+i])
		if len(cells) > numCols {
			numCols = len(cells)
		}
		rows = append(rows, cells)
	}

	if len(rows) == 0 || numCols == 0 {
		return false
	}

	// Advance line index past all table lines
	p.lineIdx = startIdx + numLines

	// Calculate desired column widths (natural width = max cell width per column)
	// and minimum widths (longest single word in each column to avoid mid-word breaks)
	desired := make([]int, numCols)
	minWidths := make([]int, numCols)
	for _, row := range rows {
		for i, cell := range row {
			if cell.width > desired[i] {
				desired[i] = cell.width
			}
			if cell.longestWord > minWidths[i] {
				minWidths[i] = cell.longestWord
			}
		}
	}

	// Compute viewport-fit layout
	layout := computeTableLayout(desired, minWidths, p.width)
	colWidths := layout.colWidths

	// Build separator line based on fitted widths
	sepLine := p.buildTableSeparatorLine(colWidths, layout.dividerSep)

	// Use ansiText style for table chrome (separators, dividers) to match content
	textStyle := p.styles.ansiText
	styledSepLine := textStyle.render(sepLine)
	styledSep := textStyle.render(layout.sep)

	if !layout.needsWrap {
		// Fast path: no wrapping needed, render single-line rows
		p.renderTableRowsFast(rows, colWidths, styledSep, styledSepLine)
	} else {
		// Slow path: wrap cells and render multi-line rows
		p.renderTableRowsWrapped(rows, colWidths, styledSep, styledSepLine)
	}

	p.out.WriteByte('\n')
	return true
}

// buildTableSeparatorLine builds the horizontal separator line for table header
func (p *parser) buildTableSeparatorLine(colWidths []int, dividerSep string) string {
	numCols := len(colWidths)
	var sepBuilder strings.Builder
	// Estimate size: each column width + divider separators
	sepBuilder.Grow(numCols * 10)
	for i, w := range colWidths {
		for range w {
			sepBuilder.WriteString("─")
		}
		if i < numCols-1 {
			sepBuilder.WriteString(dividerSep)
		}
	}
	return sepBuilder.String()
}

// buildTableBlankRow builds a blank row with column separators for visual separation
func buildTableBlankRow(colWidths []int, styledSep string) string {
	var b strings.Builder
	for i, w := range colWidths {
		b.WriteString(spaces(w))
		if i < len(colWidths)-1 {
			b.WriteString(styledSep)
		}
	}
	return b.String()
}

// renderTableRowsFast renders table rows without wrapping (fast path)
func (p *parser) renderTableRowsFast(rows [][]tableCell, colWidths []int, styledSep, styledSepLine string) {
	numCols := len(colWidths)
	blankRow := buildTableBlankRow(colWidths, styledSep)

	for rowIdx, row := range rows {
		// Add blank row between data rows for visual separation
		if rowIdx > 1 {
			p.out.WriteString(blankRow)
			p.out.WriteByte('\n')
		}

		for i := range numCols {
			var cell tableCell
			if i < len(row) {
				cell = row[i]
			}

			if rowIdx == 0 {
				// Header row - bold
				p.styles.ansiBold.renderTo(&p.out, cell.rendered)
			} else {
				p.out.WriteString(cell.rendered)
			}

			// Add padding
			padding := colWidths[i] - cell.width
			if padding > 0 {
				p.out.WriteString(spaces(padding))
			}

			if i < numCols-1 {
				p.out.WriteString(styledSep)
			}
		}
		p.out.WriteByte('\n')

		// Add separator after header
		if rowIdx == 0 {
			p.out.WriteString(styledSepLine)
			p.out.WriteByte('\n')
		}
	}
}

// renderTableRowsWrapped renders table rows with cell wrapping (slow path)
func (p *parser) renderTableRowsWrapped(rows [][]tableCell, colWidths []int, styledSep, styledSepLine string) {
	numCols := len(colWidths)
	blankRow := buildTableBlankRow(colWidths, styledSep)

	for rowIdx, row := range rows {
		// Add blank row between data rows for visual separation
		if rowIdx > 1 {
			p.out.WriteString(blankRow)
			p.out.WriteByte('\n')
		}

		// Wrap each cell and collect wrapped lines
		wrappedCells := make([][]string, numCols)
		maxLines := 1

		for i := range numCols {
			var cell tableCell
			if i < len(row) {
				cell = row[i]
			}

			if cell.width <= colWidths[i] {
				// Cell fits, no wrapping needed
				wrappedCells[i] = []string{cell.rendered}
			} else {
				// Wrap the cell content
				wrapped := p.wrapText(cell.rendered, colWidths[i])
				lines := strings.Split(wrapped, "\n")
				wrappedCells[i] = lines
			}

			if len(wrappedCells[i]) > maxLines {
				maxLines = len(wrappedCells[i])
			}
		}

		// Render each physical line of the row
		for lineIdx := range maxLines {
			for colIdx := range numCols {
				var lineContent string
				if lineIdx < len(wrappedCells[colIdx]) {
					lineContent = wrappedCells[colIdx][lineIdx]
				}

				// Apply bold to header row
				if rowIdx == 0 {
					p.styles.ansiBold.renderTo(&p.out, lineContent)
				} else {
					p.out.WriteString(lineContent)
				}

				// Pad to column width
				lineWidth := ansiStringWidth(lineContent)
				padding := colWidths[colIdx] - lineWidth
				if padding > 0 {
					p.out.WriteString(spaces(padding))
				}

				if colIdx < numCols-1 {
					p.out.WriteString(styledSep)
				}
			}
			p.out.WriteByte('\n')
		}

		// Add separator after header
		if rowIdx == 0 {
			p.out.WriteString(styledSepLine)
			p.out.WriteByte('\n')
		}
	}
}

// parseAndRenderTableRow parses a table row and renders cells in one pass
func (p *parser) parseAndRenderTableRow(line string) []tableCell {
	// Trim leading/trailing whitespace and pipes
	line = strings.TrimSpace(line)
	if line != "" && line[0] == '|' {
		line = line[1:]
	}
	if line != "" && line[len(line)-1] == '|' {
		line = line[:len(line)-1]
	}

	// Count cells first to pre-allocate
	numCells := 1
	for i := range len(line) {
		if line[i] == '|' {
			numCells++
		}
	}

	cells := make([]tableCell, 0, numCells)
	start := 0

	for i := 0; i <= len(line); i++ {
		if i < len(line) && line[i] != '|' {
			continue
		}
		// Extract and trim the cell
		cellText := strings.TrimSpace(line[start:i])
		var rendered string
		var width int
		// Fast path: if cell has no markdown, skip full inline rendering
		if !hasInlineMarkdown(cellText) {
			// Apply base text style directly
			rendered = p.styles.ansiText.render(cellText)
			width = textWidth(cellText)
		} else {
			// Use renderInlineWithWidth for markdown content
			rendered, width = p.renderInlineWithWidth(cellText)
		}
		cells = append(cells, tableCell{
			rendered:    rendered,
			width:       width,
			longestWord: longestWordWidth(cellText),
		})
		start = i + 1
	}

	return cells
}

type listItem struct {
	content string
	ordered bool
	task    bool
	checked bool
}

// parseListItem attempts to parse a list item from a trimmed line.
// Returns the parsed item and true if successful, or zero value and false otherwise.
func parseListItem(line string) (listItem, bool) {
	if len(line) < 2 {
		return listItem{}, false
	}

	// Check unordered list (-, *, +)
	if (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		content := line[2:]
		item := listItem{content: content}
		if strings.HasPrefix(content, "[ ] ") {
			item.task = true
			item.content = content[4:]
		} else if strings.HasPrefix(content, "[x] ") || strings.HasPrefix(content, "[X] ") {
			item.task = true
			item.checked = true
			item.content = content[4:]
		}
		return item, true
	}

	// Check ordered list (1., 2., etc.)
	dotIdx := strings.Index(line, ".")
	if dotIdx > 0 && dotIdx < 10 && len(line) > dotIdx+1 && line[dotIdx+1] == ' ' {
		for i := range dotIdx {
			if !unicode.IsDigit(rune(line[i])) {
				return listItem{}, false
			}
		}
		return listItem{content: line[dotIdx+2:], ordered: true}, true
	}

	return listItem{}, false
}

// isListStart checks if a line starts a list item
func isListStart(line string) bool {
	_, ok := parseListItem(line)
	return ok
}

// tryList checks for unordered lists (-, *, +) or ordered lists (1., 2., etc.)
func (p *parser) tryList(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if _, ok := parseListItem(trimmed); !ok {
		return false
	}

	// Track the current list item's bullet width for continuation content (code blocks)
	var currentBulletWidth int

	for p.lineIdx < len(p.lines) {
		l := p.lines[p.lineIdx]
		ltrimmed := strings.TrimLeft(l, " \t")
		lindent := len(l) - len(ltrimmed)

		item, isListItem := parseListItem(ltrimmed)

		// Check for fenced code block within list context
		if !isListItem && (strings.HasPrefix(ltrimmed, "```") || strings.HasPrefix(ltrimmed, "~~~")) {
			// This is a code block - check if it's indented (part of list)
			if lindent > 0 || currentBulletWidth > 0 {
				// Render the code block with list indentation
				p.renderListCodeBlock(lindent, currentBulletWidth)
				continue
			}
			// Not indented, break out of list
			break
		}

		// Check for blockquote within list context
		if !isListItem && strings.HasPrefix(ltrimmed, ">") {
			// This is a blockquote - check if it's indented (part of list)
			if lindent > 0 || currentBulletWidth > 0 {
				// Render the blockquote with list indentation
				p.renderListBlockquote(currentBulletWidth)
				continue
			}
			// Not indented, break out of list
			break
		}

		// Empty line handling
		if !isListItem && strings.TrimSpace(l) == "" {
			if p.lineIdx+1 < len(p.lines) {
				nextLine := p.lines[p.lineIdx+1]
				nextTrimmed := strings.TrimLeft(nextLine, " \t")
				nextIndent := len(nextLine) - len(nextTrimmed)
				// Continue if next line is a list item OR indented content (like code block)
				if !isListStart(nextTrimmed) && nextIndent == 0 {
					break
				}
			} else {
				break
			}
			p.lineIdx++
			continue
		}

		if !isListItem {
			break
		}

		level := lindent / p.styles.listIndent
		bulletIndent := spaces(level * p.styles.listIndent)

		var bullet string
		switch {
		case item.task && item.checked:
			bullet = p.styles.styleTaskTicked
		case item.task:
			bullet = p.styles.styleTaskUntick
		default:
			// Use consistent bullet for both ordered and unordered lists
			bullet = "- "
		}

		// Calculate the width available for content (after bullet and indentation)
		// bulletIndent is always ASCII spaces, bullet may contain unicode for task items
		bulletWidth := len(bulletIndent) + textWidth(bullet)
		contentWidth := max(p.width-bulletWidth, 10) // Minimum content width of 10

		// Store current list item's bullet width for code blocks
		currentBulletWidth = bulletWidth

		rendered := p.renderInline(item.content)
		wrapped := p.wrapText(rendered, contentWidth)

		// Pre-compute continuation indent
		continuationIndent := spaces(bulletWidth)
		first := true
		for l := range strings.SplitSeq(wrapped, "\n") {
			if first {
				// Write first line with bullet
				p.out.WriteString(bulletIndent)
				p.out.WriteString(bullet)
				p.out.WriteString(l)
				p.out.WriteByte('\n')
				first = false
			} else {
				// Write continuation lines with proper indentation
				p.out.WriteString(continuationIndent)
				p.out.WriteString(l)
				p.out.WriteByte('\n')
			}
		}

		p.lineIdx++
	}

	p.out.WriteString("\n")
	return true
}

// tryFootnoteDefinition checks for footnote definitions [^id]: content
func (p *parser) tryFootnoteDefinition(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "[^") {
		return false
	}

	// Find the closing ]
	closeBracket := strings.Index(trimmed, "]")
	if closeBracket == -1 || closeBracket < 3 {
		return false
	}

	// Must be followed by :
	if len(trimmed) <= closeBracket+1 || trimmed[closeBracket+1] != ':' {
		return false
	}

	// Extract footnote ID and content
	footnoteID := trimmed[1:closeBracket] // includes the ^
	content := strings.TrimSpace(trimmed[closeBracket+2:])

	p.lineIdx++

	// Collect continuation lines (indented)
	var contentLines []string
	if content != "" {
		contentLines = append(contentLines, content)
	}
	for p.lineIdx < len(p.lines) {
		nextLine := p.lines[p.lineIdx]
		// Check if it's a continuation (indented or empty)
		if strings.TrimSpace(nextLine) == "" {
			// Empty line might continue the footnote
			if p.lineIdx+1 < len(p.lines) {
				followingLine := p.lines[p.lineIdx+1]
				if followingLine != "" && (followingLine[0] == ' ' || followingLine[0] == '\t') {
					contentLines = append(contentLines, "")
					p.lineIdx++
					continue
				}
			}
			break
		}
		if nextLine[0] != ' ' && nextLine[0] != '\t' {
			break
		}
		contentLines = append(contentLines, strings.TrimSpace(nextLine))
		p.lineIdx++
	}

	// Render the footnote definition
	// Format: [^id]: styled as footnote marker, then content
	fullContent := strings.Join(contentLines, " ")
	renderedContent := p.renderInline(fullContent)
	wrapped := p.wrapText(renderedContent, p.width-len(footnoteID)-3) // account for "[^id]: "

	marker := p.styles.ansiFootnote.render("[" + footnoteID + "]:")
	indent := spaces(len(footnoteID) + 3)
	first := true
	for l := range strings.SplitSeq(wrapped, "\n") {
		if first {
			p.out.WriteString(marker)
			p.out.WriteByte(' ')
			p.out.WriteString(l)
			p.out.WriteByte('\n')
			first = false
		} else {
			p.out.WriteString(indent)
			p.out.WriteString(l)
			p.out.WriteByte('\n')
		}
	}
	p.out.WriteByte('\n')
	return true
}

// renderListCodeBlock renders a fenced code block within a list context
func (p *parser) renderListCodeBlock(codeIndent, bulletWidth int) {
	// Add spacing before code block
	p.out.WriteString("\n")

	line := p.lines[p.lineIdx]
	ltrimmed := strings.TrimLeft(line, " \t")

	fence := ltrimmed[:3]
	lang := strings.TrimSpace(ltrimmed[3:])
	p.lineIdx++

	// Collect code lines
	var codeLines []string
	for p.lineIdx < len(p.lines) {
		codeLine := p.lines[p.lineIdx]
		codeTrimmed := strings.TrimLeft(codeLine, " \t")
		if strings.HasPrefix(codeTrimmed, fence) {
			p.lineIdx++
			break
		}
		// Remove the list indentation from code lines
		if len(codeLine) >= codeIndent {
			codeLines = append(codeLines, codeLine[codeIndent:])
		} else {
			codeLines = append(codeLines, strings.TrimLeft(codeLine, " \t"))
		}
		p.lineIdx++
	}

	code := strings.Join(codeLines, "\n")
	if code == "" {
		return
	}

	indent := spaces(bulletWidth)
	availableWidth := p.width - bulletWidth
	p.renderCodeBlockWithIndent(code, lang, indent, availableWidth)
}

// renderListBlockquote renders a blockquote within a list context
func (p *parser) renderListBlockquote(bulletWidth int) {
	// Collect all blockquote lines
	var quoteLines []string
	for p.lineIdx < len(p.lines) {
		line := p.lines[p.lineIdx]
		ltrimmed := strings.TrimLeft(line, " \t")

		// Check if this line is part of the blockquote
		if !strings.HasPrefix(ltrimmed, ">") {
			break
		}

		// Remove the > and optional space
		content := strings.TrimPrefix(ltrimmed, ">")
		content = strings.TrimPrefix(content, " ")
		quoteLines = append(quoteLines, content)
		p.lineIdx++
	}

	if len(quoteLines) == 0 {
		return
	}

	// Calculate the indentation for the blockquote (align with list content)
	indent := spaces(bulletWidth)

	// Calculate available width for blockquote content
	availableWidth := max(p.width-bulletWidth-p.styles.blockquoteIndent, 10)

	// Use renderBlockquoteContent for full support including nested code blocks
	fullIndent := indent + spaces(p.styles.blockquoteIndent)
	p.renderBlockquoteContent(quoteLines, fullIndent, availableWidth)
}

// renderParagraph collects consecutive non-empty lines and renders them as a paragraph.
// The first line is always consumed: parse() has already verified it isn't a block,
// so without that guarantee an approximate isBlockStart check could loop forever on
// inputs like "####### foo" or "#nospace".
func (p *parser) renderParagraph() {
	var paraLines []string
	for p.lineIdx < len(p.lines) {
		line := p.lines[p.lineIdx]
		if strings.TrimSpace(line) == "" {
			p.lineIdx++
			break
		}
		if len(paraLines) > 0 && isBlockStart(line) {
			break
		}
		paraLines = append(paraLines, line)
		p.lineIdx++
	}

	if len(paraLines) == 0 {
		return
	}

	text := strings.Join(paraLines, " ")
	rendered := p.renderInline(text)
	wrapped := p.wrapText(rendered, p.width)
	p.out.WriteString(wrapped + "\n\n")
}

// isBlockStart reports whether line could begin a new block element when
// encountered inside a paragraph. It mirrors the prefix tests in parse() so a
// paragraph terminates correctly at the start of the next block.
func isBlockStart(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	switch {
	case headingLevel(trimmed) > 0:
		return true
	case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
		return true
	case strings.HasPrefix(trimmed, ">"):
		return true
	case isListStart(trimmed):
		return true
	case isHorizontalRule(trimmed):
		return true
	}
	return false
}

func isHorizontalRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return false
	}
	char := trimmed[0]
	if char != '-' && char != '*' && char != '_' {
		return false
	}
	count := 0
	for _, c := range trimmed {
		if c == rune(char) {
			count++
		} else if !unicode.IsSpace(c) {
			return false
		}
	}
	return count >= 3
}

// writeHyperlinkStart writes the OSC 8 opening sequence for a clickable hyperlink.
func writeHyperlinkStart(b *strings.Builder, url string) {
	b.WriteString(xansi.SetHyperlink(url))
}

// writeHyperlinkEnd writes the OSC 8 closing sequence to end a hyperlink.
func writeHyperlinkEnd(b *strings.Builder) {
	b.WriteString(xansi.ResetHyperlink())
}

// findURLEnd returns the length of a URL starting at the given position.
// It stops at whitespace, or certain trailing punctuation that is unlikely
// part of the URL (e.g., trailing period, comma, parenthesis if unmatched).
func findURLEnd(s string) int {
	i := 0
	parenDepth := 0
	for i < len(s) {
		c := s[i]
		if c <= ' ' {
			break
		}
		if c == '(' {
			parenDepth++
		} else if c == ')' {
			if parenDepth > 0 {
				parenDepth--
			} else {
				break
			}
		}
		i++
	}
	for i > 0 {
		c := s[i-1]
		if c == '.' || c == ',' || c == ';' || c == ':' || c == '!' || c == '?' {
			i--
		} else {
			break
		}
	}
	return i
}

// renderInline processes inline markdown elements: bold, italic, code, links, etc.
// It uses the document's base text style for restoring after styled elements.
func (p *parser) renderInline(text string) string {
	return p.renderInlineWithStyle(text, p.styles.ansiText)
}

// renderInlineWithWidth renders inline markdown and returns both the rendered string and visual width.
// This avoids a separate width calculation pass for cases like table cells.
func (p *parser) renderInlineWithWidth(text string) (string, int) {
	var out strings.Builder
	out.Grow(len(text) + 64)
	width := p.renderInlineWithStyleTo(&out, text, p.styles.ansiText)
	return out.String(), width
}

// renderInlineWithStyle processes inline markdown with a custom restore style.
// The restoreStyle is applied to plain text after styled elements (code, bold, etc.)
// to maintain proper styling context (e.g., within headings or blockquotes).
func (p *parser) renderInlineWithStyle(text string, restoreStyle ansiStyle) string {
	if text == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(len(text) + 64)
	p.renderInlineWithStyleTo(&out, text, restoreStyle)
	return out.String()
}

// renderInlineWithStyleTo writes inline markdown to the provided builder and returns the visual width.
// This is the core implementation that avoids intermediate string allocations in recursive calls.
func (p *parser) renderInlineWithStyleTo(out *strings.Builder, text string, restoreStyle ansiStyle) int {
	if text == "" {
		return 0
	}

	// Fast path: check if text contains any markdown characters or URLs
	// If not, apply the restore style directly and return
	firstMarker := strings.IndexAny(text, inlineMarkdownChars)
	firstURL := findFirstURL(text)
	if firstMarker == -1 && firstURL == -1 {
		restoreStyle.renderTo(out, text)
		return textWidth(text)
	}

	// Determine the first trigger position (marker or URL)
	firstTrigger := firstMarker
	if firstTrigger == -1 || (firstURL != -1 && firstURL < firstTrigger) {
		firstTrigger = firstURL
	}

	width := 0

	// Optimization: write any leading plain text in one batch
	if firstTrigger > 0 {
		plain := text[:firstTrigger]
		restoreStyle.renderTo(out, plain)
		width += textWidth(plain)
		text = text[firstTrigger:]
	}

	i := 0
	n := len(text)

	for i < n {
		// Check for escaped characters
		if text[i] == '\\' && i+1 < n {
			out.WriteByte(text[i+1])
			width += runewidth.RuneWidth(rune(text[i+1]))
			i += 2
			continue
		}

		// Check for inline code
		if text[i] == '`' {
			end := strings.Index(text[i+1:], "`")
			if end != -1 {
				code := text[i+1 : i+1+end]
				// Use flags to check if parent has formatting attributes that should carry to code
				if restoreStyle.hasStrike || restoreStyle.hasBold {
					// Write code style prefix, then inherited formatting, then code, then suffix
					out.WriteString(p.styles.ansiCode.prefix)
					if restoreStyle.hasBold {
						out.WriteString("\x1b[1m")
					}
					if restoreStyle.hasStrike {
						out.WriteString("\x1b[9m")
					}
					out.WriteString(code)
					out.WriteString(p.styles.ansiCode.suffix)
				} else {
					p.styles.ansiCode.renderTo(out, code)
				}
				// Restore parent style after code (since ansiCode.suffix resets everything)
				out.WriteString(restoreStyle.prefix)
				width += textWidth(code)
				i = i + 1 + end + 1
				continue
			}
		}

		// Check for bold (**text** or __text__)
		if i+1 < n && ((text[i] == '*' && text[i+1] == '*') || (text[i] == '_' && text[i+1] == '_')) {
			delim := text[i : i+2]
			end := strings.Index(text[i+2:], delim)
			if end != -1 {
				inner := text[i+2 : i+2+end]
				// Check for bold+italic (***text***)
				if strings.HasPrefix(inner, "*") && strings.HasSuffix(inner, "*") && len(inner) >= 2 {
					innerText := inner[1 : len(inner)-1]
					if restoreStyle.hasBold {
						// Heading (or already-bold) context: bold is redundant, keep italic
						combinedStyle := restoreStyle.withItalic()
						width += p.renderInlineWithStyleTo(out, innerText, combinedStyle)
					} else {
						// Add bold+italic formatting while preserving parent color (e.g., heading)
						combinedStyle := restoreStyle.withBoldItalic()
						width += p.renderInlineWithStyleTo(out, innerText, combinedStyle)
					}
				} else {
					if restoreStyle.hasBold {
						// Bold is redundant in bold contexts (e.g., headings)
						width += p.renderInlineWithStyleTo(out, inner, restoreStyle)
					} else {
						// Add bold formatting while preserving parent color (e.g., heading)
						combinedStyle := restoreStyle.withBold()
						width += p.renderInlineWithStyleTo(out, inner, combinedStyle)
					}
				}
				i = i + 2 + end + 2
				continue
			}
		}

		// Check for italic (*text* or _text_) - but not in the middle of words for _
		if text[i] == '*' || (text[i] == '_' && (i == 0 || !isWord(text[i-1]))) {
			delim := text[i]
			end := -1
			for j := i + 1; j < n; j++ {
				if text[j] == delim {
					// For underscore, check it's not in the middle of a word
					if delim == '_' && j+1 < n && isWord(text[j+1]) {
						continue
					}
					end = j
					break
				}
			}
			if end != -1 && end > i+1 {
				inner := text[i+1 : end]
				// Add italic formatting while preserving parent color (e.g., heading)
				combinedStyle := restoreStyle.withItalic()
				width += p.renderInlineWithStyleTo(out, inner, combinedStyle)
				i = end + 1
				continue
			}
		}

		// Check for strikethrough (~~text~~)
		if i+1 < n && text[i] == '~' && text[i+1] == '~' {
			end := strings.Index(text[i+2:], "~~")
			if end != -1 {
				inner := text[i+2 : i+2+end]
				// Add strikethrough formatting while preserving parent color (e.g., heading)
				combinedStyle := restoreStyle.withStrikethrough()
				width += p.renderInlineWithStyleTo(out, inner, combinedStyle)
				i = i + 2 + end + 2
				continue
			}
		}

		// Check for footnote references [^1] or [^name]
		if text[i] == '[' && i+2 < n && text[i+1] == '^' {
			// Find closing bracket
			closeBracket := strings.Index(text[i:], "]")
			if closeBracket != -1 {
				footnoteRef := text[i : i+closeBracket+1]
				// Validate it looks like a footnote (not empty after ^)
				if closeBracket > 2 {
					p.styles.ansiFootnote.renderTo(out, footnoteRef)
					width += textWidth(footnoteRef)
					i = i + closeBracket + 1
					continue
				}
			}
		}

		// Check for links [text](url)
		switch c := text[i]; c {
		case '[':
			closeBracket := findClosingBracket(text[i:])
			if closeBracket != -1 && i+closeBracket+1 < n && text[i+closeBracket+1] == '(' {
				linkText := text[i+1 : i+closeBracket]
				rest := text[i+closeBracket+2:]
				closeParen := strings.Index(rest, ")")
				if closeParen != -1 {
					url := rest[:closeParen]
					if linkText != url {
						// Emit OSC 8 hyperlink wrapping styled link text
						writeHyperlinkStart(out, url)
						p.styles.ansiLinkText.renderTo(out, linkText)
						writeHyperlinkEnd(out)
						width += textWidth(linkText)
					} else {
						// URL is the same as the text — emit clickable link with URL as text
						writeHyperlinkStart(out, url)
						p.styles.ansiLink.renderTo(out, linkText)
						writeHyperlinkEnd(out)
						width += textWidth(linkText)
					}
					i = i + closeBracket + 2 + closeParen + 1
					continue
				}
			}
			fallthrough
		default:
			// Regular character - collect consecutive plain text
			start := i
			origStart := i // Track original start to detect no-progress
			for i < n && !isInlineMarker(text[i]) {
				// Auto-link URL detection. URLs we recognize all start with 'h'
				// ("http://" or "https://"), so a single-byte gate keeps this hot
				// loop tight on prose: we only pay for slice creation + memequal
				// on the rare 'h' bytes.
				if text[i] == 'h' && ((i+8 <= n && text[i:i+8] == "https://") || (i+7 <= n && text[i:i+7] == "http://")) {
					// First, emit any plain text before the URL
					if i > start {
						plainText := text[start:i]
						restoreStyle.renderTo(out, plainText)
						width += textWidth(plainText)
					}
					// Find URL boundaries, but don't extend past inline markdown markers.
					// Use urlStopMarkdownChars (excludes _ and \ which are valid in URLs)
					// to avoid splitting URLs like https://example.com/Thing_(foo).
					remaining := text[i:]
					if nextMarker := strings.IndexAny(remaining, urlStopMarkdownChars); nextMarker >= 0 {
						remaining = remaining[:nextMarker]
					}
					urlLen := findURLEnd(remaining)
					autoURL := text[i : i+urlLen]
					// Emit OSC 8 hyperlink
					writeHyperlinkStart(out, autoURL)
					p.styles.ansiLink.renderTo(out, autoURL)
					writeHyperlinkEnd(out)
					width += textWidth(autoURL)
					i += urlLen
					start = i
					continue
				}
				i++
			}
			// If we didn't advance from the original position (unmatched marker), consume one char as literal
			if i == origStart {
				i++
			}
			// Emit remaining plain text
			if i > start && start < n {
				plainText := text[start:i]
				restoreStyle.renderTo(out, plainText)
				width += textWidth(plainText)
			}
		}
	}

	return width
}

// longestWordWidth returns the visual width of the longest word in text.
// Words are separated by whitespace. Used to determine minimum column width.
func longestWordWidth(s string) int {
	maxWidth := 0
	wordStart := -1

	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if wordStart >= 0 {
				w := textWidth(s[wordStart:i])
				if w > maxWidth {
					maxWidth = w
				}
				wordStart = -1
			}
		} else if wordStart < 0 {
			wordStart = i
		}
	}
	// Handle last word
	if wordStart >= 0 {
		w := textWidth(s[wordStart:])
		if w > maxWidth {
			maxWidth = w
		}
	}
	return maxWidth
}

// textWidth calculates the visual width of plain text (no ANSI codes).
// Optimized for ASCII-only strings which are common.
func textWidth(s string) int {
	// Fast path for ASCII-only strings
	isASCII := true
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			isASCII = false
			break
		}
	}
	if isASCII {
		return len(s)
	}
	// Slow path for unicode
	width := 0
	for _, r := range s {
		width += runewidth.RuneWidth(r)
	}
	return width
}

func findClosingBracket(text string) int {
	depth := 0
	for i, c := range text {
		switch c {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isWord(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// inlineMarkdownChars contains all characters that trigger inline markdown processing.
const inlineMarkdownChars = "\\`*_~["

// urlStopMarkdownChars is the subset of inline markdown markers that should
// terminate auto-linked URL detection. Excludes _ and \\ because they appear
// frequently in valid URLs (e.g. https://example.com/Thing_(foo)).
const urlStopMarkdownChars = "`*~["

// findFirstURL returns the index of the first "https://" or "http://" in s, or -1.
//
// Fast path: bail out with a single IndexByte scan when 'h' isn't present at
// all. Both URL prefixes start with 'h', so the absence of 'h' guarantees no
// URL — and IndexByte is much cheaper than a substring search on prose that
// dominates real input (status lines, prompts, code without URLs, etc.).
func findFirstURL(s string) int {
	if strings.IndexByte(s, 'h') < 0 {
		return -1
	}
	if idx := strings.Index(s, "https://"); idx != -1 {
		if httpIdx := strings.Index(s, "http://"); httpIdx != -1 && httpIdx < idx {
			return httpIdx
		}
		return idx
	}
	if idx := strings.Index(s, "http://"); idx != -1 {
		return idx
	}
	return -1
}

// hasInlineMarkdown checks if text contains any markdown formatting characters.
// This allows a fast path to skip processing plain text.
// Uses strings.ContainsAny which is highly optimized in the Go standard library.
func hasInlineMarkdown(text string) bool {
	return strings.ContainsAny(text, inlineMarkdownChars)
}

func isInlineMarker(b byte) bool {
	switch b {
	case '\\', '`', '*', '_', '~', '[':
		return true
	}
	return false
}

// renderCodeBlock renders a fenced code block with syntax highlighting.
func (p *parser) renderCodeBlock(code, lang string) {
	if code == "" {
		p.out.WriteString("\n")
		return
	}
	p.renderCodeBlockWithIndent(code, lang, "", p.width)
}

// renderCodeBlockWithIndent renders a fenced code block with indentation and width constraints.
func (p *parser) renderCodeBlockWithIndent(code, lang, indent string, availableWidth int) {
	// Get syntax highlighting tokens
	tokens := p.syntaxHighlight(code, lang)

	// Calculate content width with adaptive padding
	// Only apply padding if we have enough width to make it worthwhile
	paddingLeft := 2
	paddingRight := 2
	const minWidthForPadding = 24

	if availableWidth < minWidthForPadding {
		// Disable padding for narrow widths to avoid exceeding available width
		paddingLeft = 0
		paddingRight = 0
	}

	contentWidth := max(availableWidth-paddingLeft-paddingRight,
		// Minimum content width
		1)

	// Pre-compute padding strings (avoids repeated strings.Repeat calls)
	paddingLeftStr := spaces(paddingLeft)
	fullWidthPad := spaces(availableWidth)

	// Use cached background style
	bgStyle := p.styles.ansiCodeBg

	// Render empty line at the top with a copy affordance pushed to the right
	// edge. Record the rendered line index so click handlers can map a click
	// back to this block's raw content.
	topLine := strings.Count(p.out.String(), "\n")
	p.out.WriteString(indent)
	iconWidth := runewidth.StringWidth(CodeBlockCopyIcon)
	leftFill := max(availableWidth-paddingRight-iconWidth, 0)
	if availableWidth >= iconWidth+paddingRight {
		bgStyle.renderTo(&p.out, spaces(leftFill))
		p.styles.ansiCodeBlockCopyIcon.renderTo(&p.out, CodeBlockCopyIcon)
		if paddingRight > 0 {
			bgStyle.renderTo(&p.out, spaces(paddingRight))
		}
	} else {
		// Too narrow for the icon; fall back to a plain top padding row.
		bgStyle.renderTo(&p.out, fullWidthPad)
	}
	p.out.WriteByte('\n')
	p.codeBlocks = append(p.codeBlocks, CodeBlock{Content: code, Line: topLine})

	// Process tokens line by line for better performance
	var lineBuilder strings.Builder
	lineBuilder.Grow(contentWidth + 32)
	lineWidth := 0

	flushLine := func() {
		// Add left padding with background
		p.out.WriteString(indent)
		bgStyle.renderTo(&p.out, paddingLeftStr)
		// Write line content
		p.out.WriteString(lineBuilder.String())
		// Pad to full width (including right padding)
		padWidth := contentWidth - lineWidth + paddingRight
		if padWidth > 0 {
			bgStyle.renderTo(&p.out, spaces(padWidth))
		}
		p.out.WriteByte('\n')
		lineBuilder.Reset()
		lineWidth = 0
	}

	// writeSegmentWrapped prefers breaking at whitespace for code readability.
	// Falls back to character-level breaking only when no whitespace exists.
	writeSegmentWrapped := func(segment string, style ansiStyle) {
		for segment != "" {
			remaining := contentWidth - lineWidth
			if remaining <= 0 {
				flushLine()
				remaining = contentWidth
			}

			// Single pass: track width and last whitespace within remaining
			lastSpacePos := -1
			lastSpaceBytePos := -1
			lastSpaceWidth := 0
			pos := 0
			width := 0
			exceeded := false

			for pos < len(segment) {
				r, size := utf8.DecodeRuneInString(segment[pos:])
				rw := runewidth.RuneWidth(r)

				if width+rw > remaining {
					exceeded = true
					break
				}

				if r == ' ' || r == '\t' {
					lastSpacePos = pos
					lastSpaceBytePos = pos + size
					lastSpaceWidth = width + rw
				}

				width += rw
				pos += size
			}

			if !exceeded {
				style.renderTo(&lineBuilder, segment)
				lineWidth += width
				return
			}

			switch {
			case lastSpacePos >= 0:
				// Found whitespace - break there (preferred)
				part := segment[:lastSpacePos+1] // include the space
				style.renderTo(&lineBuilder, part)
				lineWidth += lastSpaceWidth
				segment = segment[lastSpaceBytePos:]
				flushLine()
			case lineWidth > 0:
				// No whitespace found and we're mid-line - flush and retry with full width
				flushLine()
				// Don't consume segment, let next iteration try with full line width
			case pos > 0:
				// No whitespace, at line start, but we measured some chars that fit
				// Break at character boundary as last resort
				style.renderTo(&lineBuilder, segment[:pos])
				lineWidth += width
				segment = segment[pos:]
				flushLine()
			default:
				// Nothing fits (remaining width too small) - write one char and continue
				r, size := utf8.DecodeRuneInString(segment)
				style.renderTo(&lineBuilder, string(r))
				lineWidth += runewidth.RuneWidth(r)
				segment = segment[size:]
				flushLine()
			}
		}
	}

	for _, tok := range tokens {
		text := tok.text

		// Process text, splitting by newlines and handling tabs
		start := 0
		for i := range len(text) {
			if text[i] == '\n' {
				// Render text before newline
				if i > start {
					segment := text[start:i]
					segment = expandTabs(segment, lineWidth)
					writeCodeSegmentsWithAutoLinks(segment, tok.style, &lineBuilder, writeSegmentWrapped)
				}
				flushLine()
				start = i + 1
			}
		}
		// Render remaining text
		if start < len(text) {
			segment := text[start:]
			segment = expandTabs(segment, lineWidth)
			writeCodeSegmentsWithAutoLinks(segment, tok.style, &lineBuilder, writeSegmentWrapped)
		}
	}

	// Flush remaining content
	if lineBuilder.Len() > 0 {
		flushLine()
	}

	// Render empty line at the bottom (use pre-computed padding)
	p.out.WriteString(indent)
	bgStyle.renderTo(&p.out, fullWidthPad)
	p.out.WriteByte('\n')

	p.out.WriteByte('\n')
}

// writeCodeSegmentsWithAutoLinks detects URLs in a code segment and wraps them
// in OSC 8 hyperlink sequences so they become clickable in the TUI.
// OSC 8 open/close are written directly to lineBuilder (not measured by writeSegment),
// and finalizeOutput in Render() ensures sequences survive line wrapping.
func writeCodeSegmentsWithAutoLinks(segment string, style ansiStyle, lineBuilder *strings.Builder, writeSegment func(string, ansiStyle)) {
	for segment != "" {
		idx := findFirstURL(segment)
		if idx < 0 {
			writeSegment(segment, style)
			return
		}
		if idx > 0 {
			writeSegment(segment[:idx], style)
		}
		urlLen := findURLEnd(segment[idx:])
		url := segment[idx : idx+urlLen]
		lineBuilder.WriteString(xansi.SetHyperlink(url))
		writeSegment(url, style)
		lineBuilder.WriteString(xansi.ResetHyperlink())
		segment = segment[idx+urlLen:]
	}
}

// spacesBuffer is a pre-allocated buffer of spaces for padding needs.
// Slicing this is much faster than strings.Repeat for small amounts.
const spacesBuffer = "                                                                                                                                "

// spaces returns a string of n spaces, using the pre-allocated buffer when possible.
func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	if n <= len(spacesBuffer) {
		return spacesBuffer[:n]
	}
	return strings.Repeat(" ", n)
}

// writeSpaces writes n spaces to the builder without allocations.
func writeSpaces(b *strings.Builder, n int) {
	if n <= 0 {
		return
	}
	if n <= len(spacesBuffer) {
		b.WriteString(spacesBuffer[:n])
		return
	}
	for n > 0 {
		chunk := min(n, len(spacesBuffer))
		b.WriteString(spacesBuffer[:chunk])
		n -= chunk
	}
}

// expandTabs replaces tabs with spaces based on current position
func expandTabs(s string, currentWidth int) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var result strings.Builder
	width := currentWidth
	for _, r := range s {
		if r == '\t' {
			n := 4 - (width % 4)
			result.WriteString(spaces(n))
			width += n
		} else {
			result.WriteRune(r)
			width += runewidth.RuneWidth(r)
		}
	}
	return result.String()
}

// ansiStringWidth calculates display width while skipping ANSI escape sequences.
func ansiStringWidth(s string) int {
	width := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			end, _ := scanAnsiSequence(s, i)
			i = end
			continue
		}
		if s[i] < utf8.RuneSelf {
			start := i
			for i < len(s) && s[i] < utf8.RuneSelf && s[i] != '\x1b' {
				i++
			}
			width += i - start
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		width += runewidth.RuneWidth(r)
		i += size
	}
	return width
}

// scanAnsiSequence parses an ANSI escape sequence that starts at s[i] (which
// must be '\x1b'). It returns the byte index just past the end of the sequence
// and a kind byte:
//
//	'[' — a CSI sequence (\x1b[…<final byte in 0x40..0x7E>).
//	']' — an OSC sequence (\x1b]…<BEL or ESC\>).
//	0   — a bare \x1b not recognised as either; end = i+1.
//
// On malformed input (no terminator) the function advances to the end of s
// rather than looping or panicking.
func scanAnsiSequence(s string, i int) (end int, kind byte) {
	n := len(s)
	if i+1 >= n {
		return i + 1, 0
	}
	switch s[i+1] {
	case '[':
		end = i + 2
		for end < n && (s[end] < '@' || s[end] > '~') {
			end++
		}
		if end < n {
			end++
		}
		return end, '['
	case ']':
		end = i + 2
		for end < n {
			if s[end] == 0x07 {
				return end + 1, ']'
			}
			if s[end] == 0x1b && end+1 < n && s[end+1] == '\\' {
				return end + 2, ']'
			}
			end++
		}
		return end, ']'
	}
	return i + 1, 0
}

// classifyOSC8 inspects a complete OSC sequence and reports whether it is an
// OSC 8 hyperlink open (with a non-empty URI) or close (empty URI). Any other
// OSC sequence returns (false, false).
func classifyOSC8(seq string) (isOpen, isClose bool) {
	// Minimum well-formed OSC 8: "\x1b]8;;\x07" (close, 6 bytes).
	if len(seq) < 6 || seq[0] != 0x1b || seq[1] != ']' || seq[2] != '8' || seq[3] != ';' {
		return false, false
	}
	// Locate the ';' separating params from the URI (after the leading "8;").
	semi := strings.IndexByte(seq[4:], ';')
	if semi < 0 {
		return false, false
	}
	uriStart := 4 + semi + 1
	var termLen int
	switch {
	case strings.HasSuffix(seq, "\x07"):
		termLen = 1
	case strings.HasSuffix(seq, "\x1b\\"):
		termLen = 2
	default:
		return false, false
	}
	if uriStart+termLen > len(seq) {
		return false, false
	}
	if len(seq)-uriStart-termLen == 0 {
		return false, true
	}
	return true, false
}

// osc8Close is the canonical OSC 8 close sequence (empty URI, BEL terminated).
const osc8Close = "\x1b]8;;\x07"

// finalizeOutput rewrites the rendered markdown to make it terminal-ready:
//
//   - OSC 8 hyperlinks that crossed a line break are closed before each '\n'
//     and re-opened on the next line so every line is independently clickable.
//   - Each line is padded with trailing spaces up to width so background fills
//     reach the right edge.
//
// Both transformations are streamed in a single pass over the input — width is
// tracked as we copy bytes and ANSI escape sequences (CSI and OSC) are
// recognised so they are not counted toward visible width.
func finalizeOutput(s string, width int) string {
	if s == "" {
		return s
	}
	needPad := width > 0
	needLink := strings.Contains(s, "\x1b]8;")
	if !needPad && !needLink {
		return s
	}

	var buf strings.Builder
	buf.Grow(len(s) + 64)

	var activeOpen string // most recent OSC 8 open sequence; "" when not inside a link
	lineWidth := 0

	flushNewline := func() {
		if activeOpen != "" {
			buf.WriteString(osc8Close)
		}
		if needPad && lineWidth < width {
			writeSpaces(&buf, width-lineWidth)
		}
		buf.WriteByte('\n')
		if activeOpen != "" {
			buf.WriteString(activeOpen)
		}
		lineWidth = 0
	}

	n := len(s)
	for i := 0; i < n; {
		c := s[i]

		if c == '\n' {
			flushNewline()
			i++
			continue
		}

		if c == '\x1b' {
			end, kind := scanAnsiSequence(s, i)
			if kind == 0 {
				// Lone ESC byte: copy through without advancing line width.
				buf.WriteByte(c)
				i = end
				continue
			}
			seq := s[i:end]
			buf.WriteString(seq)
			if kind == ']' {
				if isOpen, isClose := classifyOSC8(seq); isOpen {
					activeOpen = seq
				} else if isClose {
					activeOpen = ""
				}
			}
			i = end
			continue
		}

		// ASCII fast path: copy a run of plain bytes and count their width.
		if c < utf8.RuneSelf {
			start := i
			for i < n {
				b := s[i]
				if b == '\x1b' || b == '\n' || b >= utf8.RuneSelf {
					break
				}
				i++
			}
			buf.WriteString(s[start:i])
			lineWidth += i - start
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		buf.WriteString(s[i : i+size])
		lineWidth += runewidth.RuneWidth(r)
		i += size
	}

	// Trailing line (no terminating newline): pad to width if needed.
	if needPad && lineWidth < width {
		writeSpaces(&buf, width-lineWidth)
	}

	return buf.String()
}

type token struct {
	text  string
	style ansiStyle
}

// syntaxCacheKey builds a cache key for syntax highlighting results.
type syntaxCacheKey struct {
	lang string
	code string
}

var (
	lexerCache   = make(map[string]chroma.Lexer)
	lexerCacheMu sync.RWMutex

	// Cache for chroma token type to ansiStyle conversion (with code bg)
	chromaStyleCache   = make(map[chroma.TokenType]ansiStyle)
	chromaStyleCacheMu sync.RWMutex

	// Cache for syntax highlighting results to avoid re-tokenizing unchanged code blocks.
	// Uses an LRU cache bounded to 128 entries to prevent unbounded memory growth
	// in long-running TUI sessions with many unique code blocks.
	syntaxHighlightCache   = newLRUCache[syntaxCacheKey, []token](syntaxHighlightCacheSize)
	syntaxHighlightCacheMu sync.RWMutex
)

const (
	// syntaxHighlightCacheSize is the maximum number of syntax-highlighted code blocks
	// to keep in cache. This bounds memory usage while retaining recently viewed blocks.
	syntaxHighlightCacheSize = 128
)

func (p *parser) syntaxHighlight(code, lang string) []token {
	cacheKey := syntaxCacheKey{lang: lang, code: code}

	syntaxHighlightCacheMu.RLock()
	if cached, ok := syntaxHighlightCache.get(cacheKey); ok {
		syntaxHighlightCacheMu.RUnlock()
		return cached
	}
	syntaxHighlightCacheMu.RUnlock()

	tokens := p.doSyntaxHighlight(code, lang)

	syntaxHighlightCacheMu.Lock()
	syntaxHighlightCache.put(cacheKey, tokens)
	syntaxHighlightCacheMu.Unlock()

	return tokens
}

// doSyntaxHighlight performs the actual syntax highlighting without caching.
func (p *parser) doSyntaxHighlight(code, lang string) []token {
	lexer := p.getLexer(lang)
	if lexer == nil {
		return []token{{text: code, style: p.getCodeStyle(chroma.None)}}
	}

	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return []token{{text: code, style: p.getCodeStyle(chroma.None)}}
	}

	chromaTokens := iterator.Tokens()
	tokens := make([]token, 0, len(chromaTokens))
	for _, tok := range chromaTokens {
		if tok.Value == "" {
			continue
		}
		tokens = append(tokens, token{
			text:  tok.Value,
			style: p.getCodeStyle(tok.Type),
		})
	}
	return tokens
}

// getLexer returns a cached chroma lexer for the given language, or nil if unknown.
func (p *parser) getLexer(lang string) chroma.Lexer {
	if lang == "" {
		return nil
	}

	lexerCacheMu.RLock()
	lexer := lexerCache[lang]
	lexerCacheMu.RUnlock()
	if lexer != nil {
		return lexer
	}

	lexer = lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Match("file." + lang)
	}
	if lexer == nil {
		return nil
	}

	lexer = chroma.Coalesce(lexer)
	lexerCacheMu.Lock()
	lexerCache[lang] = lexer
	lexerCacheMu.Unlock()
	return lexer
}

func (p *parser) getCodeStyle(tokenType chroma.TokenType) ansiStyle {
	chromaStyleCacheMu.RLock()
	style, ok := chromaStyleCache[tokenType]
	chromaStyleCacheMu.RUnlock()
	if ok {
		return style
	}

	// Build lipgloss style with code background inherited
	lipStyle := chromaToLipgloss(tokenType, p.styles.chromaStyle).Inherit(p.styles.styleCodeBg)
	style = buildAnsiStyle(lipStyle)

	chromaStyleCacheMu.Lock()
	chromaStyleCache[tokenType] = style
	chromaStyleCacheMu.Unlock()
	return style
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

// wrapText wraps text to the given width, respecting ANSI escape sequences.
// It tracks active CSI styles and re-applies them on continuation lines so a
// styled segment that crosses a wrap is rendered correctly on both lines.
func (p *parser) wrapText(text string, width int) string {
	if width <= 0 || text == "" {
		return text
	}

	// Fast path: if the text fits in one line, return as-is.
	if ansiStringWidth(text) <= width {
		return text
	}

	// Fast path: no spaces — just break the long word.
	if !strings.ContainsAny(text, " \t") {
		broken := breakWord(text, width)
		if len(broken) == 1 {
			return broken[0]
		}
		return strings.Join(broken, "\n")
	}

	var out strings.Builder
	out.Grow(len(text) + len(text)/40)

	var active []string // currently-active CSI styles (cleared on \x1b[m)
	lineWidth := 0

	n := len(text)
	i := 0
	for i < n {
		// Skip any whitespace between words.
		for i < n && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}

		// Collect the next word — plain bytes plus any embedded ANSI sequences.
		wordStart := i
		wordWidth := 0
		var wordAnsi []string
		for i < n && text[i] != ' ' && text[i] != '\t' {
			c := text[i]
			if c == '\x1b' {
				end, kind := scanAnsiSequence(text, i)
				if kind != 0 {
					wordAnsi = append(wordAnsi, text[i:end])
				}
				// Lone ESC: zero-width pass-through (no width change).
				i = end
				continue
			}
			if c < utf8.RuneSelf {
				wordWidth++
				i++
				continue
			}
			r, size := utf8.DecodeRuneInString(text[i:])
			wordWidth += runewidth.RuneWidth(r)
			i += size
		}
		word := text[wordStart:i]

		// Fold this word's ANSI codes into the active set BEFORE handling it.
		// For a long word that gets split, this ensures the continuation-line
		// breaks below close and re-open any styles that were opened inside the
		// word, instead of replaying only the styles active before it.
		active = updateActiveStyles(active, wordAnsi)
		// Layout decision: do we need to wrap before this word?
		haveLine := lineWidth > 0
		needsWrap := haveLine && lineWidth+1+wordWidth > width
		if wordWidth > width && haveLine {
			needsWrap = true
		}
		if needsWrap {
			writeLineBreak(&out, active)
			lineWidth = 0
		} else if haveLine {
			out.WriteByte(' ')
			lineWidth++
		}

		if wordWidth > width {
			// Long word: break it into pieces, separated by line breaks that close
			// and restore the currently-active styles.
			broken := breakWord(word, width)
			for j, part := range broken {
				if j > 0 {
					writeLineBreak(&out, active)
				}
				out.WriteString(part)
			}
			// Always end a broken-word run with a line break so the following
			// word starts on a fresh line.
			writeLineBreak(&out, active)
			lineWidth = 0
		} else {
			out.WriteString(word)
			lineWidth += wordWidth
		}
	}

	return out.String()
}

// writeLineBreak emits a styled line break: it closes any active CSI styles,
// writes '\n', then re-opens those styles so the next line continues with the
// same formatting.
func writeLineBreak(out *strings.Builder, active []string) {
	if len(active) > 0 {
		out.WriteString("\x1b[m")
	}
	out.WriteByte('\n')
	for _, s := range active {
		out.WriteString(s)
	}
}

// updateActiveStyles folds a sequence of newly-seen ANSI codes into the list
// of currently-active CSI styles. OSC sequences are line-local and ignored;
// a full CSI reset (\x1b[m or \x1b[0m) clears the list.
func updateActiveStyles(active, newCodes []string) []string {
	for _, code := range newCodes {
		if strings.HasPrefix(code, "\x1b]") {
			continue
		}
		if code == "\x1b[m" || code == "\x1b[0m" {
			active = active[:0]
		} else {
			active = append(active, code)
		}
	}
	return active
}

func breakWord(word string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{word}
	}

	var parts []string
	var current strings.Builder
	currentWidth := 0

	for i := 0; i < len(word); {
		if word[i] == '\x1b' {
			end, _ := scanAnsiSequence(word, i)
			current.WriteString(word[i:end])
			i = end
			continue
		}

		r, size := utf8.DecodeRuneInString(word[i:])
		rw := runewidth.RuneWidth(r)

		if currentWidth+rw > maxWidth && currentWidth > 0 {
			parts = append(parts, current.String())
			current.Reset()
			currentWidth = 0
		}

		current.WriteRune(r)
		currentWidth += rw
		i += size
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// buildStylePrimitive converts an ansi.StylePrimitive to a lipgloss.Style.
// sanitizeReplacer is a package-level replacer to avoid rebuilding it on every call.
// This is critical for performance - building a replacer is expensive.
var sanitizeReplacer = strings.NewReplacer(
	"\r", "",
	"\b", "",
	"\f", "",
	"\v", "",
)

func sanitizeForTerminal(s string) string {
	if s == "" {
		return s
	}

	// Strip control chars that change cursor position / layout.
	// Keep \n and \t (tab will be expanded later).
	return sanitizeReplacer.Replace(s)
}

func buildStylePrimitive(sp ansi.StylePrimitive) lipgloss.Style {
	style := lipgloss.NewStyle()

	if sp.Color != nil {
		style = style.Foreground(lipgloss.Color(*sp.Color))
	}
	if sp.BackgroundColor != nil {
		style = style.Background(lipgloss.Color(*sp.BackgroundColor))
	}
	if sp.Bold != nil && *sp.Bold {
		style = style.Bold(true)
	}
	if sp.Italic != nil && *sp.Italic {
		style = style.Italic(true)
	}
	if sp.Underline != nil && *sp.Underline {
		style = style.Underline(true)
	}
	if sp.CrossedOut != nil && *sp.CrossedOut {
		style = style.Strikethrough(true)
	}

	return style
}
