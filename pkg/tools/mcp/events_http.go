package mcp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpNameHeader   = "Mcp-Name"
	mcpBase64Prefix = "=?base64?"
	mcpBase64Suffix = "?="
)

func encodeMCPHeaderName(s string) string {
	if s == "" {
		return s
	}
	needsEncoding := s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' ||
		(strings.HasPrefix(s, mcpBase64Prefix) && strings.HasSuffix(s, mcpBase64Suffix))
	if !needsEncoding {
		for _, r := range s {
			if r < 0x20 || r > 0x7E {
				needsEncoding = true
				break
			}
		}
	}
	if !needsEncoding {
		return s
	}
	return mcpBase64Prefix + base64.StdEncoding.EncodeToString([]byte(s)) + mcpBase64Suffix
}

// Intercept events/stream POSTs and SSE GET streams, preserving the SDK's
// concrete connection and leaving authentication, redirects, and origin policy
// to the inner client.
type eventHTTPTransport struct {
	base   http.RoundTripper
	events *eventClient
}

func (t *eventHTTPTransport) wrapEventStream(resp *http.Response) *http.Response {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusOK && mediaType == "text/event-stream" {
		resp.Body = &eventHTTPBody{ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), events: t.events}
	}
	return resp
}

func (t *eventHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		return t.wrapEventStream(resp), nil
	}
	if req.Method != http.MethodPost || req.GetBody == nil {
		return t.base.RoundTrip(req)
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(body, 1024*1024+1))
	closeErr := body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > 1024*1024 {
		if req.Header.Get("Mcp-Method") == "events/stream" || bytes.Contains(data, []byte(localSubscriptionMeta)) {
			_ = req.Body.Close()
			return nil, errors.New("MCP events/stream request exceeds 1 MiB")
		}
		return t.base.RoundTrip(req)
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Method != "events/stream" {
		return t.base.RoundTrip(req)
	}
	defer req.Body.Close()
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, err
	}
	msg, eventName, err := t.events.prepareRequest(msg)
	if err != nil {
		return nil, err
	}
	data, err = jsonrpc.EncodeMessage(msg)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	if eventName != "" {
		clone.Header.Set(mcpNameHeader, encodeMCPHeaderName(eventName))
	}
	clone.Body = io.NopCloser(bytes.NewReader(data))
	clone.ContentLength = int64(len(data))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	return t.wrapEventStream(resp), nil
}

// Filtering is pull-driven, so Close directly closes the network body and no
// pipe writer or background reader can outlive an aborted HTTP request.
type eventHTTPBody struct {
	io.ReadCloser

	reader        *bufio.Reader
	events        *eventClient
	pending       []byte
	skipLF        bool
	seenFirstLine bool
}

const maxSSEFrameBytes = gomcp.DefaultMaxEventSize

// readSSELine reads one SSE line terminated by \n or \r without peeking past
// \r (which would block on a live CR-only stream). A trailing \n belonging to
// a CRLF pair is skipped via skipLF on the next read.
func (b *eventHTTPBody) readSSELine(remaining int) (text string, err error) {
	var line []byte
	for {
		ch, readErr := b.reader.ReadByte()
		if readErr != nil {
			return string(line), readErr
		}
		if b.skipLF {
			b.skipLF = false
			if ch == '\n' {
				continue
			}
		}
		if ch == '\n' {
			return string(line), nil
		}
		if ch == '\r' {
			if b.reader.Buffered() > 0 {
				if next, peekErr := b.reader.Peek(1); peekErr == nil && len(next) == 1 && next[0] == '\n' {
					_, _ = b.reader.ReadByte()
					return string(line), nil
				}
			}
			b.skipLF = true
			return string(line), nil
		}
		line = append(line, ch)
		if len(line)+1 > remaining {
			return "", errors.New("MCP event SSE frame exceeds maximum size")
		}
	}
}

func (b *eventHTTPBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		var frame bytes.Buffer
		var metaLines bytes.Buffer
		var data []string
		eventName := ""
		for {
			text, err := b.readSSELine(maxSSEFrameBytes - frame.Len())
			if !b.seenFirstLine {
				b.seenFirstLine = true
				text = strings.TrimPrefix(text, "\uFEFF")
			}
			if err != nil {
				if errors.Is(err, io.EOF) && (text != "" || frame.Len() > 0) {
					frame.WriteString(text)
					b.pending = frame.Bytes()
					break
				}
				return 0, err
			}
			// Normalize all completed lines to LF so the SDK's LF-oriented SSE
			// scanner handles CR-only streams and fragmented CRLF separators.
			frame.WriteString(text)
			frame.WriteByte('\n')
			if text == "" {
				payload := strings.Join(data, "\n")
				msg, decodeErr := jsonrpc.DecodeMessage([]byte(payload))
				req, ok := msg.(*jsonrpc.Request)
				if decodeErr == nil && ok && !req.IsCall() && (eventName == "" || eventName == "message") && isEventNotification(req.Method) {
					b.events.receive(req)
					if metaLines.Len() > 0 {
						metaLines.WriteByte('\n')
						b.pending = metaLines.Bytes()
					}
				} else {
					b.pending = frame.Bytes()
				}
				break
			}
			field, value, _ := strings.Cut(text, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
			case "event":
				eventName = value
			case "id", "retry":
				metaLines.WriteString(text)
				metaLines.WriteByte('\n')
			}
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (c *sessionClient) enableHTTPEvents(client *gomcp.Client, httpClient *http.Client) error {
	events, err := newEventClient(client)
	if err != nil {
		return err
	}
	events.sanitizeError = c.safeError
	events.nextID = &c.nextEventID
	c.mu.Lock()
	c.events = events
	c.mu.Unlock()
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = &eventHTTPTransport{base: base, events: events}
	return nil
}

// The SDK owns HTTP connections, so their teardown cannot use eventConnection.Close.
func (c *sessionClient) closeEvents() {
	c.mu.RLock()
	events := c.events
	c.mu.RUnlock()
	if events != nil {
		events.close()
	}
}

func (c *sessionClient) watchEventConnection() {
	session := c.getSession()
	c.mu.RLock()
	events := c.events
	c.mu.RUnlock()
	go func() { _ = session.Wait(); events.close() }()
}
