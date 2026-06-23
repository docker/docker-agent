package root

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

func TestIsAskpassInvocation(t *testing.T) {
	t.Parallel()

	assert.True(t, isAskpassInvocation([]string{shell.AskpassCommandName}))
	assert.True(t, isAskpassInvocation([]string{shell.AskpassCommandName, "--", "[sudo] password:"}))
	assert.False(t, isAskpassInvocation(nil))
	assert.False(t, isAskpassInvocation([]string{"run"}))
	assert.False(t, isAskpassInvocation([]string{"run", shell.AskpassCommandName}))
}

func TestAskpassIsManagementInvocation(t *testing.T) {
	t.Parallel()

	// __askpass must skip self-update (it is invoked by sudo mid-command).
	assert.True(t, isManagementInvocation([]string{shell.AskpassCommandName, "--", "p"}))
}
