package evaluation

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

func TestBuildEvalImagePinsLocalAgentDigest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake runtime is a POSIX shell script")
	}
	const script = "#!/bin/sh\nd=${0%/*}\n" +
		"if [ \"$1\" = image ]; then\n" +
		" printf '%s\\n' \"$@\" > \"$d/inspect-args\"\n" +
		" if [ -f \"$d/inspect-fail\" ]; then exit 1; fi\n" +
		" cat \"$d/inspect-output\"\n" +
		"else\n cat > \"$d/Dockerfile\"\n printf '%s\\n' sha256:eval-image\nfi\n"
	t.Parallel()

	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, image, inspect, want string
		failedInspect              bool
	}{
		{"local manifest", "docker/docker-agent:edge", `["docker/docker-agent@` + digest + `"]`, "docker/docker-agent@" + digest, false},
		{"normalized repository", "docker/docker-agent:edge", `["docker.io/docker/docker-agent@` + digest + `"]`, "docker.io/docker/docker-agent@" + digest, false},
		{"unrelated first digest", "docker/docker-agent:edge", `["other/agent@` + digest + `","docker/docker-agent@` + digest + `"]`, "docker/docker-agent@" + digest, false},
		{"explicit digest", "docker/docker-agent@" + digest, "", "docker/docker-agent@" + digest, false},
		{"skip injection", NoAgentImage, "", "", false},
		{"missing local image", "docker/docker-agent:edge", "", "docker/docker-agent:edge", true},
		{"null inspect", "docker/docker-agent:edge", "null", "docker/docker-agent:edge", false},
		{"invalid then valid digest", "docker/docker-agent:edge", `["docker/docker-agent@bad","docker/docker-agent@` + digest + `"]`, "docker/docker-agent@" + digest, false},
		{"default image", "", `["` + strings.Split(DefaultAgentImage(), ":")[0] + `@` + digest + `"]`, strings.Split(DefaultAgentImage(), ":")[0] + "@" + digest, false},
		{"no manifest digest", "docker/docker-agent:edge", "[]", "docker/docker-agent:edge", false},
		{"invalid inspect output", "docker/docker-agent:edge", "invalid", "docker/docker-agent:edge", false},
		{"different repository", "docker/docker-agent:edge", `["other/agent@` + digest + `"]`, "docker/docker-agent:edge", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "inspect-args")
			dockerfile := filepath.Join(dir, "Dockerfile")
			fakeRuntime := filepath.Join(dir, "runtime")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "inspect-output"), []byte(tc.inspect+"\n"), 0o644))
			if tc.failedInspect {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "inspect-fail"), nil, 0o644))
			}
			writeMockCLI(t, fakeRuntime, script)
			runner := newRunner(config.NewFileSource(filepath.Join(dir, "agent.yaml")), nil,
				Config{EvalsDir: dir, ContainerRuntime: fakeRuntime, AgentImage: tc.image})
			image, err := runner.buildEvalImage(t.Context(), &session.EvalCriteria{})
			require.NoError(t, err)
			assert.Equal(t, "sha256:eval-image", image)
			content, err := os.ReadFile(dockerfile)
			require.NoError(t, err)
			if tc.want == "" {
				assert.NotContains(t, string(content), "COPY --from=")
			} else {
				assert.Contains(t, string(content), "COPY --from="+tc.want+" /docker-agent /")
			}
			if tc.image == NoAgentImage || strings.Contains(tc.image, "@") {
				_, err := os.Stat(argsFile)
				assert.True(t, os.IsNotExist(err), "skip inspect for pinned or disabled injection")
			} else {
				args, err := os.ReadFile(argsFile)
				require.NoError(t, err)
				expected := tc.image
				if expected == "" {
					expected = DefaultAgentImage()
				}
				assert.Equal(t, "image\ninspect\n--format\n{{json .RepoDigests}}\n"+expected+"\n", string(args))
			}
		})
	}
}
