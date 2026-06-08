package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/memory/database"
)

func TestBM25Tokenize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "lowercases and strips punctuation",
			input: "Go, modules! Are fun.",
			want:  []string{"modules", "fun"},
		},
		{
			name:  "removes stopwords",
			input: "the quick brown fox",
			want:  []string{"quick", "brown", "fox"},
		},
		{
			name:  "drops tokens shorter than three chars",
			input: "Go is ok",
			want:  []string{},
		},
		{
			name:  "empty input",
			input: "",
			want:  []string{},
		},
		{
			name:  "all stopwords",
			input: "the a an and or but",
			want:  []string{},
		},
		{
			name:  "normal sentence",
			input: "user prefers dark mode",
			want:  []string{"user", "prefers", "dark", "mode"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := bm25Tokenize(tt.input)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestBM25Rank(t *testing.T) {
	t.Parallel()

	memories := []database.UserMemory{
		{Memory: "I love Go"},
		{Memory: "Python is great"},
		{Memory: "Go modules are fun"},
	}

	t.Run("best match for go modules query comes first", func(t *testing.T) {
		t.Parallel()
		result := bm25Rank(memories, "go modules", 3)
		require.NotEmpty(t, result)
		assert.Equal(t, "Go modules are fun", result[0].Memory)
	})

	t.Run("limit caps results", func(t *testing.T) {
		t.Parallel()
		result := bm25Rank(memories, "go", 1)
		assert.LessOrEqual(t, len(result), 1)
	})

	t.Run("empty query returns nil", func(t *testing.T) {
		t.Parallel()
		result := bm25Rank(memories, "", 5)
		assert.Nil(t, result)
	})

	t.Run("all-stopword query returns nil", func(t *testing.T) {
		t.Parallel()
		result := bm25Rank(memories, "the a an and or", 5)
		assert.Nil(t, result)
	})

	t.Run("empty memories returns nil", func(t *testing.T) {
		t.Parallel()
		result := bm25Rank(nil, "go modules", 5)
		assert.Nil(t, result)
	})

	t.Run("zero limit returns nil", func(t *testing.T) {
		t.Parallel()
		result := bm25Rank(memories, "go", 0)
		assert.Nil(t, result)
	})

	t.Run("unrelated query excludes all", func(t *testing.T) {
		t.Parallel()
		// "zzzyyyxxx" doesn't match any token in the memory list.
		result := bm25Rank(memories, "zzzyyyxxx", 5)
		assert.Empty(t, result)
	})

	t.Run("results in descending score order", func(t *testing.T) {
		t.Parallel()
		mems := []database.UserMemory{
			{Memory: "Go language features design patterns"},
			{Memory: "Go language"},
			{Memory: "Python language features"},
		}
		result := bm25Rank(mems, "go language features", 3)
		require.Len(t, result, 3)
		// First result must score >= second.
		first := bm25Rank(mems, "go language features", 1)
		second := bm25Rank(mems, "go language features", 2)
		require.NotEmpty(t, first)
		require.Len(t, second, 2)
		assert.Equal(t, first[0], second[0])
	})
}
