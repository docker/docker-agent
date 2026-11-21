//go:build !windows

package root

import (
	"os"
	"testing"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBannerAnimationCapability(t *testing.T) {
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	for _, name := range []string{"DOCKER_AGENT_REDUCE_MOTION", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "TTY_FORCE", "TMUX"} {
		t.Setenv(name, "")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	assert.True(t, bannerAnimationSupported(slave))
	assert.False(t, bannerAnimationSupported(nil))
	f, err := os.CreateTemp(t.TempDir(), "output")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	t.Setenv("TTY_FORCE", "1")
	assert.False(t, bannerAnimationSupported(f), "forced output is not a tty")
	t.Setenv("TTY_FORCE", "")
	for _, tc := range []struct{ key, value string }{
		{"DOCKER_AGENT_REDUCE_MOTION", "1"}, {"NO_COLOR", "anything"}, {"CLICOLOR", "0"}, {"TERM", "dumb"},
	} {
		old := os.Getenv(tc.key)
		t.Setenv(tc.key, tc.value)
		assert.False(t, bannerAnimationSupported(slave), tc.key)
		t.Setenv(tc.key, old)
	}
	t.Setenv("COLORTERM", "")
	t.Setenv("TERM", "vt100")
	assert.False(t, bannerAnimationSupported(slave), "limited color")
}
