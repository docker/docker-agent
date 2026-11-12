package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEditFileArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantPath   string
		wantEdits  []Edit
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:     "normal array edits",
			input:    `{"path": "test.txt", "edits": [{"oldText": "hello", "newText": "world"}]}`,
			wantPath: "test.txt",
			wantEdits: []Edit{
				{OldText: "hello", NewText: "world"},
			},
		},
		{
			name:     "double-serialized string edits",
			input:    `{"path": "test.txt", "edits": "[{\"oldText\": \"hello\", \"newText\": \"world\"}]"}`,
			wantPath: "test.txt",
			wantEdits: []Edit{
				{OldText: "hello", NewText: "world"},
			},
		},
		{
			name:     "double-serialized multiple edits",
			input:    `{"path": "f.go", "edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}, {\"oldText\": \"c\", \"newText\": \"d\"}]"}`,
			wantPath: "f.go",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
				{OldText: "c", NewText: "d"},
			},
		},
		{
			name:       "invalid JSON",
			input:      `not json at all`,
			wantErr:    true,
			wantErrMsg: "invalid character",
		},
		{
			name:       "edits is neither array nor string",
			input:      `{"path": "test.txt", "edits": 42}`,
			wantErr:    true,
			wantErrMsg: "edits field is neither an array nor a JSON string",
		},
		{
			name:       "double-serialized but inner JSON is invalid",
			input:      `{"path": "test.txt", "edits": "not valid json"}`,
			wantErr:    true,
			wantErrMsg: "failed to parse double-serialized edits string",
		},
		{
			name:     "repair: double-serialized with extra closing brace in inner payload",
			input:    `{"edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}}]", "path": "docker-compose.yml"}`,
			wantPath: "docker-compose.yml",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:     "repair: double-serialized with extra closing bracket in inner payload",
			input:    `{"path": "f.go", "edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}]]"}`,
			wantPath: "f.go",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:     "repair: double-serialized with extra closing brace between inner array elements",
			input:    `{"path": "f.go", "edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}}, {\"oldText\": \"c\", \"newText\": \"d\"}]"}`,
			wantPath: "f.go",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
				{OldText: "c", NewText: "d"},
			},
		},
		{
			name:       "repair: rejected when inner repair yields an edit with empty oldText",
			input:      `{"path": "f.go", "edits": "[{\"oldText\": \"a\"}}, {\"newText\": \"b\"}]"}`,
			wantErr:    true,
			wantErrMsg: "empty oldText",
		},
		{
			name:     "missing edits field (partial/streaming args)",
			input:    `{"path": "/tmp/test.txt"}`,
			wantPath: "/tmp/test.txt",
		},
		{
			name:     "null edits field",
			input:    `{"path": "test.txt", "edits": null}`,
			wantPath: "test.txt",
		},
		{
			name:  "missing path with double-serialized edits",
			input: `{"edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}]"}`,
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:  "missing path with normal array edits",
			input: `{"edits": [{"oldText": "a", "newText": "b"}]}`,
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},

		// Malformed outer JSON — LLM brace/bracket counting errors.
		{
			name:     "repair: extra closing brace before array close",
			input:    `{"path": "ci.yml", "edits": [{"oldText": "old", "newText": "new"}}]}`,
			wantPath: "ci.yml",
			wantEdits: []Edit{
				{OldText: "old", NewText: "new"},
			},
		},
		{
			name:     "repair: extra closing brace with trailing newline",
			input:    "{\"path\": \"ci.yml\", \"edits\": [{\"oldText\": \"old\", \"newText\": \"new\"}}]\n}",
			wantPath: "ci.yml",
			wantEdits: []Edit{
				{OldText: "old", NewText: "new"},
			},
		},
		{
			name:     "repair: extra closing bracket (spurious array wrapper)",
			input:    `{"path": "build.sh", "edits": [{"oldText": "a", "newText": "b"}]]}`,
			wantPath: "build.sh",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:     "repair: stray backslash-n between tokens",
			input:    "{\"path\": \"Dockerfile\", \"edits\": [{\"oldText\": \"a\", \"newText\": \"b\"}\\n]}",
			wantPath: "Dockerfile",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:     "repair: stray backslash before property name",
			input:    `{"path": "f.go", "edits": [{"oldText": "a", "newText": "b"},{\"oldText": "c", "newText": "d"}]}`,
			wantPath: "f.go",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
				{OldText: "c", NewText: "d"},
			},
		},
		{
			name:       "unrepairable garbage",
			input:      `{totally broken <<<>>>`,
			wantErr:    true,
			wantErrMsg: "failed to parse edit_file arguments",
		},
		// Dropping the stray backslash closes the string early, leaving an empty
		// oldText. That means the repair removed a load-bearing character, so the
		// payload must be rejected — the double-serialized path already does this.
		{
			name:       "repair that empties oldText is rejected (outer payload)",
			input:      `{"path": "target.txt", "edits": [{"oldText":\"", "newText": "INJECTED"}]}`,
			wantErr:    true,
			wantErrMsg: "empty oldText",
		},
		{
			name:       "repair that empties oldText is rejected (double-serialized payload)",
			input:      `{"path": "target.txt", "edits": "[{\"oldText\":\\\"\",\"newText\":\"INJECTED\"}]"}`,
			wantErr:    true,
			wantErrMsg: "empty oldText",
		},
		// Well-formed JSON is never second-guessed here: the TUI parses
		// partially-streamed arguments with this function, where a not-yet-filled
		// oldText is normal. handleEditFile is the layer that refuses to apply it.
		{
			name:      "well-formed empty oldText still parses for streaming renderers",
			input:     `{"path": "target.txt", "edits": [{"oldText": "", "newText": "x"}]}`,
			wantPath:  "target.txt",
			wantEdits: []Edit{{OldText: "", NewText: "x"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args, err := ParseEditFileArgs([]byte(tc.input))
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, args.Path)
			assert.Equal(t, tc.wantEdits, args.Edits)
		})
	}
}
