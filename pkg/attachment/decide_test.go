package attachment_test

import (
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// testCaps is a small helper that builds a ModelCapabilities directly.
func visionCaps() modelinfo.ModelCapabilities {
	return modelinfo.CapsWith(true, true)
}

func textOnlyCaps() modelinfo.ModelCapabilities {
	return modelinfo.CapsWith(false, false)
}

func imageNoPDFCaps() modelinfo.ModelCapabilities {
	return modelinfo.CapsWith(true, false)
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name          string
		doc           chat.Document
		caps          modelinfo.ModelCapabilities
		wantStrategy  attachment.Strategy
		wantReasonHas string // non-empty: reason must contain this substring
	}{
		{
			name: "b64 image supported",
			doc: chat.Document{
				Name:     "photo.jpg",
				MimeType: "image/jpeg",
				Source:   chat.DocumentSource{InlineData: []byte{0xFF, 0xD8}},
			},
			caps:         visionCaps(),
			wantStrategy: attachment.StrategyB64,
		},
		{
			name: "txt text plain",
			doc: chat.Document{
				Name:     "notes.txt",
				MimeType: "text/plain",
				Source:   chat.DocumentSource{InlineText: "hello world"},
			},
			caps:         textOnlyCaps(),
			wantStrategy: attachment.StrategyTXT,
		},
		{
			name: "drop image when model has no vision",
			doc: chat.Document{
				Name:     "photo.jpg",
				MimeType: "image/jpeg",
				Source:   chat.DocumentSource{InlineData: []byte{0xFF, 0xD8}},
			},
			caps:          textOnlyCaps(),
			wantStrategy:  attachment.StrategyDrop,
			wantReasonHas: "does not support MIME type",
		},
		{
			name: "drop pdf when model has no pdf support",
			doc: chat.Document{
				Name:     "doc.pdf",
				MimeType: "application/pdf",
				Source:   chat.DocumentSource{InlineData: []byte{0x25, 0x50, 0x44, 0x46}},
			},
			caps:          imageNoPDFCaps(),
			wantStrategy:  attachment.StrategyDrop,
			wantReasonHas: "does not support MIME type",
		},
		{
			name: "drop no inline content",
			doc: chat.Document{
				Name:     "empty.md",
				MimeType: "text/markdown",
				Source:   chat.DocumentSource{},
			},
			caps:          textOnlyCaps(),
			wantStrategy:  attachment.StrategyDrop,
			wantReasonHas: "no inline content",
		},
		{
			name: "b64 pdf when pdf supported",
			doc: chat.Document{
				Name:     "spec.pdf",
				MimeType: "application/pdf",
				Source:   chat.DocumentSource{InlineData: []byte{0x25, 0x50, 0x44, 0x46}},
			},
			caps:         visionCaps(),
			wantStrategy: attachment.StrategyB64,
		},
		{
			name: "drop office doc (DOCX is binary, not supported without models.dev office modality)",
			doc: chat.Document{
				Name:     "report.docx",
				MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				Source:   chat.DocumentSource{InlineData: []byte{0x50, 0x4B}}, // ZIP magic bytes
			},
			caps:          visionCaps(), // even full caps can't send DOCX — no modality
			wantStrategy:  attachment.StrategyDrop,
			wantReasonHas: "does not support MIME type",
		},
		{
			name: "b64 wins over txt when both inline sources present",
			doc: chat.Document{
				Name:     "data.txt",
				MimeType: "text/plain",
				Source:   chat.DocumentSource{InlineData: []byte("hello"), InlineText: "hello"},
			},
			caps:         textOnlyCaps(),
			wantStrategy: attachment.StrategyB64,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStrategy, gotReason := attachment.Decide(tc.doc, tc.caps)
			if gotStrategy != tc.wantStrategy {
				t.Errorf("strategy: got %d, want %d", gotStrategy, tc.wantStrategy)
			}
			if tc.wantReasonHas != "" {
				if !strings.Contains(gotReason, tc.wantReasonHas) {
					t.Errorf("reason %q does not contain %q", gotReason, tc.wantReasonHas)
				}
			}
		})
	}
}

func TestTXTEnvelope(t *testing.T) {
	got := attachment.TXTEnvelope("readme.md", "text/markdown", "# Hello")
	// Tag must start with "document-" followed by a slug of name+mimeType.
	if !strings.HasPrefix(got, "<document-") {
		t.Errorf("TXTEnvelope: expected tag to start with <document-, got %q", got)
	}
	// Body must be present.
	if !strings.Contains(got, "# Hello") {
		t.Errorf("TXTEnvelope: body not found in %q", got)
	}
	// Must be a valid open/close tag pair.
	if !strings.Contains(got, "</document-") {
		t.Errorf("TXTEnvelope: expected closing tag, got %q", got)
	}
}

func TestTXTEnvelope_UniqueTag(t *testing.T) {
	// The tag should contain slugged name and MIME type, making collisions
	// between different documents practically impossible.
	got1 := attachment.TXTEnvelope("report.md", "text/markdown", "body")
	got2 := attachment.TXTEnvelope("notes.txt", "text/plain", "body")

	if got1 == got2 {
		t.Error("TXTEnvelope produced identical tags for different name+MIME combinations")
	}

	// Each envelope's opening tag should appear verbatim as its closing tag.
	for _, tc := range []struct {
		name, mime, body string
	}{
		{"report.md", "text/markdown", "hello"},
		{"my file.txt", "text/plain", "world"},
		{"data", "text/csv", "a,b,c"},
	} {
		out := attachment.TXTEnvelope(tc.name, tc.mime, tc.body)
		// Extract opening tag.
		closeIdx := strings.Index(out, ">")
		if closeIdx < 0 {
			t.Fatalf("no closing > in envelope: %q", out)
		}
		openTag := out[1:closeIdx] // e.g. "document-report-md-text-markdown"
		closeTag := "</" + openTag + ">"
		if !strings.HasSuffix(strings.TrimSpace(out), closeTag) {
			t.Errorf("envelope missing matching close tag %q in %q", closeTag, out)
		}
		if !strings.Contains(out, tc.body) {
			t.Errorf("body %q not found in envelope %q", tc.body, out)
		}
	}
}
