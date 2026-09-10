package cli

import (
	"bytes"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestImageGenerationWarningPreservesTextAndPrintsAfterIt(t *testing.T) {
	t.Parallel()

	const warning = "The model returned text but no image for this image-generation request. Try rephrasing the request."
	rt := &mockRuntime{
		events: []runtime.Event{
			runtime.AgentChoice("test", "sess", "Here's an image of Docker and its friends."),
			runtime.Warning(warning, "test"),
		},
	}

	var buf bytes.Buffer
	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, session.New(), []string{"draw an image"})
	assert.NilError(t, err)

	output := buf.String()
	assert.Check(t, strings.Contains(output, "Here's an image of Docker and its friends."))
	assert.Check(t, strings.Contains(output, warning))
	assert.Check(t, strings.Index(output, "Here's an image") < strings.Index(output, warning))
}
