package e2e_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func TestRuntime_OpenAI_Basic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	agentSource, err := config.Resolve("testdata/basic.yaml", nil)
	require.NoError(t, err)

	_, runConfig := startRecordingAIProxy(t)
	team, err := teamloader.Load(ctx, agentSource, runConfig)
	require.NoError(t, err)

	rt, err := runtime.New(team)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("What's 2+2?"))
	_, err = rt.Run(ctx, sess)
	require.NoError(t, err)

	response := sess.GetLastAssistantMessageContent()
	assert.Equal(t, "2 + 2 equals 4.", response)
	// Title generation is now handled by pkg/app or pkg/server, not the runtime
}

// TestRuntime_MultiAgent_SessionReload verifies that a multi-agent
// task transfer does not corrupt the parent session's persisted
// history. Before the fix (PR #2058), sub-agent streaming events
// (AgentChoiceEvent, AgentChoiceReasoningEvent) were processed by the
// PersistentRuntime against the parent session's store, creating orphan
// assistant messages. On session reload and follow-up, these orphan messages
// corrupt the message sequence sent to the model, causing API errors.
//
// The test:
//  1. Runs a first turn where the root agent delegates via transfer_task
//  2. Persists the session to a SQLite store
//  3. Reloads the session from the store
//  4. Runs a second turn (follow-up) on the reloaded session
//  5. Asserts the follow-up succeeds without errors
func TestRuntime_MultiAgent_SessionReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	agentSource, err := config.Resolve("testdata/multi_transfer.yaml", nil)
	require.NoError(t, err)

	_, runConfig := startRecordingAIProxy(t)
	team, err := teamloader.Load(ctx, agentSource, runConfig)
	require.NoError(t, err)

	// Use a SQLite store so we test real persistence and reload.
	dbPath := filepath.Join(t.TempDir(), "session.db")
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	rt, err := runtime.New(team, runtime.WithSessionStore(store))
	require.NoError(t, err)

	// --- Turn 1: trigger a task transfer ---
	sess := session.New(session.WithUserMessage("What's the weather in Paris? Delegate to the weather agent."))
	_, err = rt.Run(ctx, sess)
	require.NoError(t, err)

	response := sess.GetLastAssistantMessageContent()
	require.NotEmpty(t, response, "first turn should produce a response")

	// --- Reload the session from the store ---
	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded)

	// --- Turn 2: follow-up on the reloaded session ---
	// Before the fix, this would fail because the reloaded session contained
	// orphan streaming messages from the sub-agent that corrupt the message
	// sequence sent to the model.
	reloaded.AddMessage(session.UserMessage("Can you summarize what you found?"))
	reloaded.SendUserMessage = true

	_, err = rt.Run(ctx, reloaded)
	require.NoError(t, err, "follow-up on reloaded session should not fail; "+
		"orphan sub-agent messages in the persisted parent session would cause "+
		"model API errors due to corrupted message sequence")

	response2 := reloaded.GetLastAssistantMessageContent()
	assert.NotEmpty(t, response2, "second turn should produce a response")
}

func TestRuntime_Mistral_Basic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	agentSource, err := config.Resolve("testdata/basic.yaml", nil)
	require.NoError(t, err)

	_, runConfig := startRecordingAIProxy(t)
	team, err := teamloader.Load(ctx, agentSource, runConfig, teamloader.WithModelOverrides([]string{"mistral/mistral-small"}))
	require.NoError(t, err)

	rt, err := runtime.New(team)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("What's 2+2?"))
	_, err = rt.Run(ctx, sess)
	require.NoError(t, err)

	response := sess.GetLastAssistantMessageContent()
	assert.Equal(t, "The sum of 2 + 2 is 4.", response)
	// Title generation is now handled by pkg/app or pkg/server, not the runtime
}
