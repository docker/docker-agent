package httpclient

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// sseFilterTransport wraps a base RoundTripper and, when the response is a
// `text/event-stream`, replaces the body with one that strips SSE events
// containing no `data:` lines.
//
// Why this exists: some upstreams (notably OpenRouter) inject comment-only
// keep-alive frames into their streams:
//
//	: OPENROUTER PROCESSING
//	<blank line>
//	data: {"id":"...", ...}
//	<blank line>
//
// A correct SSE consumer ignores comment lines but still treats the blank
// line that follows them as an event boundary. The OpenAI Go SDK's
// `ssestream.Stream` does exactly that, then unconditionally calls
// `json.Unmarshal` on the resulting empty `Data` slice — which fails with
// "unexpected end of JSON input" and tears down the whole completion.
//
// The filter normalises the byte stream so events with no `data:` lines
// (comment-only events, or events bearing only `event:` / `id:` headers)
// never reach the SDK. Well-formed events pass through verbatim, and the
// filter is a no-op on non-SSE responses.
//
// dropKeepaliveEvents additionally drops whole `event: keepalive` frames
// whose data carries no payload (`data: {}` or empty). The Docker AI
// Gateway emits such frames during long generations; the genai SDK's SSE
// parser hard-fails on ANY `event:` line, so they must never reach it. The
// mode is opt-in (see WithSSEKeepaliveFilter) because other providers —
// Anthropic in particular — use `event:` headers as meaningful framing that
// must pass through untouched.
type sseFilterTransport struct {
	base                http.RoundTripper
	dropKeepaliveEvents bool
}

func (t *sseFilterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil {
		return res, err
	}
	// Match the prefix so charset suffixes (e.g. "text/event-stream;
	// charset=utf-8") still trigger filtering.
	if strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream") {
		res.Body = newSSEFilterReader(res.Body, t.dropKeepaliveEvents)
	}
	return res, err
}

// sseFilterReader buffers the lines of a single SSE event and only emits
// them once it has seen the trailing blank line AND the event contained at
// least one `data:` line. A half-built event still pending at EOF is
// dropped silently — without the terminating blank line a downstream parser
// would not have dispatched it anyway.
type sseFilterReader struct {
	src     io.ReadCloser
	scn     *bufio.Scanner
	out     bytes.Buffer // bytes ready to hand back to the caller
	pending bytes.Buffer // accumulated lines for the current event
	hasData bool         // saw at least one `data:` line in `pending`

	dropKeepaliveEvents bool // see sseFilterTransport
	isKeepalive         bool // current event is named `keepalive`
	hasMeaningfulData   bool // saw a `data:` line whose payload isn't empty or `{}`
}

func newSSEFilterReader(src io.ReadCloser, dropKeepaliveEvents bool) *sseFilterReader {
	scn := bufio.NewScanner(src)
	// SSE events can be large (long completion tokens, image URLs, …). Match
	// the buffer size used by openai-go's own SSE decoder so we don't trip
	// `bufio.ErrTooLong` on payloads it would happily accept.
	scn.Buffer(make([]byte, 0, 64*1024), bufio.MaxScanTokenSize<<9)
	return &sseFilterReader{src: src, scn: scn, dropKeepaliveEvents: dropKeepaliveEvents}
}

func (r *sseFilterReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 {
		if !r.scn.Scan() {
			if err := r.scn.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		r.consumeLine(r.scn.Bytes())
	}
	return r.out.Read(p)
}

func (r *sseFilterReader) consumeLine(line []byte) {
	switch {
	case len(line) == 0:
		// Event boundary: emit the buffered event iff it had data and is
		// not a payload-free keepalive frame in keepalive-dropping mode.
		if r.hasData && (!r.isKeepalive || r.hasMeaningfulData) {
			r.out.Write(r.pending.Bytes())
			r.out.WriteByte('\n')
		}
		r.pending.Reset()
		r.hasData = false
		r.isKeepalive = false
		r.hasMeaningfulData = false
	case line[0] == ':':
		// SSE comment — drop entirely.
	default:
		r.pending.Write(line)
		r.pending.WriteByte('\n')
		if value, ok := fieldValue(line, "data"); ok {
			r.hasData = true
			if r.dropKeepaliveEvents {
				if payload := bytes.TrimSpace(value); len(payload) > 0 && !bytes.Equal(payload, []byte("{}")) {
					r.hasMeaningfulData = true
				}
			}
		} else if r.dropKeepaliveEvents {
			if value, ok := fieldValue(line, "event"); ok && string(bytes.TrimSpace(value)) == "keepalive" {
				r.isKeepalive = true
			}
		}
	}
}

// fieldValue returns the value of an SSE line whose field name is `name`,
// with the single optional leading space the SSE grammar allows already
// removed.
func fieldValue(line []byte, name string) ([]byte, bool) {
	value, ok := bytes.CutPrefix(line, []byte(name+":"))
	if !ok {
		return nil, false
	}
	return bytes.TrimPrefix(value, []byte(" ")), true
}

func (r *sseFilterReader) Close() error {
	return r.src.Close()
}
