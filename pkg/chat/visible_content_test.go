package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVisibleAssistantContentSuppressesToolPayloads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ content, want string }{
		{"safe answer", "safe answer"},
		{`<tool_call>{"arguments":"PRIVATE`, ""},
		{`safe prefix<tool_call>{"arguments":"PRIVATE`, "safe prefix"},
		{"safe<tool_call>hidden</tool_call>also hidden", "safe"},
	} {
		assert.Equal(t, test.want, VisibleAssistantContent(test.content))
	}
}
