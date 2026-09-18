package openai

import (
	"bytes"
	"encoding/json"
)

// Preserve request diagnostics while removing opaque provider state.
func redactEncryptedContent(data []byte) string {
	if !bytes.Contains(data, []byte(`"encrypted_content"`)) {
		return string(data)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "<invalid JSON>"
	}
	redactEncryptedValue(value)
	redacted, err := json.Marshal(value)
	if err != nil {
		return "<invalid JSON>"
	}
	return string(redacted)
}

func redactEncryptedValue(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, field := range v {
			if key == "encrypted_content" {
				v[key] = "[redacted]"
			} else {
				redactEncryptedValue(field)
			}
		}
	case []any:
		for _, field := range v {
			redactEncryptedValue(field)
		}
	}
}
