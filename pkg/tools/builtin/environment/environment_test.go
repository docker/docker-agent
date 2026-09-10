package environment

import (
	"encoding/json"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestGetEnvironmentInfo_ShapeAndValues(t *testing.T) {
	t.Parallel()

	ts := New()
	res, err := ts.get(t.Context(), Args{})
	require.NoError(t, err)
	require.NotNil(t, res)

	var got Info
	require.NoError(t, json.Unmarshal([]byte(res.Output), &got))

	assert.NotEmpty(t, got.OS, "os must be populated")
	assert.NotEmpty(t, got.Shell, "shell must be populated")

	switch runtime.GOOS {
	case "windows":
		assert.Equal(t, "Windows", got.OS)
	case "darwin":
		assert.Equal(t, "macOS", got.OS)
	case "linux":
		assert.Equal(t, "Linux", got.OS)
	}
}

// TestGetEnvironmentInfo_ReadOnlyHint anchors the auto-approval contract:
// the tool's whole reason to exist as a *tool* (instead of a hook) is that
// it rides the ReadOnlyHint gate to skip user approval. Dropping the hint
// would silently reintroduce a modal per call.
func TestGetEnvironmentInfo_ReadOnlyHint(t *testing.T) {
	t.Parallel()

	toolList, err := New().Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 1)
	assert.True(t, toolList[0].Annotations.ReadOnlyHint,
		"get_environment_info must set ReadOnlyHint so the safety layer auto-approves it")
	assert.Equal(t, ToolNameGetEnvironmentInfo, toolList[0].Name)
}

// TestGetEnvironmentInfo_NoArguments guards the "no user-controlled inputs"
// promise the auto-approval relies on: adding fields to Args would let the
// model steer what the tool reads, which invalidates the safety argument.
func TestGetEnvironmentInfo_NoArguments(t *testing.T) {
	t.Parallel()

	toolList, err := New().Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 1)

	schema, err := json.Marshal(tools.MustSchemaFor[Args]())
	require.NoError(t, err)
	assert.NotContains(t, string(schema), `"properties":{`,
		"Args must remain a zero-field struct so the model cannot steer what the tool reads")
}
