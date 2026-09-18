package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestRunStream_ImageOutputGuard_RejectsBeforeDispatch drives the
// image-output request guard (pkg/model/provider/gemini/image_output_guard.go)
// through the real run loop: a real *gemini.Client, talking to an httptest
// gateway, behind a real [agent.Agent] and [LocalRuntime], through RunStream.
//
// It proves the guard's rejection survives the full fallback/loop machinery
// unchanged: zero HTTP requests ever reach the provider, the loop emits
// exactly one ErrorEvent whose text is the guard's safe, fixed message, a
// StreamStartedEvent precedes it and a StreamStoppedEvent closes the turn,
// and no assistant content (text or reasoning) is ever produced — i.e. no
// silent "success" alongside the error. The TUI-facing half of this seam
// (the same ErrorEvent reaching the message list and clearing the spinner)
// is covered by
// TestImageOutputGuard_RuntimeToTUI_RejectsBeforeDispatchAndClearsSpinner in
// pkg/tui/page/chat, which cannot import [team] (see
// e2e/dependencies_test.go's "TUI musn't know about teams").
func TestRunStream_ImageOutputGuard_RejectsBeforeDispatch(t *testing.T) {
	t.Parallel()

	var providerCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		payload := `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}]}`
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider:           "google",
		Model:              "gemini-2.5-flash-image",
		OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
	}
	env := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "test-dd-token",
	})
	client, err := gemini.NewClient(t.Context(), cfg, env, options.WithGateway(server.URL))
	require.NoError(t, err)

	// A custom function tool is enough to trip the guard on its own (no
	// ResponseModalities / rendering involved): declaring
	// output_capabilities.image is incompatible with any custom tool.
	readFileTool := tools.Tool{
		Name:        "read_file",
		Description: "reads a file from disk",
		Parameters:  map[string]any{"type": "object"},
	}
	root := agent.New("root", "You are a test agent", agent.WithModel(client), agent.WithTools(readFileTool))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("draw a cat"))
	sess.Title = "image output guard integration test"

	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}

	assert.Zero(t, providerCalls.Load(), "the guard must reject before any request reaches the provider")

	var errEvent *ErrorEvent
	var streamStarted *StreamStartedEvent
	var streamStopped *StreamStoppedEvent
	for _, ev := range events {
		switch e := ev.(type) {
		case *ErrorEvent:
			require.Nil(t, errEvent, "expected exactly one ErrorEvent")
			errEvent = e
		case *StreamStartedEvent:
			if streamStarted == nil {
				streamStarted = e
			}
		case *StreamStoppedEvent:
			streamStopped = e
		case *AgentChoiceEvent:
			t.Fatalf("guard rejection must not produce assistant content, got AgentChoiceEvent %q", e.Content)
		case *AgentChoiceReasoningEvent:
			t.Fatalf("guard rejection must not produce reasoning content, got AgentChoiceReasoningEvent %q", e.Content)
		}
	}
	require.NotNil(t, streamStarted, "expected a StreamStartedEvent")
	require.NotNil(t, errEvent, "expected an ErrorEvent for the rejected request")
	require.NotNil(t, streamStopped, "expected a StreamStoppedEvent to close out the turn")
	assert.Contains(t, errEvent.Error, "output_capabilities.image")
	assert.Contains(t, errEvent.Error, "tools")
	assert.Equal(t, ErrorCodeModelError, errEvent.Code)
}
