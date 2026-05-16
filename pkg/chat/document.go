package chat

// MessagePartTypeDocument is the part type for a structured document attachment.
// Use this type when attaching files (images, PDFs, text, Office docs, etc.) to
// a message. The Document field must be set when this type is used.
//
// This supersedes MessagePartTypeFile and MessagePartTypeImageURL, which are
// deprecated but remain supported for backward compatibility.
const MessagePartTypeDocument MessagePartType = "document"

// DocumentSource holds the actual content of a document. Exactly one of the
// fields should be set.
type DocumentSource struct {
	// InlineText holds the raw text for text/* MIME types (TXT, MD, HTML, CSV, …).
	// Used for StrategyTXT attachments.
	InlineText string `json:"inline_text,omitempty"`

	// InlineData holds binary content (images, PDFs, Office docs, …) that is
	// base64-encoded when sent to the provider. Used for StrategyB64 attachments.
	InlineData []byte `json:"inline_data,omitempty"`
}

// Document represents a file attachment in a message part. It carries
// the file name, post-processing MIME type, and the actual content via Source.
//
// The MimeType field always reflects the final MIME that the attachment system
// will use when sending to the provider (e.g. "image/jpeg" after image
// normalisation, never the original "image/bmp").
type Document struct {
	// Name is the display name of the document (e.g. "report.pdf").
	Name string `json:"name"`

	// MimeType is the post-processing MIME type of the document. For images
	// this is always "image/jpeg" or "image/png" regardless of the original
	// format. For text files it is the exact MIME (e.g. "text/plain",
	// "text/markdown", "text/html"). For binary documents it is the original
	// MIME (e.g. "application/pdf").
	MimeType string `json:"mime_type"`

	// Size is the byte length of the document content (InlineData or InlineText).
	// Optional; zero means unknown.
	Size int64 `json:"size,omitempty"`

	// Source holds the actual document content.
	Source DocumentSource `json:"source"`
}
