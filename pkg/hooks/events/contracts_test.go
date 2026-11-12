package events

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocumentedEventContracts(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../../docs/configuration/hooks/index.md")
	require.NoError(t, err)
	var table strings.Builder
	table.WriteString("| Event | Execution | Can block | Failure default | Context | Rewrite |\n| --- | --- | --- | --- | --- | --- |\n")
	for c := range All() {
		execution, block, failure, context, rewrite := "parallel", "no", "warn", "no", "—"
		if c.Sequential() {
			execution = "sequential"
		}
		if c.CanBlock {
			block = "yes"
		}
		if c.FailClosed {
			failure = "block"
		}
		if c.Context {
			context = "yes"
		}
		switch c.Rewrite {
		case RewriteNone:
		case RewriteToolInput:
			rewrite = "tool input"
		case RewriteMessages:
			rewrite = "messages"
		case RewriteToolResponse:
			rewrite = "tool response"
		}
		fmt.Fprintf(&table, "| `%s` | %s | %s | %s | %s | %s |\n", c.Name, execution, block, failure, context, rewrite)
	}
	assert.Contains(t, string(data), table.String(), "update the event-contract table when changing contracts")
}
