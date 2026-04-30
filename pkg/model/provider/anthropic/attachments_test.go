package anthropic

// attachments_test.go — unit tests for the Anthropic attachment wiring.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

func buildTestClient() *Client {
	return &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-3-5-sonnet-20241022",
			},
		},
	}
}

func TestAnthropic_SupportedMIMETypes(t *testing.T) {
	t.Parallel()

	c := buildTestClient()
	mimes := c.SupportedMIMETypes()
	if len(mimes) == 0 {
		t.Fatal("SupportedMIMETypes should return at least one MIME type")
	}
	mimeSet := make(map[string]bool, len(mimes))
	for _, m := range mimes {
		mimeSet[m] = true
	}
	for _, want := range []string{"image/png", "image/jpeg", "application/pdf", "text/plain", "text/markdown"} {
		if !mimeSet[want] {
			t.Errorf("expected MIME %q in SupportedMIMETypes, got %v", want, mimes)
		}
	}
}

func TestAnthropic_ConvertDocument_URL(t *testing.T) {
	t.Parallel()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "photo.png",
		MimeType: "image/png",
		Source:   chat.DocumentSource{URL: "https://example.com/photo.png"},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].OfImage == nil {
		t.Fatal("expected image block")
	}
	if blocks[0].OfImage.Source.OfURL == nil {
		t.Fatal("expected URL source")
	}
}

func TestAnthropic_ConvertDocument_B64_Image(t *testing.T) {
	t.Parallel()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "photo.png",
		MimeType: "image/png",
		Source:   chat.DocumentSource{InlineData: []byte("\x89PNG\r\n")},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].OfImage == nil {
		t.Fatal("expected image block")
	}
	if blocks[0].OfImage.Source.OfBase64 == nil {
		t.Fatal("expected base64 source")
	}
}

func TestAnthropic_ConvertDocument_B64_PDF(t *testing.T) {
	t.Parallel()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "doc.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{InlineData: []byte("%PDF-1.4")},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].OfDocument == nil {
		t.Fatal("expected document block for PDF")
	}
}

func TestAnthropic_ConvertDocument_TXT(t *testing.T) {
	t.Parallel()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "readme.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "# Title"},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].OfText == nil {
		t.Fatal("expected text block for TXT strategy")
	}
	if !strings.Contains(blocks[0].OfText.Text, "# Title") {
		t.Errorf("expected body in text block, got: %s", blocks[0].OfText.Text)
	}
}

func TestAnthropic_ConvertDocument_FetchAsB64(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 test"))
	}))
	defer srv.Close()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "report.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{URL: srv.URL},
	}
	// application/pdf has B64 but no URL in Anthropic table → FetchAsB64
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].OfDocument == nil {
		t.Fatal("expected document block after FetchAsB64")
	}
}

func TestAnthropic_ConvertDocument_FetchAsTXT(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain text content"))
	}))
	defer srv.Close()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "notes.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{URL: srv.URL},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].OfText == nil {
		t.Fatal("expected text block after FetchAsTXT")
	}
	if !strings.Contains(blocks[0].OfText.Text, "plain text content") {
		t.Errorf("expected fetched body in envelope, got: %s", blocks[0].OfText.Text)
	}
}

func TestAnthropic_ConvertDocument_Drop(t *testing.T) {
	t.Parallel()

	c := buildTestClient()
	doc := chat.Document{
		Name:     "clip.mp4",
		MimeType: "video/mp4",
		Source:   chat.DocumentSource{InlineData: []byte("data")},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument returned error on drop: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks on drop, got %d", len(blocks))
	}
}
