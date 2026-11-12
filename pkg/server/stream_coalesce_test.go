package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// deltaScript is a turn whose deltas all arrive faster than any window: the
// first delta goes out at once, the rest are held until stream_stopped
// flushes them.
func deltaScript(sessionID string) []runtime.Event {
	return []runtime.Event{
		runtime.StreamStarted(sessionID, "root"),
		runtime.AgentChoice("root", sessionID, "a", "m1"),
		runtime.AgentChoice("root", sessionID, "b", "m1"),
		runtime.AgentChoice("root", sessionID, "c", "m1"),
		runtime.ToolCallOutput("call-1", tools.Tool{Name: "shell"}, "1", "root"),
		runtime.ToolCallOutput("call-1", tools.Tool{Name: "shell"}, "2", "root"),
		runtime.StreamStopped(sessionID, "root", "normal"),
	}
}

func TestRunAgent_CoalescesStreamingDeltas(t *testing.T) {
	t.Parallel()

	// An hour-long window makes the merge deterministic: nothing but
	// stream_stopped can flush the held deltas.
	frames := runAgentFrames(t, WithStreamCoalesceWindow(time.Hour))

	assert.Equal(t, []string{
		"stream_started",
		"agent_choice:a",
		"agent_choice:bc",
		"tool_call_output:12",
		"stream_stopped",
	}, frames)
}

func TestRunAgent_StreamCoalescingCanBeDisabled(t *testing.T) {
	t.Parallel()

	frames := runAgentFrames(t, WithStreamCoalesceWindow(0))

	assert.Equal(t, []string{
		"stream_started",
		"agent_choice:a",
		"agent_choice:b",
		"agent_choice:c",
		"tool_call_output:1",
		"tool_call_output:2",
		"stream_stopped",
	}, frames)
}

func TestRecallSession_CoalescesStreamingDeltasBeforeSequencing(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sess := session.New()
		sm := newTestSessionManager(t, sess, &scriptedStreamRuntime{events: deltaScript(sess.ID)})
		sm.streamCoalesceWindow = time.Hour

		require.NoError(t, sm.recallSession(t.Context(), sess.ID, runtime.QueuedMessage{Content: "wake up"}))
		waitSessionIdle(t, sm, sess.ID)

		seq, ok := sm.LastEventSeq(sess.ID)
		require.True(t, ok)
		require.Equal(t, uint64(5), seq, "merged deltas must take one sequence number each")

		replayed := replaySessionEvents(t, sm, sess.ID, 5)
		assert.Equal(t, "a", replayed[1].(*runtime.AgentChoiceEvent).Content)
		assert.Equal(t, "bc", replayed[2].(*runtime.AgentChoiceEvent).Content)
		assert.Equal(t, "12", replayed[3].(*runtime.ToolCallOutputEvent).Output)
		assert.IsType(t, &runtime.StreamStoppedEvent{}, replayed[4])
	})
}

// runAgentFrames runs one turn of deltaScript through POST
// /api/sessions/:id/agent/:agent and summarises each SSE frame as
// "type[:content]".
func runAgentFrames(t *testing.T, opts ...SessionManagerOpt) []string {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{}, opts...)
	sm.AttachRuntime(ctx, sess.ID, &scriptedStreamRuntime{events: deltaScript(sess.ID)}, sess)
	addr := startAttachedServer(t, ctx, sm)

	body := httpDoTCP(t, ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/agent/agent.yaml/root",
		api.RunAgentRequest{Messages: []api.Message{{Content: "hi"}}})

	var frames []string
	for frame := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n\n") {
		var event struct {
			Type    string `json:"type"`
			Content string `json:"content"`
			Output  string `json:"output"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event))
		summary := event.Type
		if text := event.Content + event.Output; text != "" {
			summary += ":" + text
		}
		frames = append(frames, summary)
	}
	return frames
}
