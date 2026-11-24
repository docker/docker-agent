package builtins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestConfiguredCommandHookReceivesRedactedToolDocuments pins the hook
// boundary: with the agent-level redact_secrets flag on (the default), an
// auto-injected redaction pass runs ahead of user-configured
// tool_response_transform hooks. Command hooks receive the whole hook input
// as JSON on stdin, so without that preamble a hook that archives its input
// would record the raw inline document attachments (and raw tool output)
// that MCP servers return as text resources.
//
// The assertion is on the captured stdin, not on in-process hook input:
// that is exactly the channel the review flagged (pkg/hooks/handler.go
// pipes the serialized Input to the hook process).
func TestConfiguredCommandHookReceivesRedactedToolDocuments(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	capture := filepath.Join(t.TempDir(), "hook-stdin.json")
	cfg := ApplyAgentDefaults(&hooks.Config{
		ToolResponseTransform: []hooks.MatcherConfig{{
			Matcher: "*",
			Hooks: []hooks.Hook{{
				Type:    hooks.HookTypeCommand,
				Command: captureStdinCommand(t, capture),
			}},
		}},
	}, AgentDefaults{RedactSecrets: true})

	registry := hooks.NewRegistry()
	require.NoError(t, Register(registry))
	exec := hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)

	result, err := exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, &hooks.Input{
		ToolName:     "mcp_fetch",
		ToolCategory: "mcp",
		ToolResponse: "fetched token=" + secret,
		ToolDocuments: []tools.DocumentContent{
			{Name: "notes.md", URI: "mcp://docs/notes.md", MimeType: "text/markdown", Text: "token=" + secret},
			{Name: "clean.md", MimeType: "text/markdown", Text: "nothing to see"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	raw, err := os.ReadFile(capture)
	require.NoError(t, err, "the configured command hook must have run and captured its stdin")

	stdin := string(raw)
	assert.NotContains(t, stdin, secret, "the configured command hook's stdin must not carry the raw secret")
	assert.Contains(t, stdin, portcullis.Marker, "the hook must see the redacted form instead")

	var captured hooks.Input
	require.NoError(t, json.Unmarshal(raw, &captured), "the captured stdin must be the hook input protocol")
	require.Len(t, captured.ToolDocuments, 2)
	assert.NotContains(t, captured.ToolDocuments[0].Text, secret)
	assert.Contains(t, captured.ToolDocuments[0].Text, portcullis.Marker)
	assert.Equal(t, "nothing to see", captured.ToolDocuments[1].Text, "clean attachments must survive byte-for-byte")

	// The trailing pass still runs after the configured hooks: the rewrite
	// the runtime consumes is the redacted list.
	require.Len(t, result.UpdatedToolDocuments, 2)
	assert.NotContains(t, result.UpdatedToolDocuments[0].Text, secret)
	assert.Equal(t, "nothing to see", result.UpdatedToolDocuments[1].Text)
}
