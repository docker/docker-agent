package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWarningForSessionIsSessionScoped(t *testing.T) {
	t.Parallel()

	tagged, ok := WarningForSession("child-session", "msg", "worker").(*WarningEvent)
	require.True(t, ok)
	var scoped SessionScoped = tagged
	assert.Equal(t, "child-session", scoped.GetSessionID())

	untagged, ok := Warning("msg", "root").(*WarningEvent)
	require.True(t, ok)
	assert.Empty(t, untagged.GetSessionID(), "untagged warnings are root-scoped")

	taggedJSON, err := json.Marshal(tagged)
	require.NoError(t, err)
	assert.Contains(t, string(taggedJSON), `"session_id":"child-session"`)

	untaggedJSON, err := json.Marshal(untagged)
	require.NoError(t, err)
	assert.NotContains(t, string(untaggedJSON), "session_id", "wire format of untagged warnings is unchanged")
}
