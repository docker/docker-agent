// Package anthropictest serves scripted Anthropic Messages API streams for
// runtime tests that drive a real Anthropic client. Only tests import it.
package anthropictest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Request is one captured request.
type Request struct {
	Path  string
	Betas []string
	Body  map[string]any
	Raw   string
}

// Server answers each request with the next scripted SSE reply; the last
// reply repeats.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []Request
	replies  []string
}

func NewServer(t *testing.T, replies ...string) *Server {
	t.Helper()
	s := &Server{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := Request{Path: r.URL.Path, Raw: string(raw)}
		for _, h := range r.Header.Values("anthropic-beta") {
			for b := range strings.SplitSeq(h, ",") {
				req.Betas = append(req.Betas, strings.TrimSpace(b))
			}
		}
		_ = json.Unmarshal(raw, &req.Body)
		s.mu.Lock()
		s.requests = append(s.requests, req)
		reply := ""
		if n := len(s.replies); n > 0 {
			reply = s.replies[min(len(s.requests), n)-1]
		}
		s.mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) URL() string { return s.srv.URL }

// Requests returns a copy of the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// TextReply is a plain text answer reporting inputTokens.
func TextReply(id, text string, inputTokens int64) string {
	return body(
		messageStart(id, "claude-test", inputTokens),
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}),
		event("content_block_stop", map[string]any{"index": 0}),
		messageDelta("end_turn", map[string]any{"output_tokens": 7}),
		event("message_stop", nil),
	)
}

// ThinkingTextReply is signed thinking followed by a text answer.
func ThinkingTextReply(id, thinking, signature, text string, inputTokens int64) string {
	return body(
		messageStart(id, "claude-test", inputTokens),
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": thinking}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "signature_delta", "signature": signature}}),
		event("content_block_stop", map[string]any{"index": 0}),
		event("content_block_start", map[string]any{"index": 1, "content_block": map[string]any{"type": "text", "text": ""}}),
		event("content_block_delta", map[string]any{"index": 1, "delta": map[string]any{"type": "text_delta", "text": text}}),
		event("content_block_stop", map[string]any{"index": 1}),
		messageDelta("end_turn", map[string]any{"output_tokens": 7}),
		event("message_stop", nil),
	)
}

// CompactionReply is a signed native compaction block. Each iteration is an
// {input, output} token pair billed on its own.
func CompactionReply(id, summary, signature string, iterations ...[2]int64) string {
	usage := make([]any, 0, len(iterations))
	for _, it := range iterations {
		usage = append(usage, map[string]any{"type": "compaction", "input_tokens": it[0], "output_tokens": it[1]})
	}
	return body(
		messageStart(id, "claude-test", 0),
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "compaction", "content": nil, "encrypted_content": "", "signature": nil}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "compaction_delta", "content": summary, "encrypted_content": "enc-" + id}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "signature_delta", "signature": signature}}),
		event("content_block_stop", map[string]any{"index": 0}),
		messageDelta("compaction", map[string]any{"input_tokens": 0, "output_tokens": 0, "iterations": usage}),
		event("message_stop", nil),
	)
}

// FallbackBeforeOutputReply is a turn that from declined before any output,
// in the refusal category: message_start already names to, the fallback
// marker comes first, and the declined attempt reports no output tokens.
func FallbackBeforeOutputReply(id, from, to, category, text string) string {
	events := append([]sse{messageStart(id, to, 0)}, fallbackBlock(0, from, to, category)...)
	events = append(events, textBlock(1, text)...)
	declined := iteration("message", from)
	declined["output_tokens"] = 0
	events = append(events, messageDelta("end_turn", map[string]any{
		"input_tokens": 0, "output_tokens": 7,
		"iterations": []any{declined, iteration("fallback_message", to)},
	}), event("message_stop", nil))
	return body(events...)
}

// FallbackMidOutputReply is a turn that from declined mid-output, after its
// signed thinking, the text before and a complete client tool call toolID;
// to continues with the text after.
func FallbackMidOutputReply(id, from, to, before, after, toolID string) string {
	events := []sse{
		messageStart(id, from, 0),
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "declined plan"}}),
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "sig-declined"}}),
		event("content_block_stop", map[string]any{"index": 0}),
	}
	events = append(events, textBlock(1, before)...)
	events = append(events,
		event("content_block_start", map[string]any{"index": 2, "content_block": map[string]any{"type": "tool_use", "id": toolID, "name": "record", "input": map[string]any{}}}),
		event("content_block_delta", map[string]any{"index": 2, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}}),
		event("content_block_stop", map[string]any{"index": 2}),
	)
	events = append(events, fallbackBlock(3, from, to, "cyber")...)
	events = append(events, textBlock(4, after)...)
	events = append(events, messageDelta("end_turn", fallbackUsage(from, to)), event("message_stop", nil))
	return body(events...)
}

// FallbackChainReply is a turn handed down a chain of models: each one
// writes a short text, then declines to the next, which ends the turn.
func FallbackChainReply(id string, models ...string) string {
	events := []sse{messageStart(id, models[0], 0)}
	var iterations []any
	index := 0
	for i, model := range models {
		events = append(events, textBlock(index, "from "+model)...)
		index++
		if i == len(models)-1 {
			iterations = append(iterations, iteration("fallback_message", model))
			break
		}
		iterations = append(iterations, iteration("message", model))
		events = append(events, fallbackBlock(index, model, models[i+1], "cyber")...)
		index++
	}
	events = append(events, messageDelta("end_turn", map[string]any{"input_tokens": 0, "output_tokens": 7, "iterations": iterations}), event("message_stop", nil))
	return body(events...)
}

// StickyReply is a turn sticky routing sent straight to the fallback model:
// no marker, only the model and a fallback_message iteration show it.
func StickyReply(id, to, text string) string {
	events := []sse{messageStart(id, to, 0)}
	events = append(events, textBlock(0, text)...)
	events = append(events, messageDelta("end_turn", map[string]any{
		"input_tokens": 0, "output_tokens": 7,
		"iterations": []any{iteration("fallback_message", to)},
	}), event("message_stop", nil))
	return body(events...)
}

// fallbackBlock is a fallback marker: a block start and stop with no deltas.
func fallbackBlock(index int, from, to, category string) []sse {
	return []sse{
		event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{
			"type": "fallback", "from": map[string]any{"model": from}, "to": map[string]any{"model": to},
			"trigger": map[string]any{"type": "refusal", "category": refusalCategory(category)},
		}}),
		event("content_block_stop", map[string]any{"index": index}),
	}
}

func textBlock(index int, text string) []sse {
	return []sse{
		event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "text", "text": ""}}),
		event("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "text_delta", "text": text}}),
		event("content_block_stop", map[string]any{"index": index}),
	}
}

func fallbackUsage(from, to string) map[string]any {
	return map[string]any{
		"input_tokens": 0, "output_tokens": 7,
		"iterations": []any{iteration("message", from), iteration("fallback_message", to)},
	}
}

func iteration(typ, model string) map[string]any {
	return map[string]any{"type": typ, "model": model, "input_tokens": 10, "output_tokens": 7, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4}
}

type sse struct {
	name string
	data map[string]any
}

func event(name string, data map[string]any) sse {
	if data == nil {
		data = map[string]any{}
	}
	data["type"] = name
	return sse{name, data}
}

func messageStart(id, model string, inputTokens int64) sse {
	return event("message_start", map[string]any{"message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
	}})
}

func messageDelta(stopReason string, usage map[string]any) sse {
	return event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stopReason}, "usage": usage})
}

func body(events ...sse) string {
	var sb strings.Builder
	for _, e := range events {
		data, _ := json.Marshal(e.data)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", e.name, data)
	}
	return sb.String()
}

// The empty fixture category represents an explicit JSON null, not an omitted field.
func refusalCategory(category string) any {
	if category == "" {
		return nil
	}
	return category
}
