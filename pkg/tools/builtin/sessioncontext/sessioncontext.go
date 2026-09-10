// Package sessioncontext provides read-only tools that let an agent discover
// previous sessions and pull one in as context for the current session,
// without the manual export/import (HTML export + @mention) workflow.
//
// The toolset is a metadata stub — the runtime owns the handlers
// (pkg/runtime/sessioncontext_handlers.go) so they can reach the live session
// store and exclude the session that is currently running.
package sessioncontext

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameListSessions = "list_sessions"
	ToolNameReadSession  = "read_session"
)

const (
	// DefaultListLimit is the number of sessions list_sessions returns when the
	// caller does not specify a limit.
	DefaultListLimit = 20
	// MaxListLimit caps how many sessions list_sessions will return in one call
	// so a large history cannot flood the context window.
	MaxListLimit = 100
	// DefaultMaxChars bounds the size of a rendered transcript so pulling in a
	// long session cannot blow the current context window. When a transcript is
	// larger, the oldest messages are dropped and a note records the omission.
	DefaultMaxChars = 24000
)

type ToolSet struct{}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

func CreateToolSet() (tools.ToolSet, error) {
	return &ToolSet{}, nil
}

// New builds a toolset for tests and embedders.
func New() *ToolSet {
	return &ToolSet{}
}

func (t *ToolSet) Instructions() string {
	return `## Session Context Tools

Use this toolset to reuse work from a previous session as context, instead of asking the user to repeat it.

- ` + "`list_sessions(limit?)`" + ` returns recent sessions (most recent first) with their id, title, creation time and message count. The session you are running in is never listed.
- ` + "`read_session(session_id)`" + ` returns the conversation transcript of a previous session so you can use it as context. ` + "`session_id`" + ` accepts a concrete id from ` + "`list_sessions`" + `, or a relative reference like ` + "`-1`" + ` (most recent), ` + "`-2`" + ` (second most recent), and so on. Long transcripts are truncated from the start, keeping the most recent messages.

Call ` + "`list_sessions`" + ` first to find the right session, then ` + "`read_session`" + ` to pull it in. Only read a session when its content is actually relevant to the current task.`
}

type ListSessionsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum number of sessions to return, most recent first. Defaults to 20 and is capped at 100."`
}

type ReadSessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session to read. Use an id from list_sessions, or a relative reference like '-1' (most recent), '-2' (second most recent)."`
}

// Tools advertises the metadata only; Handler is intentionally nil so the
// runtime's toolMap takes over (same pattern as handoff).
func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameListSessions,
			Category:    "session_context",
			Description: "List previous sessions (most recent first) with their id, title, creation time and message count. The current session is never included.",
			Parameters:  tools.MustSchemaFor[ListSessionsArgs](),
			Annotations: tools.ToolAnnotations{
				Title:        "List Sessions",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameReadSession,
			Category:    "session_context",
			Description: "Read the conversation transcript of a previous session to use it as context. Accepts a concrete session id or a relative reference like '-1'. Long transcripts are truncated, keeping the most recent messages.",
			Parameters:  tools.MustSchemaFor[ReadSessionArgs](),
			Annotations: tools.ToolAnnotations{
				Title:        "Read Session",
				ReadOnlyHint: true,
			},
		},
	}, nil
}

// SessionInfo is the per-session payload returned by list_sessions.
type SessionInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	CreatedAt   string `json:"created_at"`
	NumMessages int    `json:"num_messages"`
	Starred     bool   `json:"starred,omitempty"`
}

// ClampLimit normalizes a caller-supplied limit: non-positive falls back to the
// default, and anything above the cap is reduced to it.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

// Header carries the session metadata rendered above a transcript.
type Header struct {
	ID          string
	Title       string
	CreatedAt   time.Time
	NumMessages int
}

// RenderTranscript renders a previous session as a readable transcript. When
// the rendered text would exceed maxChars, the oldest messages are dropped
// (the most recent ones are the most useful for continuing work) and a note
// records how many were omitted. maxChars <= 0 selects DefaultMaxChars.
func RenderTranscript(h Header, msgs []chat.Message, maxChars int) string {
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}

	var head strings.Builder
	title := strings.TrimSpace(h.Title)
	if title == "" {
		title = "(untitled)"
	}
	fmt.Fprintf(&head, "# Session %s — %s\n", h.ID, title)
	if !h.CreatedAt.IsZero() {
		fmt.Fprintf(&head, "Created: %s\n", h.CreatedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&head, "Messages: %d\n", h.NumMessages)

	blocks := make([]string, 0, len(msgs))
	for i := range msgs {
		if b := renderMessage(msgs[i]); b != "" {
			blocks = append(blocks, b)
		}
	}

	// Keep the most recent blocks that fit in the remaining budget. The header
	// always stays; the omission note (when needed) is counted against the
	// budget so the final string respects maxChars.
	budget := maxChars - head.Len()
	kept := 0
	used := 0
	for _, b := range slices.Backward(blocks) {
		cost := len(b) + 1 // +1 for the joining newline
		if used+cost > budget && kept > 0 {
			break
		}
		used += cost
		kept++
	}
	omitted := len(blocks) - kept

	var out strings.Builder
	out.WriteString(head.String())
	if omitted > 0 {
		fmt.Fprintf(&out, "\n[%d earlier message(s) omitted to fit the context budget; showing the most recent %d]\n", omitted, kept)
	}
	for _, b := range blocks[len(blocks)-kept:] {
		out.WriteString("\n")
		out.WriteString(b)
		out.WriteString("\n")
	}
	return out.String()
}

// renderMessage turns one chat message into a transcript block, or "" when it
// carries nothing worth showing (e.g. an empty implicit message).
func renderMessage(m chat.Message) string {
	content := strings.TrimSpace(messageText(m))

	var b strings.Builder
	switch m.Role {
	case chat.MessageRoleUser:
		b.WriteString("## user\n")
	case chat.MessageRoleAssistant:
		b.WriteString("## assistant\n")
	case chat.MessageRoleTool:
		b.WriteString("## tool result\n")
	default:
		b.WriteString("## " + string(m.Role) + "\n")
	}

	if content != "" {
		b.WriteString(content)
		b.WriteString("\n")
	}
	for _, tc := range m.ToolCalls {
		name := tc.Function.Name
		if name == "" {
			name = "(unknown)"
		}
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			fmt.Fprintf(&b, "→ called tool `%s`\n", name)
		} else {
			fmt.Fprintf(&b, "→ called tool `%s` with %s\n", name, args)
		}
	}

	rendered := strings.TrimRight(b.String(), "\n")
	// Header-only blocks (no content, no tool calls) carry no information.
	if !strings.Contains(rendered, "\n") {
		return ""
	}
	return rendered
}

// messageText extracts the textual content of a message, falling back to the
// text parts of a multi-content payload when Content is empty.
func messageText(m chat.Message) string {
	if strings.TrimSpace(m.Content) != "" {
		return m.Content
	}
	if len(m.MultiContent) == 0 {
		return ""
	}
	var parts []string
	for _, p := range m.MultiContent {
		if t := strings.TrimSpace(p.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
