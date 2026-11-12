package messages

// Input messages control editor input, attachments, and speech.
type (
	// AttachFileMsg attaches a file directly or opens file picker if empty/directory.
	AttachFileMsg struct{ FilePath string }

	// InsertFileRefMsg inserts @filepath reference into editor.
	InsertFileRefMsg struct{ FilePath string }

	// StartSpeakMsg starts speech-to-text transcription.
	StartSpeakMsg struct{}

	// StopSpeakMsg stops speech-to-text transcription.
	StopSpeakMsg struct{}

	// StartShellMsg starts an interactive shell.
	StartShellMsg struct{}

	// OpenURLMsg opens a URL in the browser.
	OpenURLMsg struct{ URL string }
)
