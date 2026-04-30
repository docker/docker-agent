package openai

// attachments_test.go — unit tests for the OpenAI attachment wiring.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// buildTestClient creates a minimal *Client without making any API calls.
func buildTestClient(provider string) *Client {
	cfg := latest.ModelConfig{
		Provider: provider,
		Model:    "gpt-4o",
	}
	return &Client{
		Config: base.Config{
			ModelConfig: cfg,
		},
	}
}

func TestOpenAI_SupportedMIMETypes_ByProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		mustHave []string
		mustMiss []string
	}{
		{
			provider: "openai",
			mustHave: []string{"image/png", "image/jpeg", "text/plain"},
			mustMiss: []string{"application/pdf"}, // PDF deferred to Phase 2 (Files API)
		},
		{
			provider: "azure",
			mustHave: []string{"image/png", "text/markdown"},
			mustMiss: []string{"application/pdf"}, // PDF deferred to Phase 2 (Files API)
		},
		{
			provider: "mistral",
			mustHave: []string{"image/jpeg", "text/plain"},
			// application/pdf removed: Mistral requires document_url (Phase 2)
			// image/gif not in Mistral table
			mustMiss: []string{"image/gif", "application/pdf"},
		},
		{
			provider: "xai",
			mustHave: []string{"image/png", "text/plain"},
			mustMiss: []string{"application/pdf"}, // xAI has no PDF
		},
		{
			provider: "ollama",
			mustHave: []string{"image/jpeg", "image/png", "text/plain"},
			mustMiss: []string{"application/pdf", "image/gif"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			c := buildTestClient(tc.provider)
			mimes := c.SupportedMIMETypes()
			mimeSet := make(map[string]bool, len(mimes))
			for _, m := range mimes {
				mimeSet[m] = true
			}
			for _, want := range tc.mustHave {
				if !mimeSet[want] {
					t.Errorf("provider %q: expected MIME %q in SupportedMIMETypes, got %v", tc.provider, want, mimes)
				}
			}
			for _, notWant := range tc.mustMiss {
				if mimeSet[notWant] {
					t.Errorf("provider %q: unexpected MIME %q in SupportedMIMETypes", tc.provider, notWant)
				}
			}
		})
	}
}

func TestOpenAI_ConvertDocument_URL(t *testing.T) {
	t.Parallel()

	c := buildTestClient("openai")
	doc := chat.Document{
		Name:     "photo.png",
		MimeType: "image/png",
		Source:   chat.DocumentSource{URL: "https://example.com/photo.png"},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].OfImageURL == nil {
		t.Fatal("expected OfImageURL part")
	}
	if parts[0].OfImageURL.ImageURL.URL != "https://example.com/photo.png" {
		t.Errorf("unexpected URL: %s", parts[0].OfImageURL.ImageURL.URL)
	}
}

func TestOpenAI_ConvertDocument_B64(t *testing.T) {
	t.Parallel()

	c := buildTestClient("openai")
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
	if parts[0].OfImageURL == nil {
		t.Fatal("expected OfImageURL part (base64 data URL)")
	}
	if !strings.HasPrefix(parts[0].OfImageURL.ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("expected base64 data URL, got: %s", parts[0].OfImageURL.ImageURL.URL)
	}
}

func TestOpenAI_ConvertDocument_TXT(t *testing.T) {
	t.Parallel()

	c := buildTestClient("openai")
	doc := chat.Document{
		Name:     "readme.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{InlineText: "hello world"},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].OfText == nil {
		t.Fatal("expected OfText part")
	}
	if !strings.Contains(parts[0].OfText.Text, "hello world") {
		t.Errorf("expected text to contain body, got: %s", parts[0].OfText.Text)
	}
	if !strings.Contains(parts[0].OfText.Text, "mime-type=") {
		t.Errorf("expected TXTEnvelope mime-type attribute, got: %s", parts[0].OfText.Text)
	}
}

// TestOpenAI_ConvertDocument_FetchAsB64 verifies that a URL source document
// for a MIME type that has B64-only capability (no URL) triggers StrategyFetchAsB64:
// the URL is fetched and the bytes are returned as a base64 data-URL part.
//
// We use the "ollama" provider profile because its table has image/jpeg:{B64:true}
// with no URL capability, so a URL source → FetchAsB64.
func TestOpenAI_ConvertDocument_FetchAsB64(t *testing.T) {
	t.Parallel()

	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0} // JPEG magic
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBytes)
	}))
	defer srv.Close()

	// ollama: image/jpeg has B64 only (no URL) → URL source triggers FetchAsB64.
	c := buildTestClient("ollama")
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{URL: srv.URL},
	}
	parts, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].OfImageURL == nil {
		t.Fatal("expected OfImageURL part (fetched as base64 data URL)")
	}
	if !strings.HasPrefix(parts[0].OfImageURL.ImageURL.URL, "data:image/jpeg;base64,") {
		t.Errorf("expected base64 data URL, got: %s", parts[0].OfImageURL.ImageURL.URL)
	}
}

func TestOpenAI_ConvertDocument_FetchAsTXT(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# readme"))
	}))
	defer srv.Close()

	// text/plain has TXT but no URL → FetchAsTXT
	c := buildTestClient("openai")
	doc := chat.Document{
		Name:     "readme.md",
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
	if parts[0].OfText == nil {
		t.Fatal("expected OfText part")
	}
	if !strings.Contains(parts[0].OfText.Text, "# readme") {
		t.Errorf("expected fetched content in envelope, got: %s", parts[0].OfText.Text)
	}
}

func TestOpenAI_ConvertDocument_Drop(t *testing.T) {
	t.Parallel()

	// video/mp4 not in any openai table → Drop
	c := buildTestClient("openai")
	doc := chat.Document{
		Name:     "clip.mp4",
		MimeType: "video/mp4",
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
