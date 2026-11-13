package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aymanbagabas/go-udiff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditFileMetaBoundsSerializedDiff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, before, after string
		added, removed      int
		wantChange          bool
	}{
		{"huge unchanged context", strings.Repeat("x", 17<<20) + "\nold\n", strings.Repeat("x", 17<<20) + "\nnew\n", 1, 1, true},
		{"huge changed line", "old\n", strings.Repeat("x", 17<<20) + "\n", 1, 1, false},
		{"JSON escaping", strings.Repeat("\x00", MaxEditFileMetaBytes/2) + "\nold\n", strings.Repeat("\x00", MaxEditFileMetaBytes/2) + "\nnew\n", 1, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, err := NewEditFileMeta("file.txt", tt.before, tt.after)
			require.NoError(t, err)
			assert.True(t, meta.Truncated)
			assert.Equal(t, tt.added, meta.Added)
			assert.Equal(t, tt.removed, meta.Removed)
			data, err := json.Marshal(meta)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(data), MaxEditFileMetaBytes)
			if tt.wantChange {
				require.NotEmpty(t, meta.Hunks)
				assert.Equal(t, 2, meta.Hunks[0].FromLine)
				assert.Equal(t, 2, meta.Hunks[0].ToLine)
				require.Len(t, meta.Hunks[0].Lines, 2)
				assert.Equal(t, udiff.Delete, meta.Hunks[0].Lines[0].Kind)
				assert.Equal(t, "old\n", meta.Hunks[0].Lines[0].Content)
				assert.Equal(t, "new\n", meta.Hunks[0].Lines[1].Content)
			} else {
				assert.Empty(t, meta.Hunks)
			}
		})
	}
}

func TestEditFileMetaBoundsManyHunks(t *testing.T) {
	t.Parallel()
	var before, after strings.Builder
	for range 100 {
		before.WriteString(strings.Repeat("a", 4096) + "\n")
		after.WriteString(strings.Repeat("b", 4096) + "\n")
		for range 8 {
			before.WriteString("context\n")
			after.WriteString("context\n")
		}
	}
	full, err := udiff.ToUnifiedDiff("file.txt", "file.txt", before.String(), udiff.Lines(before.String(), after.String()), 0)
	require.NoError(t, err)
	added, removed := 0, 0
	for _, hunk := range full.Hunks {
		for _, line := range hunk.Lines {
			switch line.Kind {
			case udiff.Insert:
				added++
			case udiff.Delete:
				removed++
			}
		}
	}
	meta, err := NewEditFileMeta("file.txt", before.String(), after.String())
	require.NoError(t, err)
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(data), MaxEditFileMetaBytes)
	assert.True(t, meta.Truncated)
	assert.Equal(t, added, meta.Added)
	assert.Equal(t, removed, meta.Removed)
	assert.NotEmpty(t, meta.Hunks)
	assert.Less(t, len(meta.Hunks), len(full.Hunks))
}
