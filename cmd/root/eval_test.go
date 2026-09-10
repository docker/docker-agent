package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/evaluation"
)

func TestEvalContainerRuntimeFlagDefaultsToDocker(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()

	flag := cmd.Flags().Lookup("container-runtime")
	require.NotNil(t, flag, "eval must expose --container-runtime")
	assert.Equal(t, "docker", flag.DefValue)
}

func TestEvalContainerRuntimeFlagAcceptsCustomExecutable(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--container-runtime", "podman"}))

	value, err := cmd.Flags().GetString("container-runtime")
	require.NoError(t, err)
	assert.Equal(t, "podman", value)
}

func TestEvalAgentImageFlagDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()

	flag := cmd.Flags().Lookup("agent-image")
	require.NotNil(t, flag, "eval must expose --agent-image")
	assert.Empty(t, flag.DefValue, "unset --agent-image must defer to the version-derived default")
}

func TestEvalAgentImageFlagAcceptsOverride(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--agent-image", "docker/docker-agent:1.2.3"}))

	value, err := cmd.Flags().GetString("agent-image")
	require.NoError(t, err)
	assert.Equal(t, "docker/docker-agent:1.2.3", value)
}

func TestEvalAgentImageFlagAcceptsNoneToSkipInjection(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--agent-image", "none"}))

	value, err := cmd.Flags().GetString("agent-image")
	require.NoError(t, err)
	assert.Equal(t, evaluation.NoAgentImage, value)
}
