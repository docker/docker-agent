package board

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestShQuote(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "'plain'", shQuote("plain"))
	assert.Equal(t, `'it'\''s'`, shQuote("it's"))
	assert.Equal(t, "'two words'", shQuote("two words"))
}

func TestAgentCommand(t *testing.T) {
	t.Parallel()

	// First run: creates the worktree from the base.
	cmd := (sessionLaunch{
		agent: "coder", agentSession: "sess1", listenSocket: "/tmp/a.sock",
		worktreeName: "board-abc", worktreeBase: "origin/main", prompt: "do the thing",
	}).agentCommand()
	assert.Contains(t, cmd, " run 'coder' --yolo ")
	assert.Contains(t, cmd, " --session 'sess1' --listen 'unix:///tmp/a.sock'")
	assert.Contains(t, cmd, "--worktree='board-abc' --worktree-base 'origin/main'")
	assert.True(t, strings.HasSuffix(cmd, " 'do the thing'"))

	// The board's config, data, and cache dirs are forwarded so the agent
	// resolves the same aliases and creates its worktree where the board
	// watches, even when the board runs with directory overrides (e.g. in a
	// sandbox whose $HOME differs from the mounted host directories).
	assert.Contains(t, cmd, " --config-dir "+shQuote(paths.GetConfigDir())+" ")
	assert.Contains(t, cmd, " --data-dir "+shQuote(paths.GetDataDir())+" ")
	assert.Contains(t, cmd, " --cache-dir "+shQuote(paths.GetCacheDir())+" ")

	// Resume: no worktree flags, no prompt.
	cmd = (sessionLaunch{agent: "coder", agentSession: "sess1", listenSocket: "/tmp/a.sock"}).agentCommand()
	assert.NotContains(t, cmd, "--worktree")
	assert.True(t, strings.HasSuffix(cmd, "--listen 'unix:///tmp/a.sock'"))
}

// TestAgentCommandForwardsDirOverrides covers the scenario the forwarding
// exists for: the board running with directory overrides. Not parallel: it
// mutates the process-global overrides, and restores them before returning
// — during the serial phase, before parallel tests resume.
func TestAgentCommandForwardsDirOverrides(t *testing.T) {
	configDir, dataDir, cacheDir := t.TempDir(), t.TempDir(), t.TempDir()
	paths.SetConfigDir(configDir)
	paths.SetDataDir(dataDir)
	paths.SetCacheDir(cacheDir)
	t.Cleanup(func() {
		paths.SetConfigDir("")
		paths.SetDataDir("")
		paths.SetCacheDir("")
	})

	cmd := (sessionLaunch{agent: "coder", agentSession: "sess1", listenSocket: "/tmp/a.sock"}).agentCommand()
	assert.Contains(t, cmd, " --config-dir "+shQuote(configDir)+" ")
	assert.Contains(t, cmd, " --data-dir "+shQuote(dataDir)+" ")
	assert.Contains(t, cmd, " --cache-dir "+shQuote(cacheDir)+" ")
}

func TestTmuxFormatEscape(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "plain title", tmuxFormatEscape("plain title"))
	// '#' introduces tmux format expansion: doubled to stay literal.
	assert.Equal(t, "fix ##123 · ##{pane_pid}", tmuxFormatEscape("fix #123 · #{pane_pid}"))
	// Control characters are dropped.
	assert.Equal(t, "ab", tmuxFormatEscape("a\x1b\nb\x7f"))
}
