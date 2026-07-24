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
	return modelinfo.CapsWith(true, true, false, false)
}

func textOnlyCaps() modelinfo.ModelCapabilities {
	return modelinfo.CapsWith(false, false, false, false)
}

func imageNoPDFCaps() modelinfo.ModelCapabilities {
	return modelinfo.CapsWith(true, false, false, false)
}

func TestDecide(t *testing.T) {
	t.Parallel()
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
