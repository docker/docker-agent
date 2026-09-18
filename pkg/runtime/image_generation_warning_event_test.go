package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageGenerationWarningRemoteEventPayload(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal(Warning(missingGeneratedImageWarning, "root"))
	require.NoError(t, err)

	var event map[string]any
	require.NoError(t, json.Unmarshal(payload, &event))
	assert.Equal(t, "warning", event["type"])
	assert.Equal(t, missingGeneratedImageWarning, event["message"])
	assert.Equal(t, "root", event["agent_name"])
	assert.NotEmpty(t, event["timestamp"])
}
