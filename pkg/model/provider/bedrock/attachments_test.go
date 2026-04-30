package bedrock

// attachments_test.go — unit tests for the Bedrock attachment wiring.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

func buildTestBedrockClient() *Client {
	return &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "amazon-bedrock",
				Model:    "anthropic.claude-3-5-sonnet-20241022-v2:0",
			},
		},
	}
}

func TestBedrock_SupportedMIMETypes(t *testing.T) {
	t.Parallel()

	c := buildTestBedrockClient()
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

func TestBedrock_ConvertDocument_B64_Image(t *testing.T) {
	t.Parallel()

	c := buildTestBedrockClient()
	doc := chat.Document{
		Name:     "photo.png",
		MimeType: "image/png",
		Source:   chat.DocumentSource{InlineData: []byte("\x89PNG\r\n\x1a\n")},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	imgBlock, ok := blocks[0].(*types.ContentBlockMemberImage)
	if !ok {
		t.Fatalf("expected ContentBlockMemberImage, got %T", blocks[0])
	}
	if imgBlock.Value.Format != types.ImageFormatPng {
		t.Errorf("expected PNG format, got %v", imgBlock.Value.Format)
	}
}

func TestBedrock_ConvertDocument_B64_PDF(t *testing.T) {
	t.Parallel()

	c := buildTestBedrockClient()
	doc := chat.Document{
		Name:     "report.pdf",
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
	docBlock, ok := blocks[0].(*types.ContentBlockMemberDocument)
	if !ok {
		t.Fatalf("expected ContentBlockMemberDocument, got %T", blocks[0])
	}
	if docBlock.Value.Format != types.DocumentFormatPdf {
		t.Errorf("expected PDF format, got %v", docBlock.Value.Format)
	}
}

func TestBedrock_ConvertDocument_TXT(t *testing.T) {
	t.Parallel()

	c := buildTestBedrockClient()
	doc := chat.Document{
		Name:     "notes.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{InlineText: "hello bedrock"},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected ContentBlockMemberText, got %T", blocks[0])
	}
	if !strings.Contains(textBlock.Value, "hello bedrock") {
		t.Errorf("expected body in text block, got: %s", textBlock.Value)
	}
}

func TestBedrock_ConvertDocument_FetchAsB64(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4"))
	}))
	defer srv.Close()

	c := buildTestBedrockClient()
	doc := chat.Document{
		Name:     "doc.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{URL: srv.URL},
	}
	blocks, err := c.convertDocument(t.Context(), doc)
	if err != nil {
		t.Fatalf("convertDocument: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 block after FetchAsB64, got %d", len(blocks))
	}
	if _, ok := blocks[0].(*types.ContentBlockMemberDocument); !ok {
		t.Fatalf("expected ContentBlockMemberDocument after FetchAsB64, got %T", blocks[0])
	}
}

func TestBedrock_ConvertDocument_FetchAsTXT(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fetched text"))
	}))
	defer srv.Close()

	c := buildTestBedrockClient()
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
		t.Fatalf("want 1 block after FetchAsTXT, got %d", len(blocks))
	}
	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected ContentBlockMemberText, got %T", blocks[0])
	}
	if !strings.Contains(textBlock.Value, "fetched text") {
		t.Errorf("expected fetched content in envelope, got: %s", textBlock.Value)
	}
}

func TestBedrock_ConvertDocument_Drop(t *testing.T) {
	t.Parallel()

	c := buildTestBedrockClient()
	doc := chat.Document{
		Name:     "archive.zip",
		MimeType: "application/zip",
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

func TestBedrock_SanitizeDocName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"report.pdf", "report"},
		{"my report.pdf", "my report"},
		{"bad@chars!.pdf", "bad-chars-"},
		{"  .pdf", "document"},
		{"my  doc.pdf", "my doc"}, // double space collapsed
	}
	for _, tc := range tests {
		got := sanitizeDocName(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeDocName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
