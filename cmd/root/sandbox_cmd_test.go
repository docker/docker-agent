package root

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/testutil"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// TestSandboxAllowDenyList exercises the sandbox subcommand group end
// to end against a temp HOME, verifying that allow / list / deny
// round-trip through the user config.
func TestSandboxAllowDenyList(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)

	root := NewRootCmd()
	root.SetContext(t.Context())
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	root.SetArgs([]string{"sandbox", "allow", "api.example.com", "registry.npmjs.org:443"})
	require.NoError(t, root.Execute())
	assert.Contains(t, stdout.String(), "+ api.example.com")
	assert.Contains(t, stdout.String(), "+ registry.npmjs.org:443")

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"api.example.com", "registry.npmjs.org:443"}, cfg.SandboxAllowlist)

	stdout.Reset()
	root.SetArgs([]string{"sandbox", "list"})
	require.NoError(t, root.Execute())
	assert.Contains(t, stdout.String(), "api.example.com")
	assert.Contains(t, stdout.String(), "registry.npmjs.org:443")

	stdout.Reset()
	root.SetArgs([]string{"sandbox", "deny", "api.example.com"})
	require.NoError(t, root.Execute())
	assert.Contains(t, stdout.String(), "Removed api.example.com")

	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"registry.npmjs.org:443"}, cfg.SandboxAllowlist)

	// Denying a host that isn't on the list is idempotent: it prints a
	// notice and exits successfully so scripts don't have to pre-check.
	stdout.Reset()
	root.SetArgs([]string{"sandbox", "deny", "not.on.list.example.com"})
	require.NoError(t, root.Execute())
	assert.Contains(t, stdout.String(), "is not on the persistent sandbox allowlist")
}
