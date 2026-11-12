package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRunEmptyTurnWarning(t *testing.T) {
	t.Parallel()

	const message = "Model anthropic/claude-fable-5-2 stopped without a final reply after using tools (stop reason: stop). Send a follow-up to continue if the task is unfinished."
	for _, outputJSON := range []bool{false, true} {
		name := "text"
		if outputJSON {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rt := &mockRuntime{events: []runtime.Event{runtime.Warning(message, "root")}}
			var output bytes.Buffer
			err := Run(t.Context(), NewPrinter(&output), Config{OutputJSON: outputJSON}, rt, session.New(), []string{"Investigate"})
			require.NoError(t, err)
			if !outputJSON {
				assert.Contains(t, output.String(), message)
				return
			}
			var event runtime.WarningEvent
			require.NoError(t, json.NewDecoder(strings.NewReader(output.String())).Decode(&event))
			assert.Equal(t, "warning", event.Type)
			assert.Equal(t, message, event.Message)
			assert.Equal(t, "root", event.AgentName)
		})
	}
}
