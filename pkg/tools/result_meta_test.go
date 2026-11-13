package tools

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeResultMeta(t *testing.T) {
	t.Parallel()
	type meta struct {
		Count int `json:"count"`
	}
	for _, input := range []any{meta{Count: 3}, &meta{Count: 3}, json.RawMessage(`{"count":3}`), map[string]any{"count": float64(3)}} {
		decoded, ok := DecodeResultMeta[meta](input)
		require.True(t, ok)
		assert.Equal(t, meta{Count: 3}, decoded)
	}
	for _, input := range []any{nil, json.RawMessage(`null`), json.RawMessage(` null `), json.RawMessage(`invalid`), "foreign", make(chan int)} {
		_, ok := DecodeResultMeta[meta](input)
		assert.False(t, ok)
	}
	items, ok := DecodeResultMeta[[]string](json.RawMessage(`[]`))
	require.True(t, ok)
	assert.Empty(t, items)
}
