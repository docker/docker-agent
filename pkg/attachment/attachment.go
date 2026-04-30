// Package attachment provides helpers for deciding how document attachments
// should be delivered to LLM providers and for fetching remote content.
package attachment

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/chat"
)

// Strategy describes how a document attachment should be delivered to a provider.
type Strategy int

const (
	// StrategyDrop means the attachment cannot be handled and should be omitted.
	StrategyDrop Strategy = iota
	// StrategyTXT means the document's inline text should be sent as a text envelope.
	StrategyTXT
	// StrategyB64 means the document's inline binary data should be base64-encoded and sent inline.
	StrategyB64
	// StrategyURL means the document's URL should be passed directly to the provider.
	StrategyURL
	// StrategyFetchAsB64 means the URL should be fetched and its bytes sent as base64.
	StrategyFetchAsB64
	// StrategyFetchAsTXT means the URL should be fetched and its content sent as plain text.
	StrategyFetchAsTXT
)

// Capability describes which delivery modes a provider supports for a given MIME type.
type Capability struct {
	TXT bool // provider accepts plain text for this MIME type
	B64 bool // provider accepts base64-encoded binary for this MIME type
	URL bool // provider accepts a public URL for this MIME type
}

// CapabilityTable maps MIME types to provider capabilities.
// Providers build this table to declare what they can handle.
type CapabilityTable map[string]Capability

// Decide selects the best delivery Strategy for doc given the provider's
// capability table. It returns the chosen strategy and, when the strategy is
// a fallback or a drop, a human-readable reason string.
//
// Decision order (exact — do not deviate):
//  1. MIME type not in table → Drop
//  2. Source is URL + provider supports URL → URL
//  3. Source is URL + provider does NOT support URL:
//     a. provider supports B64 → FetchAsB64
//     b. provider supports TXT → FetchAsTXT
//     c. otherwise → Drop
//  4. Source is InlineData + provider supports B64 → B64
//  5. Source is InlineText + provider supports TXT → TXT
//  6. → Drop
func Decide(doc chat.Document, table CapabilityTable) (Strategy, string) {
	capability, ok := table[doc.MimeType]
	if !ok {
		return StrategyDrop, "mime not in provider table"
	}

	if doc.Source.URL != "" && capability.URL {
		return StrategyURL, ""
	}
	if doc.Source.URL != "" && !capability.URL {
		if capability.B64 {
			return StrategyFetchAsB64, "url not supported, will fetch as b64"
		}
		if capability.TXT {
			return StrategyFetchAsTXT, "url not supported, will fetch as text"
		}
		return StrategyDrop, "provider cannot handle url or inline for this mime"
	}
	if len(doc.Source.InlineData) > 0 && capability.B64 {
		return StrategyB64, ""
	}
	if doc.Source.InlineText != "" && capability.TXT {
		return StrategyTXT, ""
	}
	return StrategyDrop, "no supported variant for this provider"
}

// TXTEnvelope wraps plain-text document content in an XML-like tag for
// inclusion in a chat message. The envelope makes the document's name and
// MIME type visible to the model.
//
// Example output:
//
//	<document name="foo.md" type="text/markdown">
//	...content...
//	</document>
func TXTEnvelope(name, mimeType, body string) string {
	return fmt.Sprintf("<document name=%q type=%q>\n%s\n</document>", name, mimeType, body)
}
