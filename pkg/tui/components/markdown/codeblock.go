package markdown

// CodeBlockCopyIcon is the unicode glyph rendered at the top-right corner of
// every fenced code block. Clicking on it copies the block's raw content to
// the clipboard. It matches the glyph used for the message-level copy
// affordance so the visual language stays consistent; the two are
// disambiguated by line index, not by glyph.
const CodeBlockCopyIcon = "\u2398"

// CodeBlock describes a fenced code block emitted by the renderer.
//
// Line is the 0-indexed line, within the renderer's output, where the copy
// icon is rendered on the code block's top padding row. Content holds the
// raw code (without ANSI styling) so callers can place it on the clipboard.
type CodeBlock struct {
	Content string
	Line    int
}
