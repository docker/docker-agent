package attachment_test

import (
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestDecide(t *testing.T) {
	// A table that exercises every capability combination used in the tests.
	table := attachment.CapabilityTable{
		"text/plain":      {TXT: true, B64: false, URL: false},
		"image/png":       {TXT: false, B64: true, URL: true},
		"application/pdf": {TXT: false, B64: true, URL: false},
		"text/markdown":   {TXT: true, B64: false, URL: true},
		"video/mp4":       {TXT: false, B64: false, URL: false}, // nothing supported
	}

	tests := []struct {
		name           string
		doc            chat.Document
		wantStrategy   attachment.Strategy
		wantReasonSubs string // non-empty substring that must appear in the reason
	}{
		// ── 1. MIME miss ────────────────────────────────────────────────────────
		{
			name: "mime not in table → Drop",
			doc: chat.Document{
				MimeType: "application/zip",
				Source:   chat.DocumentSource{InlineData: []byte("data")},
			},
			wantStrategy:   attachment.StrategyDrop,
			wantReasonSubs: "mime not in provider table",
		},

		// ── 2. URL + provider supports URL → URL ─────────────────────────────
		{
			name: "url source + URL cap → URL",
			doc: chat.Document{
				MimeType: "image/png",
				Source:   chat.DocumentSource{URL: "https://example.com/img.png"},
			},
			wantStrategy: attachment.StrategyURL,
		},

		// ── 3a. URL + no URL cap + B64 cap → FetchAsB64 ──────────────────────
		// Non-empty reason is intentional: signals an automatic fallback for logging/UI.
		{
			name: "url source + no URL cap + B64 → FetchAsB64",
			doc: chat.Document{
				MimeType: "application/pdf",
				Source:   chat.DocumentSource{URL: "https://example.com/doc.pdf"},
			},
			wantStrategy:   attachment.StrategyFetchAsB64,
			wantReasonSubs: "url not supported",
		},

		// ── 3b. URL + no URL cap + no B64 + TXT cap → FetchAsTXT ─────────────
		// Non-empty reason is intentional: signals an automatic fallback for logging/UI.
		{
			name: "url source + no URL cap + TXT → FetchAsTXT",
			doc: chat.Document{
				MimeType: "text/plain",
				Source:   chat.DocumentSource{URL: "https://example.com/readme.txt"},
			},
			wantStrategy:   attachment.StrategyFetchAsTXT,
			wantReasonSubs: "url not supported",
		},

		// ── 3c. URL + no URL cap + no B64 + no TXT → Drop ────────────────────
		{
			name: "url source + nothing supported → Drop",
			doc: chat.Document{
				MimeType: "video/mp4",
				Source:   chat.DocumentSource{URL: "https://example.com/clip.mp4"},
			},
			wantStrategy:   attachment.StrategyDrop,
			wantReasonSubs: "provider cannot handle",
		},

		// ── 4. InlineData (non-nil) + B64 cap → B64 ──────────────────────────
		{
			name: "inline binary + B64 cap → B64",
			doc: chat.Document{
				MimeType: "image/png",
				Source:   chat.DocumentSource{InlineData: []byte("\x89PNG\r\n\x1a\n")},
			},
			wantStrategy: attachment.StrategyB64,
		},

		// ── 4b. InlineData non-nil but zero-length → B64 (spec: nil check, not len) ──
		{
			name: "inline binary (empty slice, non-nil) + B64 cap → B64",
			doc: chat.Document{
				MimeType: "image/png",
				Source:   chat.DocumentSource{InlineData: []byte{}},
			},
			wantStrategy: attachment.StrategyB64,
		},

		// ── 5. InlineText + TXT cap → TXT ────────────────────────────────────
		{
			name: "inline text + TXT cap → TXT",
			doc: chat.Document{
				MimeType: "text/plain",
				Source:   chat.DocumentSource{InlineText: "hello world"},
			},
			wantStrategy: attachment.StrategyTXT,
		},

		// ── 6. InlineData present but only TXT supported → Drop ──────────────
		{
			name: "inline binary but only TXT cap → Drop",
			doc: chat.Document{
				MimeType: "text/plain",
				// validates step-6 fall-through: B64 variant present but provider only supports TXT
				Source: chat.DocumentSource{InlineData: []byte("binary-data")},
			},
			wantStrategy:   attachment.StrategyDrop,
			wantReasonSubs: "no supported variant",
		},

		// ── 7. InlineText present but only B64/URL supported → Drop ──────────
		{
			name: "inline text but only B64/URL cap → Drop",
			doc: chat.Document{
				MimeType: "image/png",
				Source:   chat.DocumentSource{InlineText: "not really an image"},
			},
			wantStrategy:   attachment.StrategyDrop,
			wantReasonSubs: "no supported variant",
		},

		// ── 8. No source at all → Drop ────────────────────────────────────────
		{
			name: "empty source → Drop",
			doc: chat.Document{
				MimeType: "text/plain",
				Source:   chat.DocumentSource{},
			},
			wantStrategy:   attachment.StrategyDrop,
			wantReasonSubs: "no supported variant",
		},

		// ── 9. URL mime that also supports URL ────────────────────────────────
		{
			name: "url source + URL cap (text/markdown) → URL",
			doc: chat.Document{
				MimeType: "text/markdown",
				Source:   chat.DocumentSource{URL: "https://example.com/readme.md"},
			},
			wantStrategy: attachment.StrategyURL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := attachment.Decide(tc.doc, table)
			if got != tc.wantStrategy {
				t.Errorf("Decide() strategy = %v, want %v (reason: %q)", got, tc.wantStrategy, reason)
			}
			if tc.wantReasonSubs != "" && !strings.Contains(reason, tc.wantReasonSubs) {
				t.Errorf("Decide() reason = %q, want substring %q", reason, tc.wantReasonSubs)
			}
			if tc.wantReasonSubs == "" && reason != "" {
				// For happy-path strategies (URL, B64, TXT) the reason should be empty.
				t.Errorf("Decide() reason = %q, want empty", reason)
			}
		})
	}
}

func TestTXTEnvelope(t *testing.T) {
	got := attachment.TXTEnvelope("readme.md", "text/markdown", "# Hello\nworld")
	if !strings.Contains(got, `name="readme.md"`) {
		t.Errorf("TXTEnvelope missing name attribute: %s", got)
	}
	if !strings.Contains(got, `mime-type="text/markdown"`) {
		t.Errorf("TXTEnvelope missing mime-type attribute: %s", got)
	}
	if !strings.Contains(got, "# Hello\nworld") {
		t.Errorf("TXTEnvelope missing body: %s", got)
	}
	if !strings.HasPrefix(got, "<document ") {
		t.Errorf("TXTEnvelope should start with <document: %s", got)
	}
	if !strings.HasSuffix(got, "</document>") {
		t.Errorf("TXTEnvelope should end with </document>: %s", got)
	}
}
