package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/paths"
)

func TestNew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, ".cagent", "history"), h.path)
	assert.Equal(t, 0, h.current)
	assert.Empty(t, h.Messages)
}

func TestNew_FollowsDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	homeDataDir := filepath.Join(home, ".cagent")
	require.NoError(t, os.MkdirAll(homeDataDir, 0o700))
	homeHistory := filepath.Join(homeDataDir, "history")
	homeLegacy := filepath.Join(homeDataDir, "history.json")
	require.NoError(t, os.WriteFile(homeHistory, []byte("\"home prompt\"\n"), 0o600))
	require.NoError(t, os.WriteFile(homeLegacy, []byte(`{"messages":["legacy home prompt"]}`), 0o600))

	dir := filepath.Join(t.TempDir(), "data")
	paths.SetDataDir(dir)
	t.Cleanup(func() { paths.SetDataDir("") })

	h, err := New("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "history"), h.path)
	assert.Empty(t, h.Messages)
	require.NoError(t, h.Add("custom prompt"))

	reloaded, err := New("")
	require.NoError(t, err)
	require.NoError(t, reloaded.Load())
	assert.Equal(t, []string{"custom prompt"}, reloaded.Messages)
	assert.Equal(t, 1, reloaded.current)
	assert.NoDirExists(t, filepath.Join(dir, ".cagent"))

	data, err := os.ReadFile(homeHistory)
	require.NoError(t, err)
	assert.Equal(t, "\"home prompt\"\n", string(data))
	data, err = os.ReadFile(homeLegacy)
	require.NoError(t, err)
	assert.JSONEq(t, `{"messages":["legacy home prompt"]}`, string(data))
}

func TestNew_MigratesOldFormatInDataDir(t *testing.T) {
	dir := t.TempDir()
	paths.SetDataDir(dir)
	t.Cleanup(func() { paths.SetDataDir("") })

	oldPath := filepath.Join(dir, "history.json")
	require.NoError(t, os.WriteFile(oldPath, []byte(`{"messages":["legacy prompt"]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "history"), []byte("\"existing prompt\"\n"), 0o600))

	h, err := New("")
	require.NoError(t, err)
	require.NoError(t, h.Load())
	assert.Equal(t, []string{"existing prompt", "legacy prompt"}, h.Messages)
	assert.NoFileExists(t, oldPath)

	reloaded, err := New("")
	require.NoError(t, err)
	require.NoError(t, reloaded.Load())
	assert.Equal(t, h.Messages, reloaded.Messages)
}

func TestNew_ExplicitDirectoryWins(t *testing.T) {
	dataDir := t.TempDir()
	paths.SetDataDir(dataDir)
	t.Cleanup(func() { paths.SetDataDir("") })

	for _, tt := range []struct {
		name string
		new  func(string) (*History, error)
		sub  string
	}{
		{name: "New", new: New, sub: ".cagent"},
		{name: "NewAtDir", new: NewAtDir},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			h, err := tt.new(dir)
			require.NoError(t, err)
			require.NoError(t, h.Add("explicit prompt"))
			assert.FileExists(t, filepath.Join(dir, tt.sub, "history"))

			reloaded, err := tt.new(dir)
			require.NoError(t, err)
			require.NoError(t, reloaded.Load())
			assert.Equal(t, []string{"explicit prompt"}, reloaded.Messages)
			assert.NoFileExists(t, filepath.Join(dataDir, "history"))
		})
	}
}

func TestHistory_AddAndSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
	require.NoError(t, err)

	messages := []string{"first", "second", "third"}
	for _, msg := range messages {
		err := h.Add(msg)
		require.NoError(t, err)
	}

	assert.Equal(t, messages, h.Messages)
	assert.Len(t, messages, h.current)

	h2, err := New("")
	require.NoError(t, err)
	require.NoError(t, h2.Load())
	assert.Equal(t, messages, h2.Messages)
}

func TestHistory_Navigation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
	require.NoError(t, err)

	assert.Empty(t, h.Previous())
	assert.Empty(t, h.Next())

	require.NoError(t, h.Add("only"))

	assert.Equal(t, "only", h.Previous())
	assert.Equal(t, "only", h.Previous()) // Should stay at the beginning
	assert.Empty(t, h.Next())             // Should return empty when going past the end
}

func TestHistory_StayAtTheBeginning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))

	assert.Equal(t, "first", h.Previous())
	assert.Equal(t, "first", h.Previous())
}

func TestHistory_NoDuplicateMessages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))
	require.NoError(t, h.Add("second"))
	require.NoError(t, h.Add("second"))

	assert.Equal(t, "second", h.Previous())
	assert.Equal(t, "first", h.Previous())
	assert.Equal(t, "first", h.Previous())
}

func TestHistory_MoveDuplicateLast(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	h, err := New("")
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
	t.Parallel()
	tmpDir := t.TempDir()

	h, err := New(tmpDir)
	require.NoError(t, err)

	multiline := "line1\nline2\nline3"
	require.NoError(t, h.Add(multiline))

	h2, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h2.Load())

	require.Len(t, h2.Messages, 1)
	assert.Equal(t, multiline, h2.Messages[0])
}

func TestHistory_MigrateOldFormat(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	err := os.MkdirAll(filepath.Join(tmpDir, ".cagent"), 0o755)
	require.NoError(t, err)
	oldHistFile := filepath.Join(tmpDir, ".cagent", "history.json")
	newHistFile := filepath.Join(tmpDir, ".cagent", "history")

	require.NoError(t, os.WriteFile(oldHistFile, []byte(`{"messages":["old1","old2","old3"]}`), 0o644))

	h, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h.Load())
	assert.Equal(t, []string{"old1", "old2", "old3"}, h.Messages)

	_, err = os.Stat(oldHistFile)
	assert.True(t, os.IsNotExist(err), "old history.json should be removed")

	_, err = os.Stat(newHistFile)
	assert.NoError(t, err, "new history file should exist")
}

func TestHistory_LatestMatch(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	h, err := New(tmpDir)
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
		h, err := New(tmpDir)
		require.NoError(t, err)

		msg, idx, ok := h.FindPrevContains("test", len(h.Messages))
		assert.False(t, ok)
		assert.Empty(t, msg)
		assert.Equal(t, -1, idx)
	})

	t.Run("empty query matches latest", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
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
		h, err := New(tmpDir)
		require.NoError(t, err)

		_, _, ok := h.FindNextContains("test", -1)
		assert.False(t, ok)
	})

	t.Run("case insensitive", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		h, err := New(tmpDir)
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
	h, err := New(tmpDir)
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))
	require.NoError(t, h.Add("second"))
	require.NoError(t, h.Add("third"))

	h.SetCurrent(1)
	assert.Equal(t, "first", h.Previous())

	h.SetCurrent(2)
	assert.Empty(t, h.Next())
}

func TestHistory_SetCurrentOutOfRange(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	h, err := New(tmpDir)
	require.NoError(t, err)

	require.NoError(t, h.Add("first"))
	require.NoError(t, h.Add("second"))

	// A negative value clamps to 0; Previous returns the oldest entry.
	h.SetCurrent(-5)
	assert.Equal(t, "first", h.Previous())

	// A value past the end clamps to len(Messages); Next returns empty.
	h.SetCurrent(100)
	assert.Empty(t, h.Next())

	// And from the clamped position, Previous returns the latest entry.
	h.SetCurrent(100)
	assert.Equal(t, "second", h.Previous())

	// On empty history, SetCurrent never causes a panic.
	empty, err := New(t.TempDir())
	require.NoError(t, err)
	empty.SetCurrent(-5)
	assert.Empty(t, empty.Previous())
	empty.SetCurrent(100)
	assert.Empty(t, empty.Next())
}

func TestHistory_VeryLongMessage(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	h, err := New(tmpDir)
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
	h2, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h2.Load())

	require.Len(t, h2.Messages, 2)
	assert.Equal(t, longStr, h2.Messages[0])
	assert.Equal(t, "short message after", h2.Messages[1])
}

func TestHistory_RedactsOnAdd(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	h, err := New(tmpDir)
	require.NoError(t, err)

	pat := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	msg := "deploy with token " + pat
	require.NoError(t, h.Add(msg))

	require.Len(t, h.Messages, 1)
	stored := h.Messages[0]
	assert.NotContains(t, stored, pat, "in-memory history must not contain the secret")
	assert.Contains(t, stored, portcullis.Marker)

	// On-disk file must also be redacted.
	data, err := os.ReadFile(filepath.Join(tmpDir, ".cagent", "history"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), pat, "persisted history must not contain the secret")
	assert.Contains(t, string(data), portcullis.Marker)
}

func TestHistory_RedactsOnLoad(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".cagent"), 0o700))
	histFile := filepath.Join(tmpDir, ".cagent", "history")

	pat := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	// Simulate a pre-existing history file written before redaction was wired in.
	require.NoError(t, os.WriteFile(histFile, []byte(`"deploy with token `+pat+"\"\n"), 0o600))

	h, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h.Load())

	require.Len(t, h.Messages, 1)
	assert.NotContains(t, h.Messages[0], pat, "loaded history must not expose the secret in memory")
	assert.Contains(t, h.Messages[0], portcullis.Marker)
}

func TestHistory_RedactsOnMigrate(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".cagent"), 0o700))
	oldHistFile := filepath.Join(tmpDir, ".cagent", "history.json")

	pat := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	require.NoError(t, os.WriteFile(oldHistFile, []byte(`{"messages":["leak `+pat+`"]}`), 0o600))

	h, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h.Load())

	require.Len(t, h.Messages, 1)
	assert.NotContains(t, h.Messages[0], pat, "migrated history must not expose the secret")
	assert.Contains(t, h.Messages[0], portcullis.Marker)

	data, err := os.ReadFile(filepath.Join(tmpDir, ".cagent", "history"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), pat, "migrated on-disk history must not contain the secret")
}

func TestNewAtDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "gordon")

	h, err := NewAtDir(dir)
	require.NoError(t, err)
	require.NoError(t, h.Add("hello"))

	// The history lands directly at dir/history, no ".cagent" segment.
	_, err = os.Stat(filepath.Join(dir, "history"))
	require.NoError(t, err)

	reloaded, err := NewAtDir(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"hello"}, reloaded.Messages)
}

func seedHistory(t *testing.T, baseDir string, count int) []string {
	t.Helper()

	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	messages := make([]string, count)
	for i := range count {
		messages[i] = fmt.Sprintf("prompt %d", i)
		require.NoError(t, encoder.Encode(messages[i]))
	}
	dir := filepath.Join(baseDir, ".cagent")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "history"), data.Bytes(), 0o600))
	return messages
}

// Each case is the first call on a lazy history, with and without a
// background Load racing it: it must see the loaded history either way.
func TestNew_LoadsOnFirstUse(t *testing.T) {
	t.Parallel()

	const count = 500
	last := fmt.Sprintf("prompt %d", count-1)
	for _, tt := range []struct {
		name string
		call func(h *History) any
		want any
	}{
		{"Len", func(h *History) any { return h.Len() }, count},
		{"Previous", func(h *History) any { return h.Previous() }, last},
		{"Next", func(h *History) any { return []any{h.Next(), len(h.Messages)} }, []any{"", count}},
		{"LatestMatch", func(h *History) any { return h.LatestMatch("prompt 49") }, last},
		{"FindPrevContains", func(h *History) any { msg, _, _ := h.FindPrevContains("prompt", count); return msg }, last},
		{"FindNextContains", func(h *History) any { msg, _, _ := h.FindNextContains("prompt", -1); return msg }, "prompt 0"},
		{"SetCurrent", func(h *History) any { h.SetCurrent(1); return h.Previous() }, "prompt 0"},
		{"Add", func(h *History) any { _ = h.Add("new prompt"); return h.Len() }, count + 1},
	} {
		for _, background := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/background=%t", tt.name, background), func(t *testing.T) {
				t.Parallel()
				tmpDir := t.TempDir()
				seedHistory(t, tmpDir, count)

				h, err := New(tmpDir)
				require.NoError(t, err)
				assert.Empty(t, h.Messages, "the constructor must not read the history file")

				var wg sync.WaitGroup
				defer wg.Wait()
				if background {
					wg.Go(func() { assert.NoError(t, h.Load()) })
				}
				assert.Equal(t, tt.want, tt.call(h))
			})
		}
	}
}

func TestNew_AddKeepsLoadedHistory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	want := append(seedHistory(t, tmpDir, 500), "new prompt")

	h, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h.Add("new prompt"))
	assert.Equal(t, want, h.Messages)

	reloaded, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, reloaded.Load())
	assert.Equal(t, want, reloaded.Messages)
}

func TestNew_MissingFile(t *testing.T) {
	t.Parallel()

	h, err := New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, h.Load())
	assert.Zero(t, h.Len())
	assert.Empty(t, h.Previous())

	require.NoError(t, h.Add("first"))
	assert.Equal(t, "first", h.Previous())
}

func TestNew_UnreadableFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".cagent", "history"), 0o700))

	h, err := New(tmpDir)
	require.NoError(t, err)
	assert.Zero(t, h.Len())
	err = h.Load()
	require.Error(t, err)
	require.ErrorIs(t, h.Load(), err, "later calls report the same failure")

	_, err = NewAtDir(filepath.Join(tmpDir, ".cagent"))
	require.Error(t, err, "NewAtDir must still report read errors eagerly")
}

func TestHistory_LoadOnlyOnce(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	messages := seedHistory(t, tmpDir, 3)

	h, err := New(tmpDir)
	require.NoError(t, err)
	require.NoError(t, h.Load())
	eager, err := NewAtDir(filepath.Join(tmpDir, ".cagent"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(tmpDir, ".cagent", "history")))

	for _, hist := range []*History{h, eager} {
		hist.SetCurrent(1)
		require.NoError(t, hist.Load())
		assert.Equal(t, messages, hist.Messages)
		assert.Equal(t, "prompt 0", hist.Previous(), "Load must not reset the cursor")
	}
}

func TestNew_LegacyImport(t *testing.T) {
	t.Parallel()

	t.Run("imported by the constructor", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".cagent"), 0o700))
		legacy := filepath.Join(tmpDir, ".cagent", "history.json")
		require.NoError(t, os.WriteFile(legacy, []byte(`{"messages":["legacy prompt"]}`), 0o600))

		h, err := New(tmpDir)
		require.NoError(t, err)
		assert.NoFileExists(t, legacy)
		assert.Equal(t, "legacy prompt", h.Previous())
	})

	t.Run("failure is returned", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".cagent"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".cagent", "history.json"), []byte("not json"), 0o600))

		_, err := New(tmpDir)
		require.Error(t, err)
	})
}

func TestHistory_Load(t *testing.T) {
	t.Parallel()

	pat := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	otherPAT := portcullistest.FakeGitHubPAT("dxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	for _, tt := range []struct {
		name string
		data string
		want []string
	}{
		{name: "empty"},
		{name: "invalid only", data: "\ninvalid\n{}\n42\ntrue\n\"unterminated"},
		{name: "latest occurrence wins", data: "\"a\"\n\"b\"\n\"c\"\n\"a\"\n\"b\"\n", want: []string{"c", "a", "b"}},
		{name: "invalid between duplicates", data: "\"a\"\nbroken\n\"b\"\n\"a\"", want: []string{"b", "a"}},
		{name: "empty strings and null", data: "\"\"\n\"a\"\nnull\n", want: []string{"a", ""}},
		{name: "equivalent JSON strings", data: "\"a\"\n\"b\"\n\"\\u0061\"\n", want: []string{"b", "a"}},
		{name: "whitespace and CRLF", data: "\"a\"\r\n \"b\" \r\n\r\n\"a\"\r\n", want: []string{"b", "a"}},
		{name: "multiline message", data: "\"a\\nb\"\n\"c\"\n\"a\\nb\"", want: []string{"c", "a\nb"}},
		{
			name: "redacted collisions",
			data: fmt.Sprintf("%q\n%q\n%q\n%q\n%q\n", "token "+pat, "token "+portcullis.Marker, "keep", "token "+otherPAT, "token "+pat),
			want: []string{"keep", "token " + portcullis.Marker},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "history")
			require.NoError(t, os.WriteFile(path, []byte(tt.data), 0o600))

			h, err := NewAtDir(dir)
			require.NoError(t, err)
			assert.Equal(t, tt.want, h.Messages)
			assert.Equal(t, len(tt.want), h.current)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.data, string(data), "loading must not rewrite the history file")

			for _, want := range slices.Backward(tt.want) {
				assert.Equal(t, want, h.Previous())
			}
			require.NoError(t, h.Add("after load"))
			assert.Equal(t, "after load", h.Previous())
		})
	}
}

func TestRedactMessages(t *testing.T) {
	t.Parallel()

	for _, count := range []int{0, 1, 2, runtime.NumCPU() + 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			var messages, want []string
			for i := range count {
				messages = append(messages, strconv.Itoa(i))
				want = append(want, "redacted "+strconv.Itoa(i))
			}
			calls := make([]atomic.Int32, count)
			redactMessages(messages, func(msg string) string {
				i, err := strconv.Atoi(msg)
				if err != nil {
					t.Errorf("invalid message index %q: %v", msg, err)
					return ""
				}
				calls[i].Add(1)
				return "redacted " + msg
			})
			assert.Equal(t, want, messages)
			for i := range calls {
				assert.Equal(t, int32(1), calls[i].Load())
			}
		})
	}
}

func TestRedactMessagesConcurrentWorkers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		workers := runtime.NumCPU()
		messages := make([]string, workers*2+1)
		for i := range messages {
			messages[i] = strconv.Itoa(i)
		}
		entered := make(chan string, len(messages))
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			redactMessages(messages, func(msg string) string {
				entered <- msg
				<-release
				return "redacted " + msg
			})
			close(done)
		}()

		synctest.Wait()
		assert.Len(t, entered, workers)
		select {
		case <-done:
			t.Error("redaction returned before workers finished")
		default:
		}
		close(release)
		<-done
		assert.Len(t, entered, len(messages))
		for i, msg := range messages {
			assert.Equal(t, "redacted "+strconv.Itoa(i), msg)
		}
	})
}

func TestHistory_ConcurrentRedactionOrder(t *testing.T) {
	t.Parallel()

	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	expected := &History{}
	count := runtime.NumCPU()*4 + 1
	pat := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	otherPAT := portcullistest.FakeGitHubPAT("dxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	for i := range count {
		for _, msg := range []string{
			fmt.Sprintf("message %d %s", i, strings.Repeat("context ", (i%7)*100)),
			"token " + pat,
			fmt.Sprintf("middle %d", i),
			"token " + otherPAT,
		} {
			require.NoError(t, encoder.Encode(msg))
			expected.addInMemory(portcullis.Redact(msg))
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	require.NoError(t, os.WriteFile(path, data.Bytes(), 0o600))
	for range 5 {
		h, err := NewAtDir(dir)
		require.NoError(t, err)
		assert.Equal(t, expected.Messages, h.Messages)
		assert.Equal(t, len(expected.Messages), h.current)
	}
	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data.Bytes(), persisted)
}

func BenchmarkHistoryLoad(b *testing.B) {
	for _, unique := range []int{1, 8000, 20000} {
		b.Run(fmt.Sprintf("unique=%d", unique), func(b *testing.B) {
			var data bytes.Buffer
			encoder := json.NewEncoder(&data)
			for i := range 20000 {
				msg := fmt.Sprintf("Review change %d: %s", i%unique, strings.Repeat("check tests and error handling; ", 5))
				require.NoError(b, encoder.Encode(msg))
			}
			dir := b.TempDir()
			require.NoError(b, os.WriteFile(filepath.Join(dir, "history"), data.Bytes(), 0o600))
			b.ReportAllocs()
			b.SetBytes(int64(data.Len()))
			b.ResetTimer()
			for b.Loop() {
				h, err := NewAtDir(dir)
				require.NoError(b, err)
				require.Len(b, h.Messages, unique)
			}
		})
	}
}
