package markdown

import "strings"

// IncrementalRenderer is a markdown renderer specialized for streaming use:
// it remembers the most recently rendered "stable prefix" of the input and,
// when the next call extends that prefix, re-renders only the new trailing
// region instead of the whole document.
//
// A "stable prefix" here is the portion of the input ending at the last block
// boundary that the underlying FastRenderer is guaranteed to render the same
// way regardless of what comes after it: a blank line that lies outside any
// fenced code block and is not flanked by list or blockquote lines (those
// constructs absorb blank lines into a single block, so the surrounding
// content is not yet "frozen").
//
// The optimization is correct because the FastRenderer (and CommonMark in
// general) treats such blank lines as hard breaks between top-level blocks:
// rendering the document up to a stable boundary and rendering the rest
// independently produces the same output as rendering the full document,
// modulo the blank-line separator that joinPrefixAndTail re-inserts.
//
// IncrementalRenderer is not safe for concurrent use.
type IncrementalRenderer struct {
	width int

	// inputPrefix is the longest stable prefix of the most recent input that
	// ends at a block boundary. outputPrefix is its rendered counterpart.
	// Both are empty until the first successful incremental render.
	inputPrefix  string
	outputPrefix string

	// codeBlocksPrefix is the list of code blocks emitted while rendering the
	// cached prefix, with Line indices relative to outputPrefix.
	codeBlocksPrefix []CodeBlock

	// fallback is used for the actual rendering work; it is reused across calls
	// so its parser pool (and chroma caches) stay warm.
	fallback *FastRenderer
}

// NewIncrementalRenderer creates a new incremental renderer with the given
// terminal width.
func NewIncrementalRenderer(width int) *IncrementalRenderer {
	return &IncrementalRenderer{
		width:    width,
		fallback: NewFastRenderer(width),
	}
}

// Render produces styled terminal output for input. When successive calls pass
// inputs that share a long common prefix (the streaming case), only the suffix
// is parsed and rendered; the rest is served from the cached output.
func (r *IncrementalRenderer) Render(input string) (string, error) {
	out, _, err := r.RenderWithCodeBlocks(input)
	return out, err
}

// RenderWithCodeBlocks behaves like Render but additionally returns the list
// of fenced code blocks in the rendered output. Each entry's Line is the
// 0-indexed line within the returned string where the block's copy label is
// drawn.
func (r *IncrementalRenderer) RenderWithCodeBlocks(input string) (string, []CodeBlock, error) {
	if input == "" {
		r.inputPrefix = ""
		r.outputPrefix = ""
		r.codeBlocksPrefix = nil
		return "", nil, nil
	}

	// If the new input no longer starts with our cached prefix, the user (or a
	// retry) replaced earlier content; fall back to a full render.
	if r.inputPrefix == "" || !strings.HasPrefix(input, r.inputPrefix) {
		return r.fullRender(input)
	}

	// Locate a fresh stable boundary anywhere up to the end of the current
	// input. We always re-render from the previous boundary onward to keep the
	// implementation simple and correctness-preserving: the previously emitted
	// tail may have been an unfinished paragraph that should now be part of a
	// completed block.
	tail := input[len(r.inputPrefix):]
	boundary := stableBoundary(tail)
	if boundary <= 0 {
		// No new block boundary in the tail yet — render only the tail and
		// concatenate. Cached prefix is unchanged.
		renderedTail, tailBlocks, err := r.fallback.RenderWithCodeBlocks(tail)
		if err != nil {
			return r.fullRender(input)
		}
		out := r.joinPrefixAndTail(r.outputPrefix, renderedTail)
		return out, r.mergeCodeBlocks(r.outputPrefix, r.codeBlocksPrefix, tailBlocks), nil
	}

	// We have a new boundary inside the tail. Render the new stable region
	// (inputPrefix + tail[:boundary]) once, append it to the cache, then render
	// the new tail.
	newStableTail := tail[:boundary]
	renderedStableTail, stableBlocks, err := r.fallback.RenderWithCodeBlocks(newStableTail)
	if err != nil {
		return r.fullRender(input)
	}
	newBlocks := r.mergeCodeBlocks(r.outputPrefix, r.codeBlocksPrefix, stableBlocks)
	r.inputPrefix += newStableTail
	r.outputPrefix = r.joinPrefixAndTail(r.outputPrefix, renderedStableTail)
	r.codeBlocksPrefix = newBlocks

	rest := tail[boundary:]
	if rest == "" {
		return r.outputPrefix, cloneCodeBlocks(r.codeBlocksPrefix), nil
	}
	renderedRest, restBlocks, err := r.fallback.RenderWithCodeBlocks(rest)
	if err != nil {
		return r.fullRender(input)
	}
	out := r.joinPrefixAndTail(r.outputPrefix, renderedRest)
	return out, r.mergeCodeBlocks(r.outputPrefix, r.codeBlocksPrefix, restBlocks), nil
}

// SetWidth updates the renderer width. Width changes invalidate the cache
// because the rendered output is width-dependent.
func (r *IncrementalRenderer) SetWidth(width int) {
	if width == r.width {
		return
	}
	r.width = width
	r.fallback = NewFastRenderer(width)
	r.inputPrefix = ""
	r.outputPrefix = ""
	r.codeBlocksPrefix = nil
}

// Reset drops the cached prefix without changing the width. Use when the
// underlying message is replaced by an unrelated new one.
func (r *IncrementalRenderer) Reset() {
	r.inputPrefix = ""
	r.outputPrefix = ""
	r.codeBlocksPrefix = nil
}

// fullRender renders input from scratch, refreshes the cache, and returns the
// result. To avoid rendering input twice (once whole, once for the cached
// prefix), we split input at its longest stable boundary and render the two
// pieces separately, then join. The two render calls on smaller inputs are
// faster than one big render plus a separate prefix render, and the prefix
// piece can be reused as outputPrefix.
func (r *IncrementalRenderer) fullRender(input string) (string, []CodeBlock, error) {
	boundary := stableBoundary(input)
	if boundary <= 0 {
		out, blocks, err := r.fallback.RenderWithCodeBlocks(input)
		if err != nil {
			return "", nil, err
		}
		r.inputPrefix = ""
		r.outputPrefix = ""
		r.codeBlocksPrefix = nil
		return out, blocks, nil
	}

	prefix := input[:boundary]
	rest := input[boundary:]
	renderedPrefix, prefixBlocks, err := r.fallback.RenderWithCodeBlocks(prefix)
	if err != nil {
		return "", nil, err
	}
	if rest == "" {
		r.inputPrefix = prefix
		r.outputPrefix = renderedPrefix
		r.codeBlocksPrefix = prefixBlocks
		return renderedPrefix, cloneCodeBlocks(prefixBlocks), nil
	}
	renderedRest, restBlocks, err := r.fallback.RenderWithCodeBlocks(rest)
	if err != nil {
		return "", nil, err
	}
	r.inputPrefix = prefix
	r.outputPrefix = renderedPrefix
	r.codeBlocksPrefix = prefixBlocks
	out := r.joinPrefixAndTail(renderedPrefix, renderedRest)
	return out, r.mergeCodeBlocks(renderedPrefix, prefixBlocks, restBlocks), nil
}

// joinPrefixAndTail concatenates a previously rendered prefix and a freshly
// rendered tail to form the equivalent of a single-shot render of
// prefix-content + "\n\n" + tail-content.
//
// Two FastRenderer details drive the body of this function:
//
//  1. FastRenderer trims trailing newlines from its output, so each piece
//     ends mid-line and we have to reintroduce a separator newline.
//  2. FastRenderer's finalizeOutput pads every line (including blank ones) to
//     `width` with spaces. The blank line we insert between the two pieces
//     therefore needs the same padding to be byte-identical to a full render.
//
// If either of those FastRenderer behaviours changes in the future, the
// IncrementalRendererMatchesFullRender* tests will fail and pinpoint this
// function as the place to update.
func (r *IncrementalRenderer) joinPrefixAndTail(prefix, tail string) string {
	if prefix == "" {
		return tail
	}
	if tail == "" {
		return prefix
	}
	if r.width <= 0 {
		return prefix + "\n\n" + tail
	}
	var b strings.Builder
	b.Grow(len(prefix) + len(tail) + r.width + 2)
	b.WriteString(prefix)
	b.WriteByte('\n')
	for range r.width {
		b.WriteByte(' ')
	}
	b.WriteByte('\n')
	b.WriteString(tail)
	return b.String()
}

// joinSeparatorLines is the number of extra rendered lines that
// joinPrefixAndTail inserts between the prefix output and the tail output:
// one to terminate the prefix's final (untrailing-newlined) line, and one
// blank-padded separator line. Keep this in sync with joinPrefixAndTail.
const joinSeparatorLines = 2

// mergeCodeBlocks returns the union of code blocks from a cached prefix output
// and a freshly rendered tail. Tail block line indices are shifted past the
// prefix's lines and the separator that joinPrefixAndTail inserts.
func (r *IncrementalRenderer) mergeCodeBlocks(prefixOut string, prefixBlocks, tailBlocks []CodeBlock) []CodeBlock {
	if len(prefixBlocks) == 0 && len(tailBlocks) == 0 {
		return nil
	}
	out := make([]CodeBlock, 0, len(prefixBlocks)+len(tailBlocks))
	out = append(out, prefixBlocks...)
	if len(tailBlocks) == 0 {
		return out
	}
	offset := 0
	if prefixOut != "" {
		offset = strings.Count(prefixOut, "\n") + joinSeparatorLines
	}
	for _, b := range tailBlocks {
		out = append(out, CodeBlock{Content: b.Content, Line: b.Line + offset})
	}
	return out
}

func cloneCodeBlocks(in []CodeBlock) []CodeBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]CodeBlock, len(in))
	copy(out, in)
	return out
}

// stableBoundary returns the byte index just after the last "safe" block
// boundary in input, or 0 if no safe boundary exists. A safe boundary is a
// blank line that the FastRenderer treats as a hard break between top-level
// blocks: it must lie outside any fenced code block AND the lines immediately
// surrounding it must not be "blank-line tolerant" constructs (lists,
// blockquotes) where the parser keeps absorbing lines across the blank.
//
// The returned index satisfies input[:boundary] ends with a newline and a
// blank line, and input[boundary:] starts a fresh top-level block.
func stableBoundary(input string) int {
	if input == "" {
		return 0
	}

	// classifyFenceLines and classifyListLikeLines emit one entry per logical
	// line, including a final trailing line if input does not end with '\n'.
	// They therefore have either len(lineEnds) or len(lineEnds)+1 entries; the
	// loop below only indexes positions in [0, len(lineEnds)+1), which is
	// always within bounds, but we still guard each access defensively to keep
	// the function robust against future refactors of the helpers.
	inFence := classifyFenceLines(input)
	isListLike := classifyListLikeLines(input, inFence)

	lineEnds := make([]int, 0, 64)
	for i := range len(input) {
		if input[i] == '\n' {
			lineEnds = append(lineEnds, i)
		}
	}

	// Walk newlines from the end backwards looking for a blank line (two
	// consecutive '\n' bytes) that isn't inside a fence and whose neighbours
	// are not list/blockquote lines (which would absorb the blank).
	for k := len(lineEnds) - 1; k > 0; k-- {
		i := lineEnds[k]
		if input[i-1] != '\n' {
			continue
		}
		if inFenceAt(inFence, k) {
			continue
		}
		boundary := i + 1
		if boundary >= len(input) {
			continue
		}
		if inFenceAt(inFence, k+1) {
			continue
		}
		if listLikeAt(isListLike, k-1) || listLikeAt(isListLike, k+1) {
			continue
		}
		return boundary
	}
	return 0
}

func inFenceAt(inFence []bool, k int) bool {
	return k >= 0 && k < len(inFence) && inFence[k]
}

func listLikeAt(isListLike []bool, k int) bool {
	return k >= 0 && k < len(isListLike) && isListLike[k]
}

// classifyListLikeLines marks every line that looks like the start of a list
// item, an ordered-list continuation, an indented continuation of a list
// item, or a blockquote. The FastRenderer absorbs blank lines between such
// lines into a single block, so they are not safe split points.
func classifyListLikeLines(input string, inFence []bool) []bool {
	var lines []bool
	lineStart := 0
	idx := 0
	for i := 0; i <= len(input); i++ {
		if i < len(input) && input[i] != '\n' {
			continue
		}
		line := input[lineStart:i]
		listLike := false
		if idx < len(inFence) && !inFence[idx] {
			trimmed := strings.TrimLeft(line, " \t")
			switch {
			case strings.HasPrefix(trimmed, ">"):
				listLike = true
			case isListStart(trimmed):
				listLike = true
			case line != "" && (line[0] == ' ' || line[0] == '\t'):
				// Indented non-empty line: could be a list-item continuation. We
				// can't be sure without the surrounding context, so treat it as
				// list-like to stay on the safe side.
				listLike = trimmed != ""
			}
		}
		lines = append(lines, listLike)
		lineStart = i + 1
		idx++
	}
	return lines
}

// classifyFenceLines returns a slice indexed by line number where each entry
// is true when that line lies inside a fenced code block. The opening and
// closing fence lines themselves are marked true so a boundary cannot land
// directly on them.
func classifyFenceLines(input string) []bool {
	var lines []bool
	openFence := ""
	lineStart := 0
	for i := 0; i <= len(input); i++ {
		if i < len(input) && input[i] != '\n' {
			continue
		}
		line := input[lineStart:i]
		trimmed := strings.TrimLeft(line, " \t")
		switch {
		case openFence != "":
			lines = append(lines, true)
			if strings.HasPrefix(strings.TrimSpace(trimmed), openFence) {
				openFence = ""
			}
		case strings.HasPrefix(trimmed, "```"):
			lines = append(lines, true)
			openFence = "```"
		case strings.HasPrefix(trimmed, "~~~"):
			lines = append(lines, true)
			openFence = "~~~"
		default:
			lines = append(lines, false)
		}
		lineStart = i + 1
	}
	return lines
}
