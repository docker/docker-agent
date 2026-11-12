package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponseClone(t *testing.T) {
	t.Parallel()
	assert.Nil(t, (*OpenAIResponse)(nil).Clone())
	assert.Nil(t, (&OpenAIResponse{}).Clone().Output)
	assert.NotNil(t, (&OpenAIResponse{Output: []json.RawMessage{}}).Clone().Output)

	orig := &OpenAIResponse{Output: []json.RawMessage{nil, {}, make(json.RawMessage, 0, 8), json.RawMessage(`{"a":1}`)}}
	cloned := orig.Clone()
	require.Len(t, cloned.Output, 4)
	for _, item := range cloned.Output[:3] {
		assert.Nil(t, item)
	}
	assert.Equal(t, orig.Output[3], cloned.Output[3])
	cloned.Output[3][0] = 'X'
	assert.Equal(t, byte('{'), orig.Output[3][0])
	cloned.Output[0] = json.RawMessage(`{}`)
	assert.Nil(t, orig.Output[0])
	assert.NotNil(t, orig.Output[1])
}
