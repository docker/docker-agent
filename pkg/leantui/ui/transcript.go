package ui

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/markdowntransform"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/streamcontent"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type blockKind int

type PendingUserKind int

const (
	blockReasoning blockKind = iota
	blockAssistant
)

const (
	PendingUserSteer PendingUserKind = iota
	PendingUserFollowUp
)

type PendingUserMessage struct {
	ID      string
	Display string
	Content string
	Kind    PendingUserKind
}

// pendingBlock accumulates the text of the block currently being streamed.
type pendingBlock struct {
	kind     blockKind
	identity streamcontent.Identity
	text     strings.Builder
	markdown *assistantMarkdown
}

// assistantMarkdown is the display-transform state of one assistant block. It
// moves from the pending block to the archived block, so a segment flushed at
// a tool boundary keeps its result and stays streaming until its message ends.
type assistantMarkdown struct {
	surface   markdowntransform.Surface
	version   uint64
	streaming bool
}

func (s *assistantMarkdown) refresh(display *markdowntransform.Display) bool {
	version := display.Version(&s.surface)
	if version == s.version {
		return false
	}
	s.version = version
	return true
}

// block is a finalized piece of the conversation. Its lines are rendered lazily
// and cached per width, so finalized content is not re-rendered every frame and
// only reflows when the terminal is resized.
type block struct {
	render   func(width int) []string
	identity streamcontent.Identity
	kind     blockKind
	text     string
	markdown *assistantMarkdown
	cacheW   int
	cache    []string
	cached   bool
}

func (b *block) complete() {
	if b.markdown != nil && b.markdown.streaming {
		b.markdown.streaming = false
		b.cached = false
	}
}

func (b *block) lines(width int) []string {
	if !b.cached || b.cacheW != width {
		b.cache = b.render(width)
		b.cacheW = width
		b.cached = true
	}
	return b.cache
}

// Transcript owns everything that scrolls: the finalized conversation blocks,
// the in-progress streamed block, and the in-flight tool calls. Committed
// blocks are immutable scrollback; the pending block and tool calls are the
// live region that changes each frame until they finalize into blocks.
type Transcript struct {
	markdownDisplay *markdowntransform.Display
	settled         bool
	fullFilePreview bool
	blocks          []*block
	pending         *pendingBlock
	toolz           *ToolTracker

	liveToolRows [2]int
}

// NewTranscript creates an empty transcript.
func NewTranscript() *Transcript {
	return &Transcript{toolz: NewToolTracker()}
}

// ClearActive drops the live region (the streamed block and any in-flight tool
// calls) while keeping the committed scrollback intact. Used when starting a
// new session.
func (t *Transcript) ClearActive() {
	t.pending = nil
	t.toolz.Reset()
}

// AddBlock appends a finalized, lazily-rendered block to the conversation.
func (t *Transcript) AddBlock(render func(width int) []string) {
	t.blocks = append(t.blocks, &block{render: render})
}

func (t *Transcript) appendPending(kind blockKind, identity streamcontent.Identity, content string) {
	if content == "" {
		return
	}
	if t.pending == nil || t.pending.kind != kind || t.pending.identity != identity {
		t.FlushPending()
		t.pending = &pendingBlock{kind: kind, identity: identity}
		if kind == blockAssistant {
			t.pending.markdown = &assistantMarkdown{streaming: true}
		}
	}
	t.pending.text.WriteString(content)
}

// AppendReasoning appends streamed reasoning text.
func (t *Transcript) AppendReasoning(content string) {
	t.appendPending(blockReasoning, streamcontent.Identity{}, content)
}

// AppendAssistant appends streamed assistant text.
func (t *Transcript) AppendAssistant(content string) {
	t.appendPending(blockAssistant, streamcontent.Identity{}, content)
}

// FlushPending finalizes the in-progress streamed block into the conversation.
func (t *Transcript) FlushPending() {
	if t.pending == nil {
		return
	}
	text := t.pending.text.String()
	kind := t.pending.kind
	identity := t.pending.identity
	state := t.pending.markdown
	t.pending = nil

	t.blocks = append(t.blocks, t.textBlock(kind, identity, text, state))
}

// CompleteMessage ends streaming for every segment of a logical message,
// including segments archived at tool and reasoning boundaries.
func (t *Transcript) CompleteMessage(identity streamcontent.Identity) {
	t.complete(func(id streamcontent.Identity) bool { return id == identity })
}

// CompleteSession ends streaming for the messages of a session whose stream
// stopped, so a nested stream's replies complete while the parent's stay live.
func (t *Transcript) CompleteSession(sessionID string) {
	t.complete(func(id streamcontent.Identity) bool { return id.SessionID == sessionID })
}

// complete marks matching messages complete without flushing the pending
// block, so chunk boundaries are the same with or without transformers.
func (t *Transcript) complete(match func(streamcontent.Identity) bool) {
	if t.pending != nil && t.pending.markdown != nil && match(t.pending.identity) {
		t.pending.markdown.streaming = false
	}
	for _, b := range t.blocks {
		if match(b.identity) {
			b.complete()
		}
	}
}

// CompleteStream ends streaming for every segment, including legacy streams
// and cancelled turns that never receive their canonical message.
func (t *Transcript) CompleteStream() {
	t.FlushPending()
	for _, b := range t.blocks {
		b.complete()
	}
}

// SetMarkdownDisplay rewrites assistant Markdown before rendering.
func (t *Transcript) SetMarkdownDisplay(display *markdowntransform.Display) {
	t.markdownDisplay = display
}

// RefreshMarkdown reports whether any transformed Markdown changed since the
// last call, invalidating the affected blocks.
func (t *Transcript) RefreshMarkdown() bool {
	changed := false
	for _, b := range t.blocks {
		if b.markdown != nil && b.markdown.refresh(t.markdownDisplay) {
			b.cached = false
			changed = true
		}
	}
	if t.pending != nil && t.pending.markdown != nil && t.pending.markdown.refresh(t.markdownDisplay) {
		changed = true
	}
	return changed
}

func (t *Transcript) assistantLines(state *assistantMarkdown, identity streamcontent.Identity, text string, width int) []string {
	in := markdownInput(state, identity, text, width)
	if t.markdownDisplay == nil || (t.settled && !t.markdownDisplay.Current(&state.surface, in)) {
		return RenderAssistantLines(text, width)
	}
	return renderTransformedLines(t.markdownDisplay.Markdown(&state.surface, in), width)
}

// SettleMarkdown makes rendering exact for the final frame, which nothing
// redraws: a reply shows its latest result, or its source when its latest
// snapshot has no result yet, never an older, possibly truncated result.
func (t *Transcript) SettleMarkdown() {
	t.settled = true
	for _, b := range t.blocks {
		if b.markdown == nil {
			continue
		}
		// A result can land after the block cached an older one but before the
		// batched refresh picks it up.
		refreshed := b.markdown.refresh(t.markdownDisplay)
		if refreshed || (b.cached && !t.markdownDisplay.Current(&b.markdown.surface, markdownInput(b.markdown, b.identity, b.text, b.cacheW))) {
			b.cached = false
		}
	}
}

func markdownInput(state *assistantMarkdown, identity streamcontent.Identity, text string, width int) markdowntransform.Input {
	// Transformers receive the reply without terminal controls, so only their
	// own styling is trusted.
	return markdowntransform.Input{
		Markdown:       markdown.StripTerminalControls(text),
		MessageType:    "assistant",
		IsStreaming:    state.streaming,
		AvailableWidth: max(width, 1),
		SessionID:      identity.SessionID,
		MessageID:      identity.MessageID,
	}
}

func (t *Transcript) textBlock(kind blockKind, identity streamcontent.Identity, text string, state *assistantMarkdown) *block {
	b := &block{kind: kind, identity: identity, text: text, markdown: state}
	b.render = func(w int) []string {
		if kind == blockReasoning {
			return RenderReasoningLines(b.text, w)
		}
		return t.assistantLines(b.markdown, b.identity, b.text, w)
	}
	return b
}

// AppendAssistantContent separates logical messages even within one stream.
func (t *Transcript) AppendAssistantContent(identity streamcontent.Identity, content string) {
	t.appendPending(blockAssistant, identity, content)
}

// AppendReasoningContent shares the assistant message's logical identity.
func (t *Transcript) AppendReasoningContent(identity streamcontent.Identity, content string) {
	t.appendPending(blockReasoning, identity, content)
}

// ResetReasoningContent removes both live reasoning and segments flushed by tools.
func (t *Transcript) ResetReasoningContent(identity streamcontent.Identity) {
	if t.pending != nil && t.pending.kind == blockReasoning && t.pending.identity == identity {
		t.pending = nil
	}
	t.blocks = slices.DeleteFunc(t.blocks, func(b *block) bool {
		return b.kind == blockReasoning && b.identity == identity
	})
}

// ReconcileAssistantContent repairs incomplete live text from the saved message.
func (t *Transcript) ReconcileAssistantContent(identity streamcontent.Identity, content string) {
	if content == "" {
		return
	}
	var matching []*block
	var delivered strings.Builder
	for _, b := range t.blocks {
		if b.identity == identity && b.kind == blockAssistant {
			matching = append(matching, b)
			delivered.WriteString(b.text)
		}
	}
	pending := t.pending != nil && t.pending.kind == blockAssistant && t.pending.identity == identity
	if pending {
		delivered.WriteString(t.pending.text.String())
	}
	if delivered.String() == content {
		return
	}
	if len(matching) == 0 && !pending {
		t.AppendAssistantContent(identity, content)
		return
	}
	if suffix, ok := strings.CutPrefix(content, delivered.String()); ok {
		if pending {
			t.pending.text.WriteString(suffix)
		} else {
			b := matching[len(matching)-1]
			b.text += suffix
			b.cached = false
		}
		return
	}
	for i, b := range matching {
		b.text = ""
		if i == 0 {
			b.text = content
		}
		b.cached = false
	}
	if pending {
		t.pending.text.Reset()
		if len(matching) == 0 {
			t.pending.text.WriteString(content)
		}
	}
}

// UpsertTool creates or updates an in-flight tool call.
func (t *Transcript) UpsertTool(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status tuitypes.ToolStatus) {
	t.toolz.Upsert(agentName, toolCall, toolDef, status)
}

// Tool returns an in-flight tool call by id.
func (t *Transcript) Tool(id string) *ToolView { return t.toolz.Get(id) }

// RemoveTool removes an in-flight tool call by id.
func (t *Transcript) RemoveTool(id string) { t.toolz.Remove(id) }

// FinishTool commits completed calls in invocation order, keeping later results
// in the live region until earlier calls finish.
func (t *Transcript) FinishTool(id string, result ToolResult, sessionState service.SessionStateReader) {
	t.toolz.Complete(id, result)
	for _, view := range t.toolz.DrainCompleted() {
		t.AddBlock(func(w int) []string {
			view.fullFilePreview = t.fullFilePreview
			return RenderToolWithState(view, w, 0, sessionState)
		})
	}
}

// FinalizeTools commits every in-flight tool with the given terminal status.
func (t *Transcript) FinalizeTools(status tuitypes.ToolStatus, sessionState service.SessionStateReader) {
	for _, view := range t.toolz.FinalizeAll(status) {
		t.AddBlock(func(w int) []string {
			view.fullFilePreview = t.fullFilePreview
			return RenderToolWithState(view, w, 0, sessionState)
		})
	}
}

// Lines renders everything that scrolls: finalized blocks, the in-progress
// streamed block, running tool calls, and user messages waiting to be accepted
// by the runtime. A blank line separates each entry.
func (t *Transcript) Lines(width, spinnerFrame int, _ bool, sessionState service.SessionStateReader, pendingUsers []PendingUserMessage) []string {
	var lines []string
	for _, b := range t.blocks {
		lines = append(lines, b.lines(width)...)
		lines = append(lines, "")
	}
	if t.pending != nil {
		lines = append(lines, t.pendingLines(width)...)
		lines = append(lines, "")
	}
	t.liveToolRows[0] = len(lines)
	t.toolz.ForEach(func(tv *ToolView) {
		tv.fullFilePreview = t.fullFilePreview
		lines = append(lines, RenderToolWithState(tv, width, spinnerFrame, sessionState)...)
		lines = append(lines, "")
	})
	t.liveToolRows[1] = len(lines)
	for _, msg := range pendingUsers {
		lines = append(lines, RenderPendingUserLines(msg, width)...)
		lines = append(lines, "")
	}
	return lines
}

// LiveToolRows returns the half-open live tool range in the last rendered frame.
func (t *Transcript) LiveToolRows() [2]int { return t.liveToolRows }

// BlockCount reports the number of committed transcript blocks.
func (t *Transcript) BlockCount() int { return len(t.blocks) }

// BlockLines renders a committed block by index.
func (t *Transcript) BlockLines(index, width int) []string {
	if index < 0 || index >= len(t.blocks) {
		return nil
	}
	return t.blocks[index].lines(width)
}

// ToolCount reports the number of in-flight tool calls.
func (t *Transcript) ToolCount() int { return t.toolz.Len() }

// ToolByIDCount reports the number of tracked tool-call ids.
func (t *Transcript) ToolByIDCount() int { return t.toolz.ByIDLen() }

// pendingLines renders the message currently being streamed. Assistant text is
// rendered as markdown live (the same renderer used once it is finalized), so
// formatting appears as it streams.
func (t *Transcript) pendingLines(width int) []string {
	text := t.pending.text.String()
	switch t.pending.kind {
	case blockReasoning:
		return RenderReasoningLines(text, width)
	case blockAssistant:
		return t.assistantLines(t.pending.markdown, t.pending.identity, text, width)
	default:
		return nil
	}
}

func spinnerLine(frame int) string {
	f := spinnerFrames[frame%len(spinnerFrames)]
	return StAccent().Render(f) + " " + StMuted().Render("Working…")
}

// SetFullFilePreview switches written-file previews between all lines and the last ten.
func (t *Transcript) SetFullFilePreview(full bool) {
	t.fullFilePreview = full
	for _, b := range t.blocks {
		b.cached = false
	}
}

// FullFilePreview reports whether written-file previews show all lines.
func (t *Transcript) FullFilePreview() bool {
	return t.fullFilePreview
}
