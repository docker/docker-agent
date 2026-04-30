package chat

// DocumentSource holds the actual content of a document attachment.
// Exactly one of InlineText, InlineData, or URL should be set.
type DocumentSource struct {
	InlineText string `json:"inline_text,omitempty"` // plain text (TXT/MD/HTML/CSV)
	InlineData []byte `json:"inline_data,omitempty"` // binary bytes (images, PDFs, etc.)
	URL        string `json:"url,omitempty"`         // public HTTPS URL
}

// Document represents a file or document attachment that can be included in a
// message part (set Type = MessagePartTypeDocument when populating this field).
// Providers use the attachment package to decide how to handle the document
// based on their capability tables.
type Document struct {
	Name     string         `json:"name"`
	MimeType string         `json:"mime_type"`
	Size     int64          `json:"size,omitempty"`
	Source   DocumentSource `json:"source"`
}
