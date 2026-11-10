package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Intercept only events/stream POSTs, preserving the SDK's concrete connection
// and leaving authentication, redirects, and origin policy to the inner client.
type eventHTTPTransport struct {
	base   http.RoundTripper
	events *eventClient
}

func (t *eventHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
	var envelope struct {
		Method string `json:"method"`
	}
	if len(data) > 1024*1024 || json.Unmarshal(data, &envelope) != nil || envelope.Method != "events/stream" {
		return t.base.RoundTrip(req)
	}
	defer req.Body.Close()
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, err
	}
	msg, err = t.events.prepareRequest(msg)
	if err != nil {
		return nil, err
	}
	data, err = jsonrpc.EncodeMessage(msg)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(bytes.NewReader(data))
	clone.ContentLength = int64(len(data))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusOK && mediaType == "text/event-stream" {
		resp.Body = &eventHTTPBody{ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), events: t.events}
	}
	return resp, nil
}

// Filtering is pull-driven, so Close directly closes the network body and no
// pipe writer or background reader can outlive an aborted HTTP request.
type eventHTTPBody struct {
	io.ReadCloser

	reader  *bufio.Reader
	events  *eventClient
	pending []byte
}

func (b *eventHTTPBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		var frame bytes.Buffer
		var data []string
		eventName := ""
		for {
			var line []byte
			var err error
			for {
				fragment, readErr := b.reader.ReadSlice('\n')
				if frame.Len()+len(line)+len(fragment) > 1024*1024 {
					return 0, errors.New("MCP event SSE frame exceeds 1 MiB")
				}
				line = append(line, fragment...)
				if !errors.Is(readErr, bufio.ErrBufferFull) {
					err = readErr
					break
				}
			}
			frame.Write(line)
			if err != nil {
				if err == io.EOF && frame.Len() > 0 {
					b.pending = frame.Bytes()
					break
				}
				return 0, err
			}
			text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			if text == "" {
				payload := strings.Join(data, "\n")
				msg, decodeErr := jsonrpc.DecodeMessage([]byte(payload))
				req, ok := msg.(*jsonrpc.Request)
				if decodeErr == nil && ok && !req.IsCall() && (eventName == "" || eventName == "message") && isEventNotification(req.Method) {
					b.events.receive(req)
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
