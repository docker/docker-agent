package message

import (
	"context"
	"fmt"
	"maps"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	maxUserMessageLines       = 30
	collapsedUserMessageLines = 5
)

// Model represents a view that can render a message
type Model interface {
	layout.Model
	layout.Sizeable
	SetMessage(msg *types.Message) tea.Cmd
	AppendContent(content string) tea.Cmd
	SetSelected(selected bool)
	SetHovered(hovered bool)
	CodeBlocks() []markdown.CodeBlock
	// Finalize releases per-message render state that is only needed while the
	// message is actively streaming. The message content and code-block metadata
	// are preserved; calling View() afterwards still produces correct output
	// without retaining a per-view render cache or IncrementalRenderer.
	Finalize()
	// HasLiveRenderState reports whether this view currently retains a
	// populated renderCache or an IncrementalRenderer instance. Used by tests
	// to assert that finalized views have actually released their per-message
	// render state without reaching into unexported fields via reflection.
	HasLiveRenderState() bool
	// RenderedSegments exposes immutable header/stable blocks separately from
	// the mutable tail so transcript follow-tail rendering need not flatten the
	// complete active response on every chunk.
	RenderedSegments(width int) (AssistantSegments, bool)
}

// AssistantSegments is a line-oriented active assistant rendering. Header and
// Stable are retained by the message view and immutable until the next width
// change; Tail contains only the mutable markdown block.
type AssistantSegments struct {
	Header []string
	Stable []string
	Tail   []string
}

// messageModel implements Model
type messageModel struct {
	message  *types.Message
	previous *types.Message

	width    int
	height   int
	focused  bool
	selected bool
	hovered  bool
	expanded bool
	spinner  spinner.Spinner

	// renderCache memoizes the output of Render(width) keyed by the inputs
	// that affect its output. During streaming, View() and Height() are called
	// in pairs for each new chunk, and the chat list also re-renders for hover
	// tracking and scroll updates; without this cache each call would re-parse
	// the entire accumulated markdown from scratch.
	renderCache renderCache

	// codeBlocks holds the fenced code blocks emitted by the last call to
	// render() for assistant messages, with Line indices translated into the
	// messageModel's own View() output coordinate system (i.e. zero-indexed
	// from the first line of View()).
	codeBlocks []markdown.CodeBlock

	// mdRenderer is reused across renders of an assistant message so that
	// streamed-in chunks only re-render the trailing block instead of the whole
	// accumulated markdown each time.
	mdRenderer        *markdown.IncrementalRenderer
	streamLines       assistantStreamLines
	segmentCodeBlocks []markdown.CodeBlock
	contentBuf        strings.Builder
	imageScanOffset   int

	// finalized is set by Finalize() once the message is no longer the active
	// streaming view. After it is set, Render() still produces correct output,
	// but does not store anything in renderCache and does not retain an
	// IncrementalRenderer between calls — both are pure caches whose memory
	// dominates a long session, and they are not worth keeping for messages
	// that are unlikely to be re-rendered hot.
	finalized bool

	markdownImages  map[string]tuiimage.Inline
	loadingImages   map[string]bool
	markdownImageID int
}

type assistantStreamLines struct {
	width       int
	stable      string
	stableLines []string
	headerKey   string
	headerLines []string
}

type markdownImagesLoadedMsg struct {
	target    *messageModel
	requested []tuiimage.MarkdownReference
	images    map[string]tuiimage.Inline
}

type markdownImageRenderedMsg struct{}

type markdownImagePlaceholder struct {
	token string
	lines []string
}

// renderCache stores the most recent Render result keyed by the inputs that
// can change its output. The key is small enough (a string and a few flags)
// that comparing it is much cheaper than rendering markdown.
type renderCache struct {
	valid     bool
	content   string
	msgType   types.MessageType
	width     int
	selected  bool
	hovered   bool
	expanded  bool
	editable  bool
	sameAgent bool
	result    string
	imageID   int
}

// New creates a new message view
func New(ar *animation.Runtime, msg, previous *types.Message) *messageModel {
	imageScanOffset := -1
	if msg != nil && msg.Type == types.MessageTypeAssistant {
		refs := tuiimage.MarkdownReferences(msg.Content)
		imageScanOffset = nextUnresolvedImageOpener(msg.Content, 0, refs)
	}
	return &messageModel{
		message:         msg,
		previous:        previous,
		width:           80, // Default width
		height:          1,  // Will be calculated
		focused:         false,
		imageScanOffset: imageScanOffset,
		spinner:         spinner.New(ar, spinner.ModeBoth, styles.SpinnerDotsAccentStyle),
	}
}

// Bubble Tea Model methods

// Init initializes the message view
func (mv *messageModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if mv.message.Type == types.MessageTypeSpinner || mv.message.Type == types.MessageTypeLoading {
		cmds = append(cmds, mv.spinner.Init())
	}
	if cmd := mv.loadMarkdownImages(mv.message); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (mv *messageModel) SetMessage(msg *types.Message) tea.Cmd {
	// Un-finalize when the underlying message is changed (e.g. streaming
	// resumes into this view). Finalize is meant for views that have
	// permanently lost their actively-streaming status; mutating the message
	// re-arms the per-message caches so subsequent renders are fast again.
	mv.finalized = false
	// If the new content is not an extension of the previous one (different
	// message, or the message was edited), drop the IncrementalRenderer's
	// cached prefix so its memory is released immediately rather than on the
	// next render. The renderer detects mismatches on its own and falls back
	// to a full render either way, so this is purely an optimization.
	if mv.mdRenderer != nil && mv.message != nil && msg != nil && !strings.HasPrefix(msg.Content, mv.message.Content) {
		mv.mdRenderer.Reset()
	}
	mv.message = msg
	mv.contentBuf.Reset()
	if msg != nil {
		mv.contentBuf.WriteString(msg.Content)
	}
	mv.imageScanOffset = -1
	mv.renderCache.valid = false
	if msg == nil || msg.Type != types.MessageTypeAssistant {
		return nil
	}
	refs := tuiimage.MarkdownReferences(msg.Content)
	mv.imageScanOffset = nextUnresolvedImageOpener(msg.Content, 0, refs)
	return mv.loadMarkdownImageReferences(refs)
}

func (mv *messageModel) AppendContent(content string) tea.Cmd {
	if content == "" || mv.message == nil {
		return nil
	}
	if mv.contentBuf.Len() == 0 && mv.message.Content != "" {
		mv.contentBuf.WriteString(mv.message.Content)
	}
	oldLen := mv.contentBuf.Len()
	mv.contentBuf.WriteString(content)
	mv.message.Content = mv.contentBuf.String()
	mv.renderCache.valid = false
	// Keep only an offset into canonical content. The one-byte lookback finds an
	// opener split as "!" then "[" without retaining or duplicating streamed text.
	if mv.imageScanOffset < 0 {
		scanStart := max(oldLen-1, 0)
		if relative := strings.Index(mv.message.Content[scanStart:], "!["); relative >= 0 {
			mv.imageScanOffset = scanStart + relative
		}
	}
	if mv.imageScanOffset < 0 {
		return nil
	}
	// Parse the complete document so Markdown context (notably inline and
	// fenced code) decides whether a raw opener is actually an image.
	refs := tuiimage.MarkdownReferences(mv.message.Content)
	mv.imageScanOffset = nextUnresolvedImageOpener(mv.message.Content, mv.imageScanOffset, refs)
	return mv.loadMarkdownImageReferences(refs)
}

func nextUnresolvedImageOpener(content string, start int, refs []tuiimage.MarkdownReference) int {
	for start < len(content) {
		relative := strings.Index(content[start:], "![")
		if relative < 0 {
			return -1
		}
		opener := start + relative
		resolved := false
		for _, ref := range refs {
			if ref.Start <= opener && opener < ref.End {
				resolved = true
				break
			}
		}
		if !resolved {
			return opener
		}
		start = opener + 2
	}
	return -1
}

func (mv *messageModel) loadMarkdownImages(msg *types.Message) tea.Cmd {
	if msg == nil || msg.Type != types.MessageTypeAssistant {
		return nil
	}
	return mv.loadMarkdownImageReferences(tuiimage.MarkdownReferences(msg.Content))
}

func (mv *messageModel) loadMarkdownImageReferences(refs []tuiimage.MarkdownReference) tea.Cmd {
	pending := make([]tuiimage.MarkdownReference, 0, len(refs))
	if mv.loadingImages == nil {
		mv.loadingImages = make(map[string]bool)
	}
	for _, ref := range refs {
		if _, loaded := mv.markdownImages[ref.Source]; loaded || mv.loadingImages[ref.Source] {
			continue
		}
		mv.loadingImages[ref.Source] = true
		pending = append(pending, ref)
	}
	if len(pending) == 0 {
		return nil
	}

	return func() tea.Msg {
		loaded := make(map[string]tuiimage.Inline)
		for _, ref := range pending {
			if image, ok := tuiimage.LoadMarkdownReference(context.Background(), ref); ok {
				loaded[ref.Source] = image
			}
		}
		return markdownImagesLoadedMsg{target: mv, requested: pending, images: loaded}
	}
}

func (mv *messageModel) SetSelected(selected bool) {
	if mv.selected != selected {
		mv.selected = selected
		mv.renderCache.valid = false
	}
}

func (mv *messageModel) SetHovered(hovered bool) {
	if mv.hovered != hovered {
		mv.hovered = hovered
		mv.renderCache.valid = false
	}
}

// Update handles messages and updates the message view state
func (mv *messageModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if loaded, ok := msg.(markdownImagesLoadedMsg); ok && loaded.target == mv {
		// Unmark failed URLs so a later SetMessage can retry them.
		for _, ref := range loaded.requested {
			if _, success := loaded.images[ref.Source]; !success {
				delete(mv.loadingImages, ref.Source)
			}
		}
		if len(loaded.images) > 0 {
			if mv.markdownImages == nil {
				mv.markdownImages = make(map[string]tuiimage.Inline)
			}
			maps.Copy(mv.markdownImages, loaded.images)
			mv.markdownImageID++
			mv.renderCache.valid = false
		}
		return mv, func() tea.Msg { return markdownImageRenderedMsg{} }
	}
	if mv.message.Type == types.MessageTypeSpinner || mv.message.Type == types.MessageTypeLoading {
		s, cmd := mv.spinner.Update(msg)
		mv.spinner = s.(spinner.Spinner)
		return mv, cmd
	}
	return mv, nil
}

// Toggle switches between expanded and collapsed state.
func (mv *messageModel) Toggle() {
	mv.expanded = !mv.expanded
	mv.renderCache.valid = false
}

// IsToggleLine returns true if the line contains the expand/collapse affordance.
func (mv *messageModel) IsToggleLine(lineIdx int) bool {
	if mv.message == nil || mv.message.Type != types.MessageTypeUser {
		return false
	}
	content := strings.TrimRight(mv.message.Content, "\n\r\t ")
	if strings.Count(content, "\n")+1 <= maxUserMessageLines {
		return false
	}

	// The indicator is placed at the end of the message view with a leading \n\n.
	// The view has an action row in place of top padding and 1 line of bottom padding.
	// height-1 is the bottom padding.
	// height-2 is the text of the indicator ("[-] click to collapse").
	// height-3 is the empty line above it.
	// By checking >= height-3, we provide a generous clickable area exactly on the toggle.
	height := mv.Height(mv.width)
	return lineIdx >= height-3
}

func (mv *messageModel) RenderedSegments(width int) (AssistantSegments, bool) {
	msg := mv.message
	if msg == nil || msg.Type != types.MessageTypeAssistant || msg.Content == "" || mv.selected || len(mv.markdownImages) != 0 {
		return AssistantSegments{}, false
	}
	messageStyle := styles.AssistantMessageStyle
	innerWidth := width - messageStyle.GetHorizontalFrameSize()
	if mv.mdRenderer == nil {
		mv.mdRenderer = markdown.NewIncrementalRenderer(innerWidth)
	} else {
		mv.mdRenderer.SetWidth(innerWidth)
	}
	parts, err := mv.mdRenderer.RenderParts(msg.Content)
	if err != nil {
		return AssistantSegments{}, false
	}
	cache := &mv.streamLines
	widthChanged := cache.width != width
	if widthChanged || !strings.HasPrefix(parts.StablePrefix, cache.stable) {
		cache.width, cache.stable = width, ""
		cache.stableLines = nil
	}
	if cache.stable != parts.StablePrefix {
		delta := parts.StablePrefix[len(cache.stable):]
		if cache.stable != "" && strings.HasPrefix(delta, "\n") {
			delta = strings.TrimPrefix(delta, "\n")
		}
		cache.stableLines = append(cache.stableLines, styledAssistantLines(messageStyle, width, delta)...)
		cache.stable = parts.StablePrefix
	}
	header := actionRow(innerWidth, mv.hovered, types.MessageCopyLabel)
	prefix := ""
	if !mv.sameAgentAsPrevious(msg) {
		prefix = mv.senderPrefix(msg.Sender)
	}
	headerKey := prefix + header
	if cache.headerKey != headerKey || widthChanged {
		cache.headerKey = headerKey
		cache.headerLines = nil
		if prefix != "" {
			cache.headerLines = append(cache.headerLines, strings.Split(strings.TrimSuffix(prefix, "\n"), "\n")...)
		}
		cache.headerLines = append(cache.headerLines, styledAssistantLines(messageStyle, width, header)...)
	}
	tailLines := styledAssistantLines(messageStyle, width, parts.MutableTail)
	if parts.MutableTail != "" && parts.StablePrefix != "" {
		separator := styledAssistantLines(messageStyle, width, strings.Repeat(" ", max(innerWidth, 0)))
		tailLines = append(separator, tailLines...)
	}

	prefixLines := len(cache.headerLines)
	mv.segmentCodeBlocks = nil
	if len(parts.CodeBlocks) > 0 {
		mv.segmentCodeBlocks = make([]markdown.CodeBlock, len(parts.CodeBlocks))
	}
	for i, cb := range parts.CodeBlocks {
		mv.segmentCodeBlocks[i] = markdown.CodeBlock{Content: cb.Content, Line: cb.Line + prefixLines}
	}
	if len(mv.segmentCodeBlocks) == 0 {
		mv.codeBlocks = nil
	} else {
		mv.codeBlocks = append(mv.codeBlocks[:0], mv.segmentCodeBlocks...)
	}
	return AssistantSegments{Header: cache.headerLines, Stable: cache.stableLines, Tail: tailLines}, true
}

func styledAssistantLines(style lipgloss.Style, width int, content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(style.Width(width).Render(content), "\n"), "\n")
}

// View renders the message view
func (mv *messageModel) View() string {
	return mv.Render(mv.width)
}

// Render renders the message view content. Results are memoized so repeated
// calls with the same inputs (very common during streaming, hover tracking,
// and from Height()) skip the expensive markdown parse.
func (mv *messageModel) Render(width int) string {
	msg := mv.message

	// Spinner-driven types (MessageTypeSpinner, MessageTypeLoading, and an empty
	// MessageTypeAssistant placeholder) animate on every tick, so the result is
	// not cacheable. Everything else is a pure function of the inputs tracked in
	// renderCache below.
	// Finalized messages skip writing into renderCache so the per-view
	// retained ANSI string does not pile up across long sessions; the chat
	// list's bounded LRU still memoizes their rendered output.
	cacheable := !mv.isSpinnerDriven() && !mv.finalized
	if cacheable {
		c := &mv.renderCache
		if c.valid &&
			c.width == width &&
			c.msgType == msg.Type &&
			c.selected == mv.selected &&
			c.hovered == mv.hovered &&
			c.expanded == mv.expanded &&
			c.editable == (msg.SessionPosition != nil) &&
			c.content == msg.Content &&
			c.sameAgent == mv.sameAgentAsPrevious(msg) &&
			c.imageID == mv.markdownImageID {
			return c.result
		}
	}

	result := mv.render(width)

	if cacheable {
		mv.renderCache = renderCache{
			valid:     true,
			content:   msg.Content,
			msgType:   msg.Type,
			width:     width,
			selected:  mv.selected,
			hovered:   mv.hovered,
			expanded:  mv.expanded,
			editable:  msg.SessionPosition != nil,
			sameAgent: mv.sameAgentAsPrevious(msg),
			result:    result,
			imageID:   mv.markdownImageID,
		}
	}
	return result
}

// isSpinnerDriven reports whether the rendered output animates on every tick
// and therefore cannot be cached across renders.
func (mv *messageModel) isSpinnerDriven() bool {
	switch mv.message.Type {
	case types.MessageTypeSpinner, types.MessageTypeLoading:
		return true
	case types.MessageTypeAssistant:
		return mv.message.Content == "" && len(mv.message.AssistantMedia) == 0
	}
	return false
}

// render is the uncached rendering core. Render() wraps it with memoization.
func (mv *messageModel) render(width int) string {
	msg := mv.message
	switch msg.Type {
	case types.MessageTypeSpinner:
		if msg.Content == "" {
			return mv.spinner.View() // top-level: keep the playful spinner
		}
		// Delegated stream: animated glyph + per-agent-colored "parent → child".
		glyph := styles.SpinnerDotsAccentStyle.MarginLeft(2).Render(mv.spinner.RawFrame())
		return glyph + " " + styles.AgentAccentStyleFor(msg.Sender).Render(msg.Content)
	case types.MessageTypeUser:
		// Choose style based on selection state
		messageStyle := styles.UserMessageStyle
		if mv.selected && msg.SessionPosition != nil {
			messageStyle = styles.SelectedUserMessageStyle
		}

		formatUserContent := func(c string) string {
			c = strings.TrimRight(c, "\n\r\t ")
			if c == "" {
				return msg.Content
			}

			totalLines := strings.Count(c, "\n") + 1
			if totalLines > maxUserMessageLines {
				if !mv.expanded {
					parts := strings.SplitN(c, "\n", collapsedUserMessageLines+1)
					visibleLines := strings.Join(parts[:collapsedUserMessageLines], "\n")
					hiddenCount := totalLines - collapsedUserMessageLines
					indicator := "\n\n" + styles.MutedStyle.Render(fmt.Sprintf("[+] expand %d more lines", hiddenCount))
					return visibleLines + indicator
				}
				indicator := "\n\n" + styles.MutedStyle.Render("[-] collapse")
				return c + indicator
			}
			return c
		}

		innerWidth := width - messageStyle.GetHorizontalFrameSize()
		content := formatUserContent(msg.Content)

		// Action labels live in the top padding row and appear on hover or
		// selection: edit (when the message can be branched) and copy.
		var labels []string
		if msg.SessionPosition != nil {
			labels = append(labels, types.UserMessageEditLabel)
		}
		labels = append(labels, types.MessageCopyLabel)
		topRow := actionRow(innerWidth, mv.hovered || mv.selected, labels...)

		// Use a modified style with no top padding (the action row replaces it)
		noTopPaddingStyle := messageStyle.PaddingTop(0)
		return noTopPaddingStyle.Width(width).Render(topRow + "\n" + content)
	case types.MessageTypeAssistant:
		if msg.Content == "" && len(msg.AssistantMedia) == 0 {
			return mv.spinner.View()
		}

		messageStyle := styles.AssistantMessageStyle
		if mv.selected {
			messageStyle = styles.SelectedMessageStyle
		}

		innerRenderWidth := width - messageStyle.GetHorizontalFrameSize()
		content, imagePlaceholders := mv.markdownImagePlaceholders(msg.Content, innerRenderWidth)
		rendered, codeBlocks, err := mv.renderAssistantMarkdown(content, innerRenderWidth)
		if err != nil {
			rendered = content
			codeBlocks = nil
		}
		rendered, codeBlocks = replaceMarkdownImagePlaceholders(rendered, codeBlocks, imagePlaceholders)
		rendered = appendAssistantMediaLines(rendered, msg.AssistantMedia, innerRenderWidth)

		var prefix string
		if !mv.sameAgentAsPrevious(msg) {
			prefix = mv.senderPrefix(msg.Sender)
		}

		// Always reserve a top row to avoid layout shifts when the copy label
		// appears on hover. When not hovered, the row is filled with spaces
		// (invisible). AssistantMessageStyle has PaddingTop=0, so this extra
		// row acts as a stable spacer.
		innerWidth := width - messageStyle.GetHorizontalFrameSize()
		topRow := actionRow(innerWidth, mv.hovered || mv.selected, types.MessageCopyLabel)

		// Translate the markdown-relative line indices into messageModel View()
		// coordinates. The rendered markdown is preceded by the sender prefix
		// (when shown) and the always-present topRow line inside the styled
		// envelope, so the first line of `rendered` lands at this offset.
		prefixLines := 0
		if prefix != "" {
			prefixLines = strings.Count(prefix, "\n")
		}
		lineOffset := prefixLines + 1 // +1 for topRow
		if len(codeBlocks) > 0 {
			mv.codeBlocks = make([]markdown.CodeBlock, len(codeBlocks))
			for i, cb := range codeBlocks {
				mv.codeBlocks[i] = markdown.CodeBlock{
					Content: cb.Content,
					Line:    cb.Line + lineOffset,
				}
			}
		} else {
			mv.codeBlocks = nil
		}

		return prefix + messageStyle.Width(width).Render(topRow+"\n"+rendered)
	case types.MessageTypeShellOutput:
		if rendered, blocks, err := markdown.NewFastRenderer(width).RenderWithCodeBlocks(fmt.Sprintf("```console\n%s\n```", msg.Content)); err == nil {
			// The view has no envelope, so block lines map 1:1 to View() lines,
			// making the per-block copy affordance clickable.
			mv.codeBlocks = blocks
			return rendered
		}
		mv.codeBlocks = nil
		return msg.Content
	case types.MessageTypeCancelled:
		return styles.WarningStyle.Render("⚠ stream cancelled ⚠")
	case types.MessageTypeAgentReturn:
		return renderAgentReturn(msg.Sender, msg.Content, width)
	case types.MessageTypeWelcome:
		messageStyle := styles.WelcomeMessageStyle
		// Convert explicit newlines to markdown hard line breaks (two trailing spaces)
		// This preserves line breaks from YAML multiline syntax (|) while still
		// allowing markdown formatting like **bold** and *italic*
		content := preserveLineBreaks(msg.Content)
		rendered, blocks, err := markdown.NewFastRenderer(width - messageStyle.GetHorizontalFrameSize()).RenderWithCodeBlocks(content)
		if err != nil {
			rendered = msg.Content
			blocks = nil
		}
		// Translate block lines into View() coordinates so the per-block copy
		// affordance is clickable: the style envelope prepends its top border
		// and padding rows before the rendered markdown.
		lineOffset := messageStyle.GetBorderTopSize() + messageStyle.GetPaddingTop()
		mv.codeBlocks = nil
		for _, cb := range blocks {
			mv.codeBlocks = append(mv.codeBlocks, markdown.CodeBlock{Content: cb.Content, Line: cb.Line + lineOffset})
		}
		return messageStyle.Width(width - 1).Render(strings.TrimRight(rendered, "\n\r\t "))
	case types.MessageTypeError:
		// Render the error content with a clickable retry affordance on its own
		// trailing line so the user can resume the conversation after a failure.
		retryHint := styles.MutedStyle.Render(types.ErrorRetryLabel)
		content := msg.Content + "\n\n" + retryHint
		return styles.ErrorMessageStyle.Width(width - 1).Render(content)
	case types.MessageTypeLoading:
		// Show spinner with the loading description, truncated to fit width
		spinnerView := mv.spinner.View()
		spinnerWidth := ansi.StringWidth(spinnerView) + 1 // +1 for space separator
		maxDescWidth := width - spinnerWidth
		description := msg.Content
		if maxDescWidth > 0 && ansi.StringWidth(description) > maxDescWidth {
			description = ansi.Truncate(description, maxDescWidth, "…")
		}
		return spinnerView + " " + styles.MutedStyle.Render(description)
	default:
		return msg.Content
	}
}

func (mv *messageModel) markdownImagePlaceholders(content string, width int) (string, []markdownImagePlaceholder) {
	refs := tuiimage.MarkdownReferences(content)
	var output strings.Builder
	placeholders := make([]markdownImagePlaceholder, 0, len(refs))
	cursor := 0
	for i, ref := range refs {
		image, loaded := mv.markdownImages[ref.Source]
		if !loaded || ref.Start < cursor || ref.End <= ref.Start {
			continue
		}
		markerLines := tuiimage.RenderMarkers(image, width)
		if len(markerLines) == 0 {
			continue
		}

		name := ref.Alt
		if name == "" {
			name = image.Name
		}
		lines := make([]string, 0, len(markerLines)+1)
		if name != "" {
			lines = append(lines, "  "+styles.MutedStyle.Render(name))
		}
		lines = append(lines, markerLines...)
		token := fmt.Sprintf("CAGENTIMAGEPLACEHOLDER%dTOKEN", i)

		output.WriteString(content[cursor:ref.Start])
		output.WriteString("\n\n" + token + "\n\n")
		cursor = ref.End
		placeholders = append(placeholders, markdownImagePlaceholder{token: token, lines: lines})
	}
	if len(placeholders) == 0 {
		return content, nil
	}
	output.WriteString(content[cursor:])
	return output.String(), placeholders
}

func replaceMarkdownImagePlaceholders(rendered string, codeBlocks []markdown.CodeBlock, placeholders []markdownImagePlaceholder) (string, []markdown.CodeBlock) {
	if len(placeholders) == 0 {
		return rendered, codeBlocks
	}
	byToken := make(map[string][]string, len(placeholders))
	for _, placeholder := range placeholders {
		byToken[placeholder.token] = placeholder.lines
	}

	lines := strings.Split(rendered, "\n")
	result := make([]string, 0, len(lines))
	type lineShift struct {
		line  int
		delta int
	}
	var shifts []lineShift
	for lineIndex, line := range lines {
		token := strings.TrimSpace(ansi.Strip(line))
		replacement, ok := byToken[token]
		if !ok {
			result = append(result, line)
			continue
		}
		result = append(result, replacement...)
		shifts = append(shifts, lineShift{line: lineIndex, delta: len(replacement) - 1})
	}
	for i := range codeBlocks {
		// Compare against the pre-replacement line so later shifts don't
		// re-match against already-adjusted positions.
		orig := codeBlocks[i].Line
		for _, shift := range shifts {
			if shift.line < orig {
				codeBlocks[i].Line += shift.delta
			}
		}
	}
	return strings.Join(result, "\n"), codeBlocks
}

// appendAssistantMediaLines appends generated-media blocks after the
// rendered markdown, preserving the text-then-media order of the assistant
// turn. Appending never shifts earlier lines, so code-block coordinates
// computed for the markdown remain valid.
func appendAssistantMediaLines(rendered string, media []types.AssistantMedia, width int) string {
	blocks := make([]string, 0, len(media))
	for _, m := range media {
		if lines := assistantMediaLines(m, width); len(lines) > 0 {
			blocks = append(blocks, strings.Join(lines, "\n"))
		}
	}
	if len(blocks) == 0 {
		return rendered
	}
	joined := strings.Join(blocks, "\n\n")
	if rendered = strings.TrimRight(rendered, "\n\r\t "); rendered == "" {
		return joined
	}
	return rendered + "\n\n" + joined
}

// assistantMediaLines renders one generated-media item: a muted name label
// plus kitty marker rows when the image is renderable (mirroring the
// markdown-image layout above), or the item's safe textual fallback when
// graphics are unavailable or the image never decoded.
func assistantMediaLines(media types.AssistantMedia, width int) []string {
	if media.Image != nil {
		if markers := tuiimage.RenderMarkers(*media.Image, width); len(markers) > 0 {
			lines := make([]string, 0, len(markers)+1)
			if media.Image.Name != "" {
				lines = append(lines, "  "+styles.MutedStyle.Render(media.Image.Name))
			}
			return append(lines, markers...)
		}
	}
	if media.Fallback == "" {
		return nil
	}
	return []string{styles.MutedStyle.Width(width).Render(media.Fallback)}
}

// renderAssistantMarkdown renders streamed assistant content using a per-message
// IncrementalRenderer. The renderer remembers the last rendered stable prefix
// so each new chunk only re-parses the trailing region. The first render at a
// given width is equivalent to a fresh full render.
//
// For finalized messages we use a transient renderer that is discarded after
// each call. Finalized messages are no longer streamed, so the prefix-cache
// inside an IncrementalRenderer is not earning its keep — keeping it resident
// across the lifetime of every historical message in a session is the
// dominant source of retained memory in long sessions. The parent message
// list's bounded rendered-item LRU can still memoize finalized message output
// without storing an additional per-view copy.
//
// It also returns the list of fenced code blocks emitted by the renderer so
// that callers can map clicks on the per-block copy affordance back to the
// underlying raw code.
func (mv *messageModel) renderAssistantMarkdown(content string, width int) (string, []markdown.CodeBlock, error) {
	if mv.finalized {
		r := markdown.NewIncrementalRenderer(width)
		return r.RenderWithCodeBlocks(content)
	}
	if mv.mdRenderer == nil {
		mv.mdRenderer = markdown.NewIncrementalRenderer(width)
	} else {
		mv.mdRenderer.SetWidth(width)
	}
	return mv.mdRenderer.RenderWithCodeBlocks(content)
}

// actionRow builds the right-aligned row of click affordances (edit, copy)
// rendered in place of a message's top padding. When show is false the row is
// blank so it acts as a stable spacer and hovering never shifts the layout.
// At very narrow widths leading labels are dropped, then the row is hard-
// truncated: it must never wrap, as that would change the message height on
// hover and invalidate click hit-testing until the next full render.
func actionRow(innerWidth int, show bool, labels ...string) string {
	innerWidth = max(innerWidth, 0)
	if !show {
		return strings.Repeat(" ", innerWidth)
	}
	joined := strings.Join(labels, types.MessageActionSeparator)
	for len(labels) > 1 && ansi.StringWidth(joined) > innerWidth {
		labels = labels[1:]
		joined = strings.Join(labels, types.MessageActionSeparator)
	}
	joined = ansi.Truncate(joined, innerWidth, "")
	padding := max(innerWidth-ansi.StringWidth(joined), 0)
	return strings.Repeat(" ", padding) + styles.MutedStyle.Render(joined)
}

func (mv *messageModel) senderPrefix(sender string) string {
	if sender == "" {
		return ""
	}
	return styles.AgentBadgeStyleFor(sender).MarginLeft(2).Render(sender) + "\n\n"
}

// renderAgentReturn renders the delegation-return transition: the returning
// child's badge, a muted "returned control to" connector, and the parent's
// badge — the same visual language as the transfer_task/handoff headers, kept
// visually light. The line is word-wrapped to the width and each wrapped line
// hard-capped, so narrow chat columns never overflow (the cap only ever trims
// trailing wrap spaces).
func renderAgentReturn(fromAgent, toAgent string, width int) string {
	line := styles.AgentBadgeStyleFor(fromAgent).MarginLeft(2).Render(fromAgent) +
		styles.MutedStyle.Render(" "+types.AgentReturnLabel+" ") +
		styles.AgentBadgeStyleFor(toAgent).Render(toAgent)
	if width <= 0 {
		return line
	}
	lines := strings.Split(ansi.Wrap(line, width, ""), "\n")
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "")
	}
	return strings.Join(lines, "\n")
}

// sameAgentAsPrevious returns true if the previous message was from the same agent
func (mv *messageModel) sameAgentAsPrevious(msg *types.Message) bool {
	if mv.previous == nil || mv.previous.Sender != msg.Sender {
		return false
	}
	switch mv.previous.Type {
	case types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeToolResult:
		return true
	default:
		return false
	}
}

// Height calculates the height needed for this message view. Render() is
// memoized, so calling it from here does not duplicate work when View() is
// invoked for the same inputs.
func (mv *messageModel) Height(width int) int {
	content := mv.Render(width)
	return strings.Count(content, "\n") + 1
}

// Message returns the underlying message
func (mv *messageModel) Message() *types.Message {
	return mv.message
}

// CodeBlocks returns the fenced code blocks emitted by the most recent render
// of this message, with Line indices expressed in View() output coordinates.
// Returns nil when the message has no code blocks or has not been rendered
// yet (e.g. non-assistant messages).
func (mv *messageModel) CodeBlocks() []markdown.CodeBlock {
	return mv.codeBlocks
}

// Layout.Sizeable methods

// StopAnimation stops the spinner animation and unregisters from the animation coordinator.
// This must be called when the view is removed from the UI to avoid leaked animation subscriptions.
func (mv *messageModel) StopAnimation() {
	if mv.message.Type == types.MessageTypeSpinner || mv.message.Type == types.MessageTypeLoading {
		mv.spinner.Stop()
	}
}

// Finalize releases per-message render state that no longer needs to be kept
// resident once the message is no longer the actively streaming view. This is
// called by the parent message list when a new top-level message arrives, and
// for every historical view loaded from a session.
//
// Finalize is a no-op for non-assistant message types: only assistant views
// allocate an IncrementalRenderer and accumulate large rendered ANSI strings
// during streaming, so user messages, tool calls, error/welcome banners and
// the like have nothing to release. Setting `finalized = true` on those views
// would only have the side-effect of permanently disabling renderCache for
// selected/hovered states (which bypass the parent's bounded LRU), forcing a
// fresh re-render on every animation tick. Restricting the disable to
// assistant views keeps the leak fix scoped to the type that actually leaks.
//
// The retained payload of an assistant view is dominated by the renderCache
// (a copy of the rendered ANSI string) and the IncrementalRenderer's internal
// caches (last rendered prefix, glamour AST state). Both are pure render
// state — they can be regenerated from mv.message on demand. We deliberately
// leave mv.message, mv.codeBlocks and the spinner untouched so that View()
// keeps returning correct output, click-targeting on code blocks still works,
// and the spinner-driven types continue to animate.
//
// Finalize is idempotent and durable: subsequent renders do not re-populate
// renderCache or store an IncrementalRenderer on the struct. This is
// important because the parent message list invalidates its own LRU on
// several events (spinner removal, theme change, window resize) and would
// otherwise re-render every previously finalized view, putting the per-
// message render state right back where it was.
func (mv *messageModel) Finalize() {
	if mv.message == nil || mv.message.Type != types.MessageTypeAssistant {
		return
	}
	mv.renderCache = renderCache{}
	mv.imageScanOffset = -1
	if mv.mdRenderer != nil {
		mv.mdRenderer.Reset()
		mv.mdRenderer = nil
	}
	mv.finalized = true
}

// HasLiveRenderState reports whether this view still retains per-message
// render state — either a populated renderCache or an IncrementalRenderer
// instance. Used as a structural assertion in regression tests that verify
// Finalize() actually released what it was supposed to release.
func (mv *messageModel) HasLiveRenderState() bool {
	return mv.renderCache.result != "" || mv.mdRenderer != nil
}

// SetSize sets the dimensions of the message view
func (mv *messageModel) SetSize(width, height int) tea.Cmd {
	if mv.width != width {
		mv.renderCache.valid = false
	}
	mv.width = width
	mv.height = height
	return nil
}

// GetSize returns the current dimensions
func (mv *messageModel) GetSize() (width, height int) {
	return mv.width, mv.height
}

// preserveLineBreaks preserves leading indentation by converting leading spaces
// to non-breaking spaces (U+00A0) which won't be stripped by markdown parsers.
// Line breaks are handled by glamour.WithPreservedNewLines().
func preserveLineBreaks(s string) string {
	if !strings.Contains(s, "\n") {
		return preserveIndentation(s)
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = preserveIndentation(line)
	}
	return strings.Join(lines, "\n")
}

// preserveIndentation converts leading spaces in a line to non-breaking spaces (U+00A0).
// This prevents markdown parsers from stripping leading whitespace while maintaining
// the same visual appearance in terminal output.
func preserveIndentation(line string) string {
	if line == "" || line[0] != ' ' {
		return line
	}
	leadingSpaces := 0
	for _, c := range line {
		if c == ' ' {
			leadingSpaces++
		} else {
			break
		}
	}
	if leadingSpaces == 0 {
		return line
	}
	return strings.Repeat("\u00A0", leadingSpaces) + line[leadingSpaces:]
}
