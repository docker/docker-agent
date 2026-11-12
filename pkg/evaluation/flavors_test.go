package evaluation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestEvalContainerForwardsOrderedFlavors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake container runtime executable is a POSIX shell script")
	}
	const script = "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"${0%/*}/args\"\necho '{\"type\":\"agent_choice\",\"content\":\"ok\"}'\n"
	t.Parallel()

	for _, setup := range []string{"", "echo setup"} {
		t.Run(setup, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args")
			fake := filepath.Join(dir, "runtime")
			writeMockCLI(t, fake, script)
			runConfig := &config.RuntimeConfig{
				Config:              config.Config{Flavors: []string{"baseline", "fixtures,with spaces"}},
				EnvProviderForTests: environment.NewNoEnvProvider(),
			}
			runner := newRunner(config.NewFileSource(filepath.Join(dir, "agent.yaml")), runConfig,
				Config{ContainerRuntime: fake})
			_, err := runner.runDockerAgentInContainer(t.Context(), "image", []string{"question with $(shell) text", "--flavor"}, setup)
			require.NoError(t, err)
			data, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			assert.Contains(t, string(data), "--flavor\nbaseline\n--flavor\nfixtures,with spaces\n--\n/configs/agent.yaml\nquestion with $(shell) text\n--flavor\n")
		})
	}
}

func TestSavedEvalRecordsFlavors(t *testing.T) {
	t.Parallel()
	run := &EvalRun{Name: "flavors", Config: Config{Flavors: []string{"baseline", "fixtures"}}}
	path, err := SaveRunSessionsJSON(run, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var output RunOutput
	require.NoError(t, json.Unmarshal(data, &output))
	assert.Equal(t, run.Config.Flavors, output.Config.Flavors)
}
