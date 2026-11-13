package builtins

import (
	"encoding/json"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestRedactSecretsResultMetadata(t *testing.T) {
	t.Parallel()
	secret := fakeGitHubPAT()
	meta, err := filesystem.NewEditFileMeta("config.txt", "token="+secret+"\nold\n", "token="+secret+"\nnew\n")
	require.NoError(t, err)
	for _, event := range []hooks.EventType{hooks.EventToolResponseTransform, hooks.EventBeforeLLMCall} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()
			in := &hooks.Input{HookEventName: event, ToolResponse: "edited", ToolResultMeta: meta, Messages: []chat.Message{{Content: "edited", ToolResultMeta: meta}}}
			out, err := redactSecrets(t.Context(), in, nil)
			require.NoError(t, err)
			require.NotNil(t, out)
			data, err := json.Marshal(out)
			require.NoError(t, err)
			assert.NotContains(t, string(data), secret)
			assert.Contains(t, string(data), portcullis.Marker)
			if event == hooks.EventToolResponseTransform {
				assert.Nil(t, out.HookSpecificOutput.UpdatedToolResponse, "clean text should not need a rewrite")
				require.NotNil(t, out.HookSpecificOutput.UpdatedToolResultMeta)
				decoded, ok := filesystem.DecodeEditFileMeta(out.HookSpecificOutput.UpdatedToolResultMeta)
				require.True(t, ok)
				assert.Len(t, decoded.Hunks, len(meta.Hunks))
			} else {
				require.Len(t, out.HookSpecificOutput.UpdatedMessages, 1)
			}
			original, err := json.Marshal(in)
			require.NoError(t, err)
			assert.Contains(t, string(original), secret, "redaction must not mutate caller-owned data")
		})
	}
}

func TestRedactSecretsUnserializableResultMetadataFailsClosed(t *testing.T) {
	t.Parallel()
	out := redactToolOutput(&hooks.Input{ToolResponse: "clean", ToolResultMeta: make(chan int)})
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResultMeta)
	assert.JSONEq(t, "null", string(out.HookSpecificOutput.UpdatedToolResultMeta))
}
