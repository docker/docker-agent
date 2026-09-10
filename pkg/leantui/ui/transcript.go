package ui

import (
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
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
	Display string
	Content string
	Kind    PendingUserKind
}

// pendingBlock accumulates the text of the block currently being streamed.
type pendingBlock struct {
	kind blockKind
	text strings.Builder
}

// block is a finalized piece of the conversation. Its lines are rendered lazily
// and cached per width, so finalized content is not re-rendered every frame and
// only reflows when the terminal is resized.
type block struct {
	render func(width int) []string
	cacheW int
	cache  []string
	cached bool
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
	blocks  []*block
	pending *pendingBlock
	toolz   *ToolTracker
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

func (t *Transcript) appendPending(kind blockKind, content string) {
	if content == "" {
		return
	}
	if t.pending == nil || t.pending.kind != kind {
		t.FlushPending()
		t.pending = &pendingBlock{kind: kind}
	}
	t.pending.text.WriteString(content)
}

// AppendReasoning appends streamed reasoning text.
func (t *Transcript) AppendReasoning(content string) { t.appendPending(blockReasoning, content) }

// AppendAssistant appends streamed assistant text.
func (t *Transcript) AppendAssistant(content string) { t.appendPending(blockAssistant, content) }

// FlushPending finalizes the in-progress streamed block into the conversation.
func (t *Transcript) FlushPending() {
	if t.pending == nil {
		return
	}
	text := t.pending.text.String()
	kind := t.pending.kind
	t.pending = nil

	switch kind {
	case blockReasoning:
		t.AddBlock(func(w int) []string { return RenderReasoningLines(text, w) })
	case blockAssistant:
		t.AddBlock(func(w int) []string { return RenderAssistantLines(text, w) })
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

// FinishTool commits a completed tool call as an immutable block.
func (t *Transcript) FinishTool(id string, result ToolResult, sessionState service.SessionStateReader) {
	view := t.toolz.Finish(id, result)
	if view == nil {
		return
	}
	t.AddBlock(func(w int) []string { return RenderToolWithState(view, w, 0, sessionState) })
}

// Lines renders everything that scrolls: finalized blocks, the in-progress
// streamed block, running tool calls, and user messages waiting to be accepted
// by the runtime. A blank line separates each entry. The spinner is shown only
// while busy with nothing yet streaming.
func (t *Transcript) Lines(width, spinnerFrame int, busy bool, sessionState service.SessionStateReader, pendingUsers []PendingUserMessage) []string {
	var lines []string
	for _, b := range t.blocks {
		lines = append(lines, b.lines(width)...)
		lines = append(lines, "")
	}
	if t.pending != nil {
		lines = append(lines, t.pendingLines(width)...)
		lines = append(lines, "")
	}
	t.toolz.ForEach(func(tv *ToolView) {
		lines = append(lines, RenderToolWithState(tv, width, spinnerFrame, sessionState)...)
		lines = append(lines, "")
	})
	if busy && t.pending == nil && t.toolz.Empty() {
		lines = append(lines, spinnerLine(spinnerFrame), "")
	}
	for _, msg := range pendingUsers {
		lines = append(lines, RenderPendingUserLines(msg, width)...)
		lines = append(lines, "")
	}
	return lines
}

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
		return RenderAssistantLines(text, width)
	default:
		return nil
	}
}

func spinnerLine(frame int) string {
	f := spinnerFrames[frame%len(spinnerFrames)]
	return StAccent().Render(f) + " " + StMuted().Render("Working…")
}
