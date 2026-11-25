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
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpMethodHeader          = "Mcp-Method"
	mcpProtocolVersionHeader = "Mcp-Protocol-Version"
)

func isTaskMethod(method string) bool {
	return method == "tasks/get" || method == "tasks/update" || method == "tasks/cancel"
}

// taskHTTPTransport intercepts Streamable HTTP traffic for SEP-2663 Tasks:
//  1. Sets the SEP-2243 Mcp-Name header to params.taskId on tasks/get,
//     tasks/update, and tasks/cancel requests.
//  2. Inspects JSON and SSE responses (including GET Last-Event-ID resumptions)
//     for CreateTaskResult (resultType: "task") and rewrites the result to
//     carry the private task token in _meta.
type taskHTTPTransport struct {
	base           http.RoundTripper
	tasks          *taskClient
	mu             sync.Mutex
	pendingMethods map[string]string
}

func (t *taskHTTPTransport) trackPendingMethod(id, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingMethods[id] = method
}

func (t *taskHTTPTransport) takePendingMethod(id string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	method, ok := t.pendingMethods[id]
	if ok {
		delete(t.pendingMethods, id)
	}
	return method, ok
}

func (t *taskHTTPTransport) deletePendingMethod(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pendingMethods, id)
}

func (t *taskHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		if req.Header.Get("Last-Event-ID") == "" {
			return t.base.RoundTrip(req)
		}
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "text/event-stream" {
				resp.Body = &taskHTTPBody{
					ReadCloser: resp.Body,
					reader:     bufio.NewReader(resp.Body),
					tasks:      t.tasks,
					takeMethod: t.takePendingMethod,
				}
			}
		}
		return resp, nil
	}
	if req.Method != http.MethodPost {
		return t.base.RoundTrip(req)
	}

	method := req.Header.Get(mcpMethodHeader)
	outReq := req
	var trackedID string

	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				TaskID    string          `json:"taskId"`
				RequestID json.RawMessage `json:"requestId"`
			} `json:"params"`
		}
		decodeErr := json.NewDecoder(body).Decode(&envelope)
		closeErr := body.Close()
		if closeErr != nil {
			return nil, closeErr
		}
		if decodeErr == nil {
			if method == "" {
				method = envelope.Method
			}
			if id, ok := formatRawJSONID(envelope.ID); ok && method != "" {
				trackedID = id
				t.trackPendingMethod(trackedID, method)
			} else if envelope.Method == "notifications/cancelled" {
				if cancelID, ok := formatRawJSONID(envelope.Params.RequestID); ok {
					t.deletePendingMethod(cancelID)
				}
			}
			if isTaskMethod(envelope.Method) && envelope.Params.TaskID != "" &&
				req.Header.Get(mcpProtocolVersionHeader) >= tasksMinProtocolVersion {
				clone := req.Clone(req.Context())
				clone.Header.Set(mcpNameHeader, encodeMCPHeaderName(envelope.Params.TaskID))
				outReq = clone
			}
		}
	}

	resp, err := t.base.RoundTrip(outReq)
	if err != nil {
		if trackedID != "" {
			t.deletePendingMethod(trackedID)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if trackedID != "" {
			t.deletePendingMethod(trackedID)
		}
		return resp, nil
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json":
		if trackedID != "" {
			t.deletePendingMethod(trackedID)
		}
		return t.inspectJSONResponse(method, resp)
	case "text/event-stream":
		resp.Body = &taskHTTPBody{
			ReadCloser: resp.Body,
			reader:     bufio.NewReader(resp.Body),
			tasks:      t.tasks,
			method:     method,
			takeMethod: t.takePendingMethod,
		}
		return resp, nil
	default:
		if trackedID != "" {
			t.deletePendingMethod(trackedID)
		}
		return resp, nil
	}
}

func (t *taskHTTPTransport) inspectJSONResponse(method string, resp *http.Response) (*http.Response, error) {
	bodyBytes, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}

	msg, decodeErr := jsonrpc.DecodeMessage(bodyBytes)
	if decodeErr == nil {
		if rpcResp, ok := msg.(*jsonrpc.Response); ok {
			rewritten, changed, inspectErr := t.tasks.inspectResponse(method, rpcResp)
			if inspectErr != nil {
				return nil, inspectErr
			}
			if changed {
				encoded, encodeErr := jsonrpc.EncodeMessage(rewritten)
				if encodeErr != nil {
					return nil, encodeErr
				}
				bodyBytes = encoded
			}
		}
	}

	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	resp.ContentLength = int64(len(bodyBytes))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// taskHTTPBody is a pull-driven SSE frame inspector for Streamable HTTP
// responses carrying JSON-RPC responses over text/event-stream.
type taskHTTPBody struct {
	io.ReadCloser

	reader        *bufio.Reader
	tasks         *taskClient
	method        string
	takeMethod    func(string) (string, bool)
	pending       []byte
	skipLF        bool
	seenFirstLine bool
}

func (b *taskHTTPBody) readSSELine(remaining int) (string, error) {
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
			return "", errors.New("MCP task SSE frame exceeds maximum size")
		}
	}
}

func (b *taskHTTPBody) Read(p []byte) (int, error) {
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
			frame.WriteString(text)
			frame.WriteByte('\n')
			if text == "" {
				if eventName == "" || eventName == "message" {
					payload := strings.Join(data, "\n")
					if msg, decodeErr := jsonrpc.DecodeMessage([]byte(payload)); decodeErr == nil {
						if rpcResp, ok := msg.(*jsonrpc.Response); ok {
							method := b.method
							found := method != ""
							if b.takeMethod != nil {
								if trackedMethod, ok := b.takeMethod(formatSubscriptionID(rpcResp.ID.Raw())); ok {
									method = trackedMethod
									found = true
								}
							}
							if found {
								rewritten, changed, inspectErr := b.tasks.inspectResponse(method, rpcResp)
								if inspectErr != nil {
									return 0, inspectErr
								}
								if changed {
									encoded, encodeErr := jsonrpc.EncodeMessage(rewritten)
									if encodeErr != nil {
										return 0, encodeErr
									}
									var out bytes.Buffer
									out.Write(metaLines.Bytes())
									if eventName != "" {
										out.WriteString("event: ")
										out.WriteString(eventName)
										out.WriteByte('\n')
									}
									out.WriteString("data: ")
									out.Write(encoded)
									out.WriteString("\n\n")
									b.pending = out.Bytes()
									break
								}
							}
						}
					}
				}
				b.pending = frame.Bytes()
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

func (c *sessionClient) enableHTTPTasks(client *gomcp.Client, httpClient *http.Client) (*taskClient, error) {
	tasks, err := newTaskClient(client)
	if err != nil {
		return nil, err
	}
	tasks.sanitizeError = c.safeError
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = &taskHTTPTransport{
		base:           base,
		tasks:          tasks,
		pendingMethods: make(map[string]string),
	}
	return tasks, nil
}
