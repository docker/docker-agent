package ui

import (
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
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
}

// block is a finalized piece of the conversation. Its lines are rendered lazily
// and cached per width, so finalized content is not re-rendered every frame and
// only reflows when the terminal is resized.
type block struct {
	render   func(width int) []string
	identity streamcontent.Identity
	kind     blockKind
	text     string
	cacheW   int
	cache    []string
	cached   bool
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
	t.pending = nil

	t.blocks = append(t.blocks, textBlock(kind, identity, text))
}

func textBlock(kind blockKind, identity streamcontent.Identity, text string) *block {
	b := &block{kind: kind, identity: identity, text: text}
	b.render = func(w int) []string {
		if kind == blockReasoning {
			return RenderReasoningLines(b.text, w)
		}
		return RenderAssistantLines(b.text, w)
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
		t.AddBlock(func(w int) []string { return RenderToolWithState(view, w, 0, sessionState) })
	}
}

// FinalizeTools commits every in-flight tool with the given terminal status.
func (t *Transcript) FinalizeTools(status tuitypes.ToolStatus, sessionState service.SessionStateReader) {
	for _, view := range t.toolz.FinalizeAll(status) {
		t.AddBlock(func(w int) []string { return RenderToolWithState(view, w, 0, sessionState) })
	}
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
	t.liveToolRows[0] = len(lines)
	t.toolz.ForEach(func(tv *ToolView) {
		lines = append(lines, RenderToolWithState(tv, width, spinnerFrame, sessionState)...)
		lines = append(lines, "")
	})
	t.liveToolRows[1] = len(lines)
	if busy && t.pending == nil && t.toolz.Empty() {
		lines = append(lines, spinnerLine(spinnerFrame), "")
	}
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
		return RenderAssistantLines(text, width)
	default:
		return nil
	}
}

func spinnerLine(frame int) string {
	f := spinnerFrames[frame%len(spinnerFrames)]
	return StAccent().Render(f) + " " + StMuted().Render("Working…")
}
