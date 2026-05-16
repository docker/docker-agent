package messages

// EditUserMessageMsg requests entering edit mode for a user message.
type EditUserMessageMsg struct {
	MsgIndex        int    // TUI message index (directly usable, no re-computation needed)
	SessionPosition int    // Session position for branching
	OriginalContent string // Original message content
}

// BranchFromEditMsg requests branching from a session position with new content.
type BranchFromEditMsg struct {
	ParentSessionID  string
	BranchAtPosition int
	Content          string
	Attachments      []Attachment
}

// InvalidateStatusBarMsg signals that the statusbar cache should be invalidated.
// This is emitted when bindings change (e.g., entering/exiting inline edit mode).
type InvalidateStatusBarMsg struct{}

// FocusPanel identifies a focusable panel in the TUI.
type FocusPanel int

const (
	PanelEditor       FocusPanel = iota // Focus the editor/input panel
	PanelMessages                       // Focus the messages/content panel
	PanelSidebarTitle                   // Focus content panel for sidebar title editing (no message selection)
)

// RequestFocusMsg is emitted by the chat page to request the parent to change panel focus.
// The editor and focus state are managed by the parent (tui.Model), not by the chat page.
type RequestFocusMsg struct {
	Target FocusPanel
	// ClickX and ClickY carry the mouse coordinates that triggered the focus
	// request so the parent can focus the correct message. A zero value for
	// both means "no click context" (e.g. keyboard-driven focus).
	ClickX, ClickY int
}
