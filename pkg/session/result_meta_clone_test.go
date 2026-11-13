package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestSessionCopiesIsolateToolResultMetadata(t *testing.T) {
	t.Parallel()
	for _, representation := range []string{"typed hunks", "JSON map", "raw JSON"} {
		t.Run(representation, func(t *testing.T) {
			t.Parallel()
			for _, operation := range []string{"clone", "branch", "fork", "snapshot"} {
				t.Run(operation, func(t *testing.T) {
					t.Parallel()
					original, err := filesystem.NewEditFileMeta("file.txt", "before\n", "after\n")
					require.NoError(t, err)
					encoded, err := json.Marshal(original)
					require.NoError(t, err)
					var meta any
					switch representation {
					case "typed hunks":
						meta = original
					case "JSON map":
						require.NoError(t, json.Unmarshal(encoded, &meta))
					case "raw JSON":
						meta = json.RawMessage(encoded)
					}
					parent := New()
					parent.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "edit", ToolResultMeta: meta}})
					var items []Item
					switch operation {
					case "clone":
						items = parent.Clone().Messages
					case "branch":
						branch, err := BranchSession(parent, parent.ItemCount())
						require.NoError(t, err)
						items = branch.Messages
					case "fork":
						fork, err := ForkSession(parent, parent.ItemCount())
						require.NoError(t, err)
						items = fork.Messages
					case "snapshot":
						items = parent.MessagesSnapshot()
					}
					require.Len(t, items, 1)
					copied, ok := filesystem.DecodeEditFileMeta(items[0].Message.Message.ToolResultMeta)
					require.True(t, ok)
					assert.Equal(t, original, copied)
					// Mutating decoded hunks must not affect the parent's typed hunk pointers.
					copied.Hunks[0].Lines[0].Content = "changed clone\n"
					unchanged, ok := filesystem.DecodeEditFileMeta(parent.Messages[0].Message.Message.ToolResultMeta)
					require.True(t, ok)
					assert.Equal(t, "before\n", unchanged.Hunks[0].Lines[0].Content)
					// Mutating the clone's raw JSON bytes must not affect any parent representation.
					raw, ok := items[0].Message.Message.ToolResultMeta.(json.RawMessage)
					require.True(t, ok)
					raw[0] = '!'
					parentJSON, err := json.Marshal(parent.Messages[0].Message.Message.ToolResultMeta)
					require.NoError(t, err)
					assert.JSONEq(t, string(encoded), string(parentJSON))
					// Mutating nested parent maps/hunks/bytes must not affect a separate snapshot.
					snapshot := parent.MessagesSnapshot()
					switch value := meta.(type) {
					case filesystem.EditFileMeta:
						value.Hunks[0].Lines[0].Content = "changed parent\n"
					case map[string]any:
						value["hunks"].([]any)[0].(map[string]any)["Lines"].([]any)[0].(map[string]any)["Content"] = "changed parent\n"
					case json.RawMessage:
						value[0] = '!'
					}
					restored, ok := filesystem.DecodeEditFileMeta(snapshot[0].Message.Message.ToolResultMeta)
					require.True(t, ok)
					assert.Equal(t, "before\n", restored.Hunks[0].Lines[0].Content)
				})
			}
		})
	}
}

func TestCloneToolResultMetadataPreservesJSONNumbersAndNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, cloneToolResultMeta(nil))
	value := map[string]any{"count": int64(9007199254740993), "nested": []any{map[string]any{"values": []string{"one"}}}}
	copied := cloneToolResultMeta(value)
	data, err := json.Marshal(copied)
	require.NoError(t, err)
	assert.Contains(t, string(data), "9007199254740993")
	decoded, ok := tools.DecodeResultMeta[map[string]json.RawMessage](copied)
	require.True(t, ok)
	assert.Equal(t, "9007199254740993", string(decoded["count"]))
	assert.Nil(t, cloneToolResultMeta(make(chan int)))
}
