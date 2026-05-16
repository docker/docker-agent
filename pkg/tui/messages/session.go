package messages

import "github.com/docker/docker-agent/pkg/session"

// Attachment represents content attached to a message. It is either a reference
// to a file on disk (FilePath is set) or inline content already in memory
// (Content is set, e.g. pasted text). When FilePath is set the consumer reads
// and classifies the file at send time; when only Content is set the consumer
// uses it directly as inline text. This design lets us add binary-file support
// (images, PDFs, …) in the future by extending the struct with a MimeType hint.
type Attachment struct {
	// Name is the human-readable label (e.g. "paste-1", "main.go").
	Name string
	// FilePath is the resolved, absolute path to a file on disk.
	// Empty when the content is supplied inline (paste attachments).
	FilePath string
	// Content holds the raw text content. Set for paste attachments whose
	// backing temp file is cleaned up before the message reaches the app layer.
	// Empty for file-reference attachments that are read from disk.
	Content string
}

// Session lifecycle messages control session state and persistence.
type (
	// NewSessionMsg requests creation of a new session.
	NewSessionMsg struct{}

	// ClearSessionMsg resets the current tab and starts a new session
	// in the same working directory.
	ClearSessionMsg struct{}

	// ExitSessionMsg requests exiting the current session.
	ExitSessionMsg struct{}

	// ExitAfterFirstResponseMsg exits TUI after first assistant response completes.
	ExitAfterFirstResponseMsg struct{}

	// EvalSessionMsg saves evaluation data to the specified file.
	EvalSessionMsg struct{ Filename string }

	// CompactSessionMsg generates a summary and compacts session history.
	CompactSessionMsg struct{ AdditionalPrompt string }

	// CopySessionToClipboardMsg copies the entire conversation to clipboard.
	CopySessionToClipboardMsg struct{}

	// CopyLastResponseToClipboardMsg copies the last assistant response to clipboard.
	CopyLastResponseToClipboardMsg struct{}

	// UndoSnapshotMsg restores files from the latest snapshot.
	UndoSnapshotMsg struct{}

	// ShowSnapshotsDialogMsg requests opening the snapshots dialog.
	ShowSnapshotsDialogMsg struct{}

	// ResetSnapshotMsg requests restoring the workspace to a snapshot.
	// Keep is the number of snapshots to retain in chronological order:
	// 0 reverts every snapshot (back to the original pre-agent state),
	// N keeps snapshots 1..N and reverts any later ones.
	ResetSnapshotMsg struct{ Keep int }

	// ExportSessionMsg exports the session to the specified file.
	ExportSessionMsg struct{ Filename string }

	// OpenSessionBrowserMsg opens the session browser dialog.
	OpenSessionBrowserMsg struct{}

	// LoadSessionMsg loads a session by ID.
	LoadSessionMsg struct{ SessionID string }

	// ToggleSessionStarMsg toggles star on a session; empty ID means current session.
	ToggleSessionStarMsg struct{ SessionID string }

	// DeleteSessionMsg deletes a session by ID.
	DeleteSessionMsg struct{ SessionID string }

	// SetSessionTitleMsg sets the session title to specified value.
	SetSessionTitleMsg struct{ Title string }

	// RegenerateTitleMsg regenerates the session title using the AI.
	RegenerateTitleMsg struct{}

	// ForkSessionMsg requests forking the current session into a new tab.
	ForkSessionMsg struct{}

	// StreamCancelledMsg notifies components that the stream has been cancelled.
	StreamCancelledMsg struct{ ShowMessage bool }

	// ClearQueueMsg clears all queued messages.
	ClearQueueMsg struct{}

	// ToggleSplitDiffMsg toggles split diff view mode.
	ToggleSplitDiffMsg struct{}

	// SendMsg contains the content sent to the agent.
	SendMsg struct {
		Content     string       // Full content sent to the agent (with file contents expanded)
		Attachments []Attachment // Attached files or inline content (e.g. pastes)
	}

	// SendAttachmentMsg is a message for the first message with an attachment.
	SendAttachmentMsg struct{ Content *session.Message }
)
