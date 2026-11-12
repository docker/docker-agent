//go:build js

package builtins_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// The browser limiter cannot spill to a file, so it must still bound the
// response, keep it valid UTF-8, say honestly that the rest is gone, and
// never touch the filesystem.
func TestLimitLargeToolResultsBrowserBoundsMCPResponse(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var b strings.Builder
	for i := range 3000 {
		b.WriteString(strings.Repeat("世", 200))
		b.WriteString(" line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	original := b.String()

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "browser-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "mcp",
		ToolName:      "search",
		ToolResponse:  original,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Less(t, len(updated), largeToolCallResultTailBytesForTest+1024, "notice plus excerpt stays bounded")
	assert.True(t, utf8.ValidString(updated))
	assert.Contains(t, updated, "Tool call result was too large")
	assert.Contains(t, updated, "not available in the browser")
	assert.Contains(t, updated, "Showing the last")
	assert.Contains(t, updated, " line 2999\n")
	assert.NotContains(t, updated, " line 0\n")
	assert.NotContains(t, updated, "available in a file")

	_, err = os.Stat(filepath.Join(os.TempDir(), "docker-agent-tool-results"))
	assert.True(t, os.IsNotExist(err), "the browser limiter must not write to the filesystem")
}

func TestLimitLargeToolResultsBrowserSessionEndIsNoop(t *testing.T) {
	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "browser-session",
		HookEventName: hooks.EventSessionEnd,
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
