package gemini

// attachments_test.go — unit tests for the Gemini attachment wiring.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestGemini_SupportedMIMETypes(t *testing.T) {
	t.Parallel()

	c := &Client{}
	mimes := c.SupportedMIMETypes()
	if len(mimes) == 0 {
		t.Fatal("SupportedMIMETypes should return at least one MIME type")
	}
	mimeSet := make(map[string]bool, len(mimes))
	for _, m := range mimes {
		mimeSet[m] = true
	}
	// Gemini-specific: images, video, audio, PDF, text
	for _, want := range []string{
		"image/png", "image/jpeg", "image/heic", "image/heif",
		"video/mp4", "audio/wav", "application/pdf", "text/plain",
	} {
		if !mimeSet[want] {
			t.Errorf("expected MIME %q in SupportedMIMETypes, got %v", want, mimes)
		}
	}
}

func TestGemini_ConvertDocument_B64(t *testing.T) {
	t.Parallel()

	c := &Client{}
	doc := chat.Document{
		Name:     "photo.png",
		MimeType: "image/png",
		Source:   chat.DocumentSource{InlineData: []byte("\x89PNG\r\n\x1a\n")},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].InlineData == nil {
		t.Fatal("expected InlineData part")
	}
	if parts[0].InlineData.MIMEType != "image/png" {
		t.Errorf("unexpected MIME type: %s", parts[0].InlineData.MIMEType)
	}
}

func TestGemini_ConvertDocument_TXT(t *testing.T) {
	t.Parallel()

	c := &Client{}
	doc := chat.Document{
		Name:     "readme.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "# Hello"},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].Text == "" {
		t.Fatal("expected Text part")
	}
	if !strings.Contains(parts[0].Text, "# Hello") {
		t.Errorf("expected body in text part, got: %s", parts[0].Text)
	}
}

func TestGemini_ConvertDocument_FetchAsB64(t *testing.T) {
	t.Parallel()

	pdfBytes := []byte("%PDF-1.4 test content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer srv.Close()

	c := &Client{}
	doc := chat.Document{
		Name:     "doc.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{URL: srv.URL},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].InlineData == nil {
		t.Fatal("expected InlineData part after FetchAsB64")
	}
}

func TestGemini_ConvertDocument_FetchAsTXT(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain text"))
	}))
	defer srv.Close()

	c := &Client{}
	doc := chat.Document{
		Name:     "notes.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{URL: srv.URL},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].Text == "" {
		t.Fatal("expected Text part after FetchAsTXT")
	}
	if !strings.Contains(parts[0].Text, "plain text") {
		t.Errorf("expected fetched content in envelope, got: %s", parts[0].Text)
	}
}

func TestGemini_ConvertDocument_Drop(t *testing.T) {
	t.Parallel()

	c := &Client{}
	doc := chat.Document{
		Name:     "archive.zip",
		MimeType: "application/zip",
		Source:   chat.DocumentSource{InlineData: []byte("data")},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument returned error on drop: %v", err)
	}
	if len(parts) != 0 {
		t.Errorf("expected 0 parts on drop, got %d", len(parts))
	}
}
