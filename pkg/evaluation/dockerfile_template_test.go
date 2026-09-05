package evaluation

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderTemplate is a small helper to render the embedded Dockerfile
// templates with the same fields buildEvalImage populates.
func renderTemplate(t *testing.T, custom, copyWorkingDir bool, baseImage, agentImage string) string {
	t.Helper()

	var data struct {
		CopyWorkingDir bool
		BaseImage      string
		AgentImage     string
	}
	data.CopyWorkingDir = copyWorkingDir
	data.BaseImage = baseImage
	data.AgentImage = agentImage

	tmpl := dockerfileTemplate
	if custom {
		tmpl = dockerfileCustomTemplate
	}

	var buf bytes.Buffer
	require.NoError(t, tmpl.Execute(&buf, data))
	return buf.String()
}

// TestDockerfileCustomTemplateParity guards against the regression in
// https://github.com/docker/docker-agent/issues/796: the custom-base-image
// template must copy the docker-agent binary and wrap it with the /run.sh
// entrypoint, exactly like the default template. Without these, the eval
// container inherits the base image's `ENTRYPOINT ["/docker-agent"]` and the
// agent YAML path is passed as a bare subcommand, producing
// `unknown command "/configs/<agent>.yaml"`.
func TestDockerfileCustomTemplateParity(t *testing.T) {
	t.Parallel()

	out := renderTemplate(t, true /* custom */, true /* copyWorkingDir */, "python:3.12", "docker/docker-agent:1.2.3")

	assert.Contains(t, out, "FROM python:3.12",
		"custom template must use the provided base image")
	assert.Contains(t, out, "COPY --from=docker/docker-agent:1.2.3 /docker-agent /",
		"custom template must copy the docker-agent binary into the eval image")
	assert.Contains(t, out, `ENTRYPOINT ["/run.sh", "/docker-agent", "run", "--exec", "--yolo", "--json"]`,
		"custom template must set the /run.sh docker-agent run entrypoint")
	assert.Contains(t, out, "/run.sh",
		"custom template must create the /run.sh entrypoint wrapper")
}

// TestDockerfileTemplatesRender ensures both templates render with and without
// a working directory copy for the fields buildEvalImage supplies.
func TestDockerfileTemplatesRender(t *testing.T) {
	t.Parallel()

	for _, custom := range []bool{false, true} {
		for _, copyWorkingDir := range []bool{false, true} {
			out := renderTemplate(t, custom, copyWorkingDir, "alpine:latest", "docker/docker-agent:edge")
			assert.Contains(t, out, "ENTRYPOINT [")
			if copyWorkingDir {
				assert.Contains(t, out, "COPY . ./")
			} else {
				assert.NotContains(t, out, "COPY . ./")
			}
		}
	}
}

// TestDockerfileTemplatesSkipInjectionWhenAgentImageEmpty covers the "none"
// --agent-image mode (resolved to an empty AgentImage), where the eval image
// must not copy in a docker-agent binary at all and instead trusts whatever
// /docker-agent is already present in the base image.
func TestDockerfileTemplatesSkipInjectionWhenAgentImageEmpty(t *testing.T) {
	t.Parallel()

	for _, custom := range []bool{false, true} {
		out := renderTemplate(t, custom, false, "alpine:latest", "")
		assert.NotContains(t, out, "COPY --from=",
			"no docker-agent binary should be injected when AgentImage is empty")
		assert.Contains(t, out, `ENTRYPOINT ["/run.sh", "/docker-agent", "run", "--exec", "--yolo", "--json"]`,
			"entrypoint must still run whatever /docker-agent the base image provides")
	}
}
