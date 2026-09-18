package sidebar

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestNewIgnoresProcessGitState pins the TestMain seam: a new sidebar shows
// the injected directory with no branch and no watcher, whatever repository
// the test binary happens to run in.
func TestNewIgnoresProcessGitState(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sb := New(animation.NewRuntime(), t.Context(), service.NewSessionState(sess))
	m := sb.(*model)

	// The display path is normalized to the OS separator.
	assert.Equal(t, filepath.FromSlash("/work/app"), m.workingDirectory)
	assert.Empty(t, m.gitBranchName)
	require.Nil(t, m.gitBranchWatcher)
	assert.Nil(t, m.Init(), "no watcher means nothing to wait for")
}
