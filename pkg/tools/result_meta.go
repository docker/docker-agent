package tools

import (
	"bytes"
	"encoding/json"
)

// DecodeResultMeta accepts in-process metadata and metadata decoded from hook or API JSON.
func DecodeResultMeta[T any](value any) (T, bool) {
	if typed, ok := value.(T); ok {
		return typed, true
	}
	var zero T
	if value == nil {
		return zero, false
	}
	data, err := json.Marshal(value)
	if err != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return zero, false
	}
	var decoded T
	if json.Unmarshal(data, &decoded) != nil {
		return zero, false
	}
	return decoded, true
}
