package attachment

// Advisor is implemented by providers that support document attachments.
// The UI layer uses SupportedMIMETypes to populate file-picker filters so that
// only files the active provider can handle are offered to the user.
type Advisor interface {
	// SupportedMIMETypes returns the list of MIME types that this provider
	// can accept as document attachments (e.g. "application/pdf", "image/png").
	SupportedMIMETypes() []string
}
