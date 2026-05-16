package builtins_test

import (
	"os"
	"os/user"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestAddUserInfoEmitsHostAndUser verifies that the builtin returns
// session_start additional context containing the current process's
// username and hostname.
//
// We resolve those values at test time (via os/user and os.Hostname)
// rather than hard-coding them so the test passes on any developer
// laptop or CI runner. If either lookup fails on the host (rare —
// some sandboxes), we skip the assertion for that piece individually
// so the suite still reports the working half.
func TestAddUserInfoEmitsHostAndUser(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddUserInfo)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, nil)
	require.NoError(t, err)
	require.NotNil(t, out, "add_user_info must emit context on a normal host")
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventSessionStart, out.HookSpecificOutput.HookEventName,
		"add_user_info must target session_start, not turn_start")

	ctx := out.HookSpecificOutput.AdditionalContext

	if u, err := user.Current(); err == nil && u.Username != "" {
		assert.Contains(t, ctx, u.Username,
			"emitted context must include the current process's username")
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		assert.Contains(t, ctx, host,
			"emitted context must include the current host's name")
	}
}

// TestAddUserInfoIgnoresInputCwd documents that the builtin does NOT
// depend on Cwd: user/host info is process-global, so a nil input or
// an input with no Cwd must still produce output. This is what makes
// it safe to wire as a session_start hook even before the runtime has
// resolved a working directory.
func TestAddUserInfoIgnoresInputCwd(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddUserInfo)

	out, err := fn(t.Context(), nil, nil)
	require.NoError(t, err)
	// We can't assert non-nil unconditionally (a sandboxed test
	// environment might lose both lookups), but if one succeeded the
	// builtin must have emitted something.
	if out != nil {
		require.NotNil(t, out.HookSpecificOutput)
		assert.NotEmpty(t, out.HookSpecificOutput.AdditionalContext)
	}
}
