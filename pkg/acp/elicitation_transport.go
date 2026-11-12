package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

const (
	elicitationSessionKey = "docker-agent/internal-acp-session"
	maxElicitationBytes   = 256 << 10
)

// NewConnection binds the pinned SDK with elicitation scope and tool-name adapters.
func (a *Agent) NewConnection(input io.Writer, output io.Reader) *acp.AgentSideConnection {
	pending := &elicitationRequests{ids: make(map[string]string)}
	reader := &elicitationReader{scanner: bufio.NewScanner(output), pending: pending}
	reader.scanner.Buffer(make([]byte, 4096), 10<<20)
	conn := acp.NewAgentSideConnection(a, &elicitationWriter{output: &toolNameWriter{output: input}, pending: pending}, reader)
	a.SetAgentConnection(conn)
	a.elicitationConn = conn
	return conn
}

// The SDK writes one complete JSON-RPC message per Write, under its write mutex.
// Remove this workaround when SDK form/URL requests gain top-level sessionId.
type elicitationWriter struct {
	output  io.Writer
	pending *elicitationRequests
}

func (w *elicitationWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte(`"elicitation/create"`)) && !bytes.Contains(p, []byte(`"$/cancel_request"`)) {
		return w.output.Write(p)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(p, &message); err != nil {
		return 0, err
	}
	var method string
	if err := json.Unmarshal(message["method"], &method); err != nil {
		return w.output.Write(p)
	}
	if method == "$/cancel_request" && w.pending != nil {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(message["params"], &params) == nil {
			w.pending.take(params.RequestID)
		}
	}
	if method != acp.ClientMethodElicitationCreate {
		return w.output.Write(p)
	}
	if len(p) > maxElicitationBytes {
		return 0, errors.New("elicitation request too large")
	}
	var params, meta map[string]json.RawMessage
	if err := json.Unmarshal(message["params"], &params); err != nil {
		return 0, err
	}
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		return 0, err
	}
	var sid string
	if err := json.Unmarshal(meta[elicitationSessionKey], &sid); err != nil || sid == "" {
		return 0, errors.New("elicitation session scope missing")
	}
	if _, exists := params["sessionId"]; exists {
		return 0, errors.New("conflicting elicitation scope")
	}
	params["sessionId"] = meta[elicitationSessionKey]
	delete(meta, elicitationSessionKey)
	if len(meta) == 0 {
		delete(params, "_meta")
	} else {
		encoded, err := json.Marshal(meta)
		if err != nil {
			return 0, err
		}
		params["_meta"] = encoded
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}
	message["params"] = encoded
	encoded, err = json.Marshal(message)
	if err != nil {
		return 0, err
	}
	if len(encoded) > maxElicitationBytes {
		return 0, errors.New("elicitation request too large")
	}
	encoded = append(encoded, '\n')
	if w.pending != nil {
		var mode string
		if err := json.Unmarshal(params["mode"], &mode); err != nil {
			return 0, err
		}
		if err := w.pending.add(message["id"], mode); err != nil {
			return 0, err
		}
	}
	n, err := w.output.Write(encoded)
	if err != nil {
		if w.pending != nil {
			w.pending.take(message["id"])
		}
		return 0, err
	}
	if n != len(encoded) {
		if w.pending != nil {
			w.pending.take(message["id"])
		}
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

type elicitationRequests struct {
	mu  sync.Mutex
	ids map[string]string
}

func elicitationRequestID(raw json.RawMessage) string {
	// Match the SDK's numeric IDs without imposing the stricter form-value bounds.
	text := strings.TrimSpace(string(raw))
	if !json.Valid(raw) || text == "" || text[0] == '"' || text[0] == '-' {
		return ""
	}
	mantissa, exponent := text, 0
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		mantissa = text[:i]
		exp := text[i+1:]
		negative := strings.HasPrefix(exp, "-")
		exp = strings.TrimLeft(strings.TrimLeft(exp, "+-"), "0")
		if len(exp) > 4 {
			return ""
		}
		if exp != "" {
			value, err := strconv.Atoi(exp)
			if err != nil || value > 4096 {
				return ""
			}
			exponent = value
		}
		if negative {
			exponent = -exponent
		}
	}
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		exponent -= len(mantissa) - i - 1
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return "0"
	}
	if len(digits) > 4096 {
		return ""
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent += len(digits) - len(trimmed)
	if exponent < 0 || len(trimmed)+exponent > 20 {
		return ""
	}
	return trimmed + strings.Repeat("0", exponent)
}

func (p *elicitationRequests) add(raw json.RawMessage, mode string) error {
	id := elicitationRequestID(raw)
	if id == "" {
		return errors.New("missing elicitation request ID")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) >= 128 {
		return errors.New("too many pending elicitations")
	}
	p.ids[id] = mode
	return nil
}

func (p *elicitationRequests) take(raw json.RawMessage) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := elicitationRequestID(raw)
	mode := p.ids[id]
	delete(p.ids, id)
	return mode
}

// Inspect only matched elicitation responses before the SDK loses numeric precision.
type elicitationReader struct {
	scanner *bufio.Scanner
	pending *elicitationRequests
	buffer  []byte
}

func (r *elicitationReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.buffer) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := r.scanner.Bytes()
		// Mirror the SDK envelope's field types and case-insensitive decoding, including null errors.
		var message struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      *json.RawMessage  `json:"id,omitempty"`
			Method  string            `json:"method,omitempty"`
			Params  json.RawMessage   `json:"params,omitempty"`
			Result  json.RawMessage   `json:"result,omitempty"`
			Error   *acp.RequestError `json:"error,omitempty"`
		}
		if json.Unmarshal(line, &message) == nil && message.Method == "" && message.ID != nil {
			if mode := r.pending.take(*message.ID); mode != "" && message.Error == nil {
				var result struct {
					Action  string          `json:"action"`
					Content json.RawMessage `json:"content,omitempty"`
				}
				valid := len(line) <= maxElicitationBytes && json.Unmarshal(message.Result, &result) == nil
				if result.Action == "accept" && mode == "form" && valid {
					valid = safeElicitationNumbers(message.Result)
				}
				if !valid {
					message.Result = json.RawMessage(`{"action":"decline"}`)
				} else if mode == "url" || result.Action != "accept" {
					result.Content = nil
					message.Result, _ = json.Marshal(result)
				}
				encoded, err := json.Marshal(message)
				if err != nil {
					return 0, err
				}
				if len(encoded) > maxElicitationBytes {
					// Normalization can expand escapes; don't retain oversized envelope fields.
					id := elicitationRequestID(*message.ID)
					line = []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"action":"decline"}}`)
				} else {
					line = encoded
				}
			}
		}
		r.buffer = append(slices.Clone(line), '\n')
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}
