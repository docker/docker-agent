package messages

import tea "charm.land/bubbletea/v2"

// RoutedMsg wraps a message with a session ID for routing.
// Runtime events are wrapped in this type so the TUI can route
// them to the correct tab/session.
type RoutedMsg struct {
	SessionID string  // The session ID this message is for
	Inner     tea.Msg // The wrapped message
}

// SpawnSessionMsg is sent when a new session should be created.
type SpawnSessionMsg struct {
	WorkingDir string // The working directory for the new session
}

// SwitchTabMsg requests switching to a different session tab.
type SwitchTabMsg struct {
	SessionID string // The session to switch to
}

// CloseTabMsg requests closing a session tab.
type CloseTabMsg struct {
	SessionID string // The session to close
}

// ReorderTabMsg requests moving a tab from one position to another.
type ReorderTabMsg struct {
	FromIdx int
	ToIdx   int
}

// TabInfo contains display information for a session tab.
type TabInfo struct {
	SessionID      string // Unique session identifier
	Title          string // Display title
	IsActive       bool   // Whether this is the currently active tab
	IsRunning      bool   // Whether the session is currently streaming
	NeedsAttention bool   // Whether the tab needs user attention (e.g., tool confirmation)
}

// TabsUpdatedMsg is sent when the tab list has changed.
type TabsUpdatedMsg struct {
	Tabs      []TabInfo
	ActiveIdx int
}

// WorkingStateChangedMsg is emitted by the content view when working state changes.
// tui.Model uses this to update the editor's working indicator and resize handle spinner.
type WorkingStateChangedMsg struct {
	Working     bool
	QueueLength int
}

// BellMsg is sent when the terminal bell should be rung to notify the user.
// This is used when an inactive tab needs attention (e.g., tool confirmation).
type BellMsg struct{}
