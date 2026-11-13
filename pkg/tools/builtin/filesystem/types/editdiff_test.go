package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aymanbagabas/go-udiff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditFileMeta(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, before, after string
		added, removed      int
	}{
		{"middle deletion", "a\nb\nc\n", "a\nc\n", 0, 1},
		{"whole deletion", "a\nb\n", "", 0, 2},
		{"insertion", "a\nc\n", "a\nb\nc\n", 1, 0},
		{"replacement", "a\nb\nc\n", "a\nx\nc\n", 1, 1},
		{"no trailing newline", "a\nb", "a\n", 0, 1},
		{"unicode and CRLF", "α\r\nβ\r\nγ\r\n", "α\r\nγ\r\n", 0, 1},
		{"no op", "a\n", "a\n", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, err := NewEditFileMeta("file.txt", tt.before, tt.after)
			require.NoError(t, err)
			var before, after strings.Builder
			added, removed := 0, 0
			for _, hunk := range meta.Hunks {
				for _, line := range hunk.Lines {
					switch line.Kind {
					case udiff.Equal:
						before.WriteString(line.Content)
						after.WriteString(line.Content)
					case udiff.Delete:
						before.WriteString(line.Content)
						removed++
					case udiff.Insert:
						after.WriteString(line.Content)
						added++
					}
				}
			}
			if tt.before != tt.after {
				assert.Equal(t, tt.before, before.String())
				assert.Equal(t, tt.after, after.String())
			}
			assert.Equal(t, tt.added, added)
			assert.Equal(t, tt.removed, removed)
			encoded, err := json.Marshal(meta)
			require.NoError(t, err)
			var value any
			require.NoError(t, json.Unmarshal(encoded, &value))
			decoded, ok := DecodeEditFileMeta(value)
			require.True(t, ok)
			assert.Equal(t, meta, decoded)
		})
	}
}

func TestApplyEdits(t *testing.T) {
	t.Parallel()
	after, err := ApplyEdits("first\nsecond\nthird\n", []Edit{
		{OldText: "second\n", NewText: "intermediate\n"},
		{OldText: "intermediate\n"},
	})
	require.NoError(t, err)
	assert.Equal(t, "first\nthird\n", after)
	after, err = ApplyEdits("same\nsame\n", []Edit{{OldText: "same\n"}})
	require.NoError(t, err)
	assert.Equal(t, "same\n", after)
	_, err = ApplyEdits("original", []Edit{{NewText: "injected"}})
	require.EqualError(t, err, "Edit 1 failed: oldText must not be empty")
	_, err = ApplyEdits("original", []Edit{{OldText: "original", NewText: "updated"}, {OldText: "absent"}})
	require.EqualError(t, err, "Edit 2 failed: old text not found")
}

func TestDecodeEditFileMetaRejectsMissingMetadata(t *testing.T) {
	t.Parallel()
	for _, value := range []any{nil, map[string]any{}, "invalid", make(chan int)} {
		_, ok := DecodeEditFileMeta(value)
		assert.False(t, ok)
	}
}
