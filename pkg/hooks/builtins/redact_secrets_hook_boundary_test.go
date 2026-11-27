package builtins

import (
	"context"
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

// TestFinalRedactionPassSurvivesExplicitRedactorEntry pins the final-pass
// requirement: an agent that also wires the builtin by hand — as
// examples/redact_secrets_hooks.yaml does — must not lose the auto-injected
// final scrub to the executor's hook deduplication. `hooksFor` compares
// configured values, so an explicit entry that copies the automatic pass's
// label and options used to collapse the final pass into itself, leaving the
// explicit entry at its configured position. A transform after it could then
// rewrite a secret back into the attachments and the runtime would emit and
// record it unsanitized. The automatic passes therefore carry an internal
// identity that YAML cannot reproduce, which keeps them alive whatever the
// explicit entry's `name` is.
func TestFinalRedactionPassSurvivesExplicitRedactorEntry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		explicitName string
	}{
		{name: "unnamed explicit redactor"},
		{
			name:         "explicit redactor copies the final pass name",
			explicitName: "redact_secrets (final pass)",
		},
		{
			name:         "explicit redactor copies the leading pass name",
			explicitName: "redact_secrets (before configured hooks)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secret := fakeGitHubPAT()
			registry := hooks.NewRegistry()
			require.NoError(t, Register(registry))
			require.NoError(t, registry.RegisterBuiltin("reintroduce_secret", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
				return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
					UpdatedToolDocuments: []tools.DocumentContent{
						{Name: "leaked.md", MimeType: "text/markdown", Text: "token=" + secret},
					},
				}}, nil
			}))

			cfg := ApplyAgentDefaults(&hooks.Config{
				ToolResponseTransform: []hooks.MatcherConfig{
					{
						Matcher: "*",
						// Explicit redactor: the same builtin the runtime
						// auto-injects. Its label is user-configurable, so a
						// matching name must not be able to replace an
						// automatic pass.
						Hooks: []hooks.Hook{{
							Name:    tc.explicitName,
							Type:    hooks.HookTypeBuiltin,
							Command: RedactSecrets,
						}},
					},
					{
						Matcher: "*",
						// Runs after the explicit redactor and rewrites the
						// secret back in: only a surviving final pass can
						// scrub this.
						Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "reintroduce_secret"}},
					},
				},
			}, AgentDefaults{RedactSecrets: true})

			exec := hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)
			result, err := exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, &hooks.Input{
				ToolName:     "mcp_fetch",
				ToolCategory: "mcp",
				ToolResponse: "fetched resource",
				ToolDocuments: []tools.DocumentContent{
					{Name: "clean.md", MimeType: "text/markdown", Text: "nothing to see"},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, result)

			// The reintroduced list is the runtime's rewrite: it must
			// already be scrubbed by the final auto-injected pass.
			require.NotNil(t, result.UpdatedToolDocuments, "the rewrite from the configured hook must reach the runtime")
			require.Len(t, result.UpdatedToolDocuments, 1)
			assert.NotContains(t, result.UpdatedToolDocuments[0].Text, secret,
				"the final redaction pass must survive hook dedup when the agent wires the builtin explicitly")
			assert.Contains(t, result.UpdatedToolDocuments[0].Text, portcullis.Marker)
		})
	}
}

// TestExplicitRedactorCopyingLeadingPassNameRunsInPlace pins the other half
// of the dedup contract: an explicit redactor whose label copies the
// auto-injected leading pass must still run at its configured position. A
// command hook placed after it records the hook input on stdin; without the
// explicit redactor, a secret reintroduced by the preceding transform would
// reach that hook even though the automatic passes bracket the sequence.
func TestExplicitRedactorCopyingLeadingPassNameRunsInPlace(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	capture := filepath.Join(t.TempDir(), "hook-stdin.json")
	registry := hooks.NewRegistry()
	require.NoError(t, Register(registry))
	require.NoError(t, registry.RegisterBuiltin("reintroduce_secret", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
			UpdatedToolDocuments: []tools.DocumentContent{
				{Name: "leaked.md", MimeType: "text/markdown", Text: "token=" + secret},
			},
		}}, nil
	}))

	cfg := ApplyAgentDefaults(&hooks.Config{
		ToolResponseTransform: []hooks.MatcherConfig{
			{
				// Reintroduces a secret after the automatic leading pass.
				Matcher: "*",
				Hooks:   []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "reintroduce_secret"}},
			},
			{
				// Copies the automatic leading pass's label. This entry must
				// still run here, between the two transforms.
				Matcher: "*",
				Hooks: []hooks.Hook{{
					Name:    "redact_secrets (before configured hooks)",
					Type:    hooks.HookTypeBuiltin,
					Command: RedactSecrets,
				}},
			},
			{
				Matcher: "*",
				Hooks:   []hooks.Hook{{Type: hooks.HookTypeCommand, Command: captureStdinCommand(t, capture)}},
			},
		},
	}, AgentDefaults{RedactSecrets: true})

	exec := hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)
	result, err := exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, &hooks.Input{
		ToolName:     "mcp_fetch",
		ToolCategory: "mcp",
		ToolResponse: "fetched resource",
		ToolDocuments: []tools.DocumentContent{
			{Name: "clean.md", MimeType: "text/markdown", Text: "nothing to see"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	raw, err := os.ReadFile(capture)
	require.NoError(t, err, "the configured command hook must have run and captured its stdin")
	stdin := string(raw)
	assert.NotContains(t, stdin, secret,
		"the explicit redactor copying the leading pass name must still run at its configured position")
	assert.Contains(t, stdin, portcullis.Marker)

	// The automatic final pass keeps the guarantee for emitted results.
	require.NotNil(t, result.UpdatedToolDocuments)
	require.Len(t, result.UpdatedToolDocuments, 1)
	assert.NotContains(t, result.UpdatedToolDocuments[0].Text, secret)
}
