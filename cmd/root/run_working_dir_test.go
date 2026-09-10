package root

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// Only an explicit --working-dir makes generic new-session actions in the
// TUI default to the initial session's directory; the value passed on is the
// session's effective directory (worktree/resume resolution included), not
// the raw flag (#4039).
func TestExplicitDefaultWorkingDir(t *testing.T) {
	t.Parallel()

	t.Run("no explicit flag keeps the picker", func(t *testing.T) {
		t.Parallel()
		f := &runExecFlags{}
		sess := session.New(session.WithWorkingDir("/repo"))
		assert.Empty(t, f.explicitDefaultWorkingDir(sess),
			"without --working-dir no default may be configured")
	})

	t.Run("explicit flag uses the session's effective directory", func(t *testing.T) {
		t.Parallel()
		f := &runExecFlags{workingDirChanged: true}
		sess := session.New(session.WithWorkingDir("/repo/worktree"))
		assert.Equal(t, "/repo/worktree", f.explicitDefaultWorkingDir(sess))
	})

	t.Run("explicit flag falls back to the process CWD", func(t *testing.T) {
		t.Parallel()
		f := &runExecFlags{workingDirChanged: true}
		wd, err := os.Getwd()
		require.NoError(t, err)
		assert.Equal(t, wd, f.explicitDefaultWorkingDir(session.New()),
			"an empty session working dir must mirror runTUIWrapped's CWD fallback")
	})
}
