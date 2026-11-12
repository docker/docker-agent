package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode"
	"unicode/utf8"
)

const toolNameKey = "docker-agent/internal-acp-tool-name"

func toolNameMeta(name string) map[string]any {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) {
		return nil
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return nil
		}
	}
	return map[string]any{toolNameKey: name}
}

// The pinned SDK lacks the stable name field. Only promote our own carrier at
// the two protocol tool-call boundaries, never inside arbitrary tool payloads.
type toolNameWriter struct{ output io.Writer }

func (w *toolNameWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte(toolNameKey)) {
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
	if method != "session/update" && method != "session/request_permission" {
		return w.output.Write(p)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(message["params"], &params); err != nil {
		return 0, err
	}
	key := "toolCall"
	if method == "session/update" {
		key = "update"
	}
	var call map[string]json.RawMessage
	if err := json.Unmarshal(params[key], &call); err != nil {
		return 0, err
	}
	if method == "session/update" {
		var kind string
		if json.Unmarshal(call["sessionUpdate"], &kind) != nil || (kind != "tool_call" && kind != "tool_call_update") {
			return w.output.Write(p)
		}
	}
	var meta map[string]json.RawMessage
	if raw := call["_meta"]; raw != nil {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return 0, err
		}
	}
	raw, found := meta[toolNameKey]
	if !found {
		return w.output.Write(p)
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || toolNameMeta(name) == nil {
		return 0, errors.New("invalid ACP tool name carrier")
	}
	if existing, ok := call["name"]; ok {
		var value string
		if json.Unmarshal(existing, &value) != nil || value != name {
			return 0, errors.New("conflicting ACP tool name")
		}
	}
	call["name"] = raw
	delete(meta, toolNameKey)
	if len(meta) == 0 {
		delete(call, "_meta")
	} else {
		encoded, err := json.Marshal(meta)
		if err != nil {
			return 0, err
		}
		call["_meta"] = encoded
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		return 0, err
	}
	params[key] = encoded
	encoded, err = json.Marshal(params)
	if err != nil {
		return 0, err
	}
	message["params"] = encoded
	encoded, err = json.Marshal(message)
	if err != nil {
		return 0, err
	}
	encoded = append(encoded, '\n')
	n, err := w.output.Write(encoded)
	if err != nil {
		return 0, err
	}
	if n != len(encoded) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}
