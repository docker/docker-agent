package history

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	assert.Equal(t, -1, h.current)
	assert.Empty(t, h.Messages)
}

func TestHistory_AddAndSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	messages := []string{"first", "second", "third"}
	for _, msg := range messages {
		err := h.Add(msg)
		require.NoError(t, err)
	}

	assert.Equal(t, messages, h.Messages)
	assert.Len(t, messages, h.current)

	h2, err := New()
	require.NoError(t, err)
	assert.Equal(t, messages, h2.Messages)
}

func TestHistory_Navigation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	assert.Empty(t, h.Previous())
	assert.Empty(t, h.Next())

	messages := []string{"first", "second", "third"}
	for _, msg := range messages {
		require.NoError(t, h.Add(msg))
	}

	assert.Equal(t, "third", h.Previous())
	assert.Equal(t, "second", h.Previous())
	assert.Equal(t, "first", h.Previous())
	assert.Equal(t, "first", h.Previous())

	assert.Equal(t, "second", h.Next())
	assert.Equal(t, "third", h.Next())
	assert.Empty(t, h.Next())
}

func TestHistory_EdgeCases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	assert.Empty(t, h.Previous())
	assert.Empty(t, h.Next())

	require.NoError(t, h.Add("only"))

	assert.Equal(t, "only", h.Previous())
	assert.Equal(t, "only", h.Previous()) // Should stay at the beginning
	assert.Empty(t, h.Next())             // Should return empty when going past the end
}

func TestHistory_StayAtTheBeginning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))

	assert.Equal(t, "first", h.Previous())
	assert.Equal(t, "first", h.Previous())
}

func TestHistory_NoDuplicateMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))
	require.NoError(t, h.Add("second"))
	require.NoError(t, h.Add("second"))

	assert.Equal(t, "second", h.Previous())
	assert.Equal(t, "first", h.Previous())
	assert.Equal(t, "first", h.Previous())
}

func TestHistory_MoveDuplicateLast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h, err := New()
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))
	require.NoError(t, h.Add("second"))
	require.NoError(t, h.Add("third"))
	require.NoError(t, h.Add("first"))

	assert.Equal(t, "first", h.Previous())
	assert.Equal(t, "third", h.Previous())
	assert.Equal(t, "second", h.Previous())
	assert.Equal(t, "second", h.Previous())
}

func TestHistory_MultilineMessage(t *testing.T) {
	tmpDir := t.TempDir()

	h, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)

	multiline := "line1\nline2\nline3"
	require.NoError(t, h.Add(multiline))

	h2, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)

	require.Len(t, h2.Messages, 1)
	assert.Equal(t, multiline, h2.Messages[0])
}

func TestHistory_MigrateOldFormat(t *testing.T) {
	tmpDir := t.TempDir()
	err := os.MkdirAll(filepath.Join(tmpDir, ".cagent"), 0o755)
	require.NoError(t, err)
	oldHistFile := filepath.Join(tmpDir, ".cagent", "history.json")
	newHistFile := filepath.Join(tmpDir, ".cagent", "history")

	require.NoError(t, os.WriteFile(oldHistFile, []byte(`{"messages":["old1","old2","old3"]}`), 0o644))

	h, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)
	assert.Equal(t, []string{"old1", "old2", "old3"}, h.Messages)

	_, err = os.Stat(oldHistFile)
	assert.True(t, os.IsNotExist(err), "old history.json should be removed")

	_, err = os.Stat(newHistFile)
	assert.NoError(t, err, "new history file should exist")
}

func TestHistory_LatestMatch(t *testing.T) {
	tmpDir := t.TempDir()

	h, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)

	// Empty history returns empty string
	assert.Empty(t, h.LatestMatch(""))
	assert.Empty(t, h.LatestMatch("prefix"))

	// Add some messages
	require.NoError(t, h.Add("hello world"))
	require.NoError(t, h.Add("hello there"))
	require.NoError(t, h.Add("goodbye"))

	// Empty prefix returns latest message
	assert.Equal(t, "goodbye", h.LatestMatch(""))

	// Prefix matching returns latest match
	assert.Equal(t, "hello there", h.LatestMatch("hello"))
	assert.Equal(t, "goodbye", h.LatestMatch("good"))

	// No match returns empty string
	assert.Empty(t, h.LatestMatch("xyz"))

	// Exact match doesn't count (must extend prefix)
	assert.Empty(t, h.LatestMatch("goodbye"))
}

func TestHistory_FindPrevContains(t *testing.T) {
	t.Parallel()

	t.Run("empty history", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		msg, idx, ok := h.FindPrevContains("test", len(h.Messages))
		assert.False(t, ok)
		assert.Empty(t, msg)
		assert.Equal(t, -1, idx)
	})

	t.Run("empty query matches latest", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("first"))
		require.NoError(t, h.Add("second"))
		require.NoError(t, h.Add("third"))

		msg, idx, ok := h.FindPrevContains("", len(h.Messages))
		assert.True(t, ok)
		assert.Equal(t, "third", msg)
		assert.Equal(t, 2, idx)
	})

	t.Run("substring match", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("deploy staging"))
		require.NoError(t, h.Add("run tests"))
		require.NoError(t, h.Add("deploy production"))

		msg, idx, ok := h.FindPrevContains("deploy", len(h.Messages))
		assert.True(t, ok)
		assert.Equal(t, "deploy production", msg)
		assert.Equal(t, 2, idx)
	})

	t.Run("case insensitive match", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("Deploy Staging"))
		require.NoError(t, h.Add("run tests"))

		msg, idx, ok := h.FindPrevContains("deploy", len(h.Messages))
		assert.True(t, ok)
		assert.Equal(t, "Deploy Staging", msg)
		assert.Equal(t, 0, idx)
	})

	t.Run("cycling through matches", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("deploy v1"))
		require.NoError(t, h.Add("run tests"))
		require.NoError(t, h.Add("deploy v2"))
		require.NoError(t, h.Add("check logs"))
		require.NoError(t, h.Add("deploy v3"))

		// First match: most recent
		msg, idx, ok := h.FindPrevContains("deploy", len(h.Messages))
		assert.True(t, ok)
		assert.Equal(t, "deploy v3", msg)
		assert.Equal(t, 4, idx)

		// Cycle to next older match
		msg, idx, ok = h.FindPrevContains("deploy", idx)
		assert.True(t, ok)
		assert.Equal(t, "deploy v2", msg)
		assert.Equal(t, 2, idx)

		// Cycle to oldest match
		msg, idx, ok = h.FindPrevContains("deploy", idx)
		assert.True(t, ok)
		assert.Equal(t, "deploy v1", msg)
		assert.Equal(t, 0, idx)

		// No more matches
		_, _, ok = h.FindPrevContains("deploy", idx)
		assert.False(t, ok)
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("hello"))
		require.NoError(t, h.Add("world"))

		msg, idx, ok := h.FindPrevContains("xyz", len(h.Messages))
		assert.False(t, ok)
		assert.Empty(t, msg)
		assert.Equal(t, -1, idx)
	})

	t.Run("from out of bounds", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("hello"))

		msg, idx, ok := h.FindPrevContains("hello", 100)
		assert.True(t, ok)
		assert.Equal(t, "hello", msg)
		assert.Equal(t, 0, idx)
	})
}

func TestHistory_FindNextContains(t *testing.T) {
	t.Parallel()

	t.Run("basic forward search", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("deploy v1"))
		require.NoError(t, h.Add("run tests"))
		require.NoError(t, h.Add("deploy v2"))

		msg, idx, ok := h.FindNextContains("deploy", -1)
		assert.True(t, ok)
		assert.Equal(t, "deploy v1", msg)
		assert.Equal(t, 0, idx)
	})

	t.Run("sequential forward search", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("echo 1"))
		require.NoError(t, h.Add("echo 2"))
		require.NoError(t, h.Add("echo 3"))

		msg, idx, ok := h.FindNextContains("echo", -1)
		assert.True(t, ok)
		assert.Equal(t, "echo 1", msg)
		assert.Equal(t, 0, idx)

		msg, idx, ok = h.FindNextContains("echo", idx)
		assert.True(t, ok)
		assert.Equal(t, "echo 2", msg)
		assert.Equal(t, 1, idx)

		msg, idx, ok = h.FindNextContains("echo", idx)
		assert.True(t, ok)
		assert.Equal(t, "echo 3", msg)
		assert.Equal(t, 2, idx)

		_, _, ok = h.FindNextContains("echo", idx)
		assert.False(t, ok)
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("hello"))

		msg, idx, ok := h.FindNextContains("xyz", -1)
		assert.False(t, ok)
		assert.Empty(t, msg)
		assert.Equal(t, -1, idx)
	})

	t.Run("empty history", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		_, _, ok := h.FindNextContains("test", -1)
		assert.False(t, ok)
	})

	t.Run("case insensitive", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(WithBaseDir(tmpDir))
		require.NoError(t, err)

		require.NoError(t, h.Add("Deploy Staging"))

		msg, _, ok := h.FindNextContains("deploy", -1)
		assert.True(t, ok)
		assert.Equal(t, "Deploy Staging", msg)
	})
}

func TestHistory_SetCurrent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	h, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))
	require.NoError(t, h.Add("second"))
	require.NoError(t, h.Add("third"))

	h.SetCurrent(1)
	assert.Equal(t, "first", h.Previous())

	h.SetCurrent(2)
	assert.Empty(t, h.Next())
}

func TestHistory_VeryLongMessage(t *testing.T) {
	tmpDir := t.TempDir()

	h, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)

	// Create a message longer than bufio.Scanner's default 64KB limit
	longMessage := make([]byte, 100*1024) // 100KB
	for i := range longMessage {
		longMessage[i] = 'a' + byte(i%26)
	}
	longStr := string(longMessage)

	require.NoError(t, h.Add(longStr))
	require.NoError(t, h.Add("short message after"))

	// Reload history from disk
	h2, err := New(WithBaseDir(tmpDir))
	require.NoError(t, err)

	require.Len(t, h2.Messages, 2)
	assert.Equal(t, longStr, h2.Messages[0])
	assert.Equal(t, "short message after", h2.Messages[1])
}
