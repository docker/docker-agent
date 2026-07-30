package chat

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// recordingMessages wraps the real [messages.Model], counting the calls this
// test cares about while forwarding every call to the embedded
// implementation so its real state (the spinner entry, the rendered error)
// mutates for real. Mirrors the recordingSidebar pattern in
// agent_switching_test.go.
type recordingMessages struct {
	messages.Model

	assistantMessageCalls int
	errorMessages         []string
	appendCalls           int
	appendReasoningCalls  int
	removeSpinnerCalls    int
}

func (r *recordingMessages) AddAssistantMessage(sender, label string) tea.Cmd {
	r.assistantMessageCalls++
	return r.Model.AddAssistantMessage(sender, label)
}

func (r *recordingMessages) AddErrorMessage(content string) tea.Cmd {
	r.errorMessages = append(r.errorMessages, content)
	return r.Model.AddErrorMessage(content)
}

func (r *recordingMessages) AppendToLastMessage(agentName, content string) tea.Cmd {
	r.appendCalls++
	return r.Model.AppendToLastMessage(agentName, content)
}

func (r *recordingMessages) AppendReasoning(agentName, content string) tea.Cmd {
	r.appendReasoningCalls++
	return r.Model.AppendReasoning(agentName, content)
}

// RemoveSpinner counts calls and forwards to the real implementation, whose
// own removeSpinner is a no-op (skips invalidateView) when the last message
// isn't a spinner — so a VisualGeneration bump around the call proves a
// spinner actually existed and was removed, not just that the method fired.
func (r *recordingMessages) RemoveSpinner() {
	r.removeSpinnerCalls++
	r.Model.RemoveSpinner()
}

// TestImageOutputGuard_RuntimeToTUI_RejectsBeforeDispatchAndClearsSpinner
// drives the gateway image-output request guard
// (pkg/model/provider/gemini/image_output_guard.go) through its real
// pre-dispatch path — a real *gemini.Client talking to an httptest gateway —
// and then through the exact production event flow the run loop uses to
// surface a fatal model error (pkg/runtime/loop_steps.go's
// handleStreamError: modelerrors.FormatError + ErrorWithCodeForSession) into
// a real chatPage.handleRuntimeEvent (pkg/tui/page/chat/runtime_events.go).
//
// This package cannot build a real [runtime.LocalRuntime] run itself: TUI
// code must not import pkg/team (see e2e/dependencies_test.go's "TUI musn't
// know about teams"), which a real run requires. That half — the guard's
// rejection surviving the full fallback/loop machinery unchanged, through
// RunStream — is covered by
// TestRunStream_ImageOutputGuard_RejectsBeforeDispatch in pkg/runtime. The
// two tests meet at the same seam: [modelerrors.FormatError] and the
// [runtime.ErrorEvent] it feeds, exercised here with the constructors
// production code actually calls, not a hand-rolled message.
//
// It proves, in one flow, against the real message-list state (never a call
// counter alone): zero HTTP requests reach the provider (the guard rejects
// before dispatch); StreamStartedEvent leaves exactly one real spinner
// message; handling the actual ErrorEvent through AddErrorMessage removes
// that spinner and adds the fixed safe error before StreamStoppedEvent ever
// runs; and the later StreamStoppedEvent's call to the exported RemoveSpinner
// finds nothing left to remove — a no-op, not the mechanism that cleared the
// spinner. No assistant text or reasoning is ever appended, i.e. no silent
// "success" alongside the error.
func TestImageOutputGuard_RuntimeToTUI_RejectsBeforeDispatchAndClearsSpinner(t *testing.T) {
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
	// output_capabilities.image on the gateway route is incompatible with
	// any custom tool.
	readFileTool := tools.Tool{
		Name:        "read_file",
		Description: "reads a file from disk",
		Parameters:  map[string]any{"type": "object"},
	}
	stream, dispatchErr := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "draw a cat"},
	}, []tools.Tool{readFileTool})
	require.Nil(t, stream)
	require.Error(t, dispatchErr)

	var incompatible *gemini.ImageOutputRequestIncompatibleError
	require.ErrorAs(t, dispatchErr, &incompatible, "expected the guard's rejection error")
	require.Zero(t, providerCalls.Load(), "the guard must reject before any request reaches the provider")

	// Build the exact production event sequence: same constructors and the
	// same modelerrors.FormatError call pkg/runtime/loop_steps.go's
	// handleStreamError makes when a model call fails fatally.
	const (
		agentName = "root"
		sessionID = "sess-image-output-guard"
	)
	visibleError := modelerrors.FormatError(dispatchErr)

	sessForPage := session.New()
	p := New(animation.NewRuntime(), t.Context(),
		app.New(t.Context(), queueTestRuntime{}, sessForPage),
		service.NewSessionState(sessForPage)).(*chatPage)

	rec := &recordingMessages{Model: p.messages}
	p.messages = rec

	handled, _ := p.handleRuntimeEvent(runtime.StreamStarted(sessionID, agentName))
	require.True(t, handled, "expected StreamStartedEvent to be a recognized runtime event")
	assert.Equal(t, 1, rec.assistantMessageCalls,
		"the stream-started spinner must have been requested exactly once")
	require.Equal(t, 1, rec.MessageTypeCount(types.MessageTypeSpinner),
		"a real spinner message must exist in the list right after StreamStartedEvent")
	assert.Zero(t, rec.removeSpinnerCalls, "no spinner removal is expected before the stream stops")

	handled, _ = p.handleRuntimeEvent(runtime.ErrorWithCodeForSession(sessionID, runtime.ErrorCodeModelError, visibleError))
	require.True(t, handled, "expected ErrorEvent to be a recognized runtime event")

	// The spinner must already be gone here, before StreamStoppedEvent ever
	// runs: this is the production AddErrorMessage -> internal removeSpinner
	// path (pkg/tui/components/messages/messages.go), not the later
	// StreamStoppedEvent -> exported RemoveSpinner cleanup. If
	// AddErrorMessage's internal removal were disabled, this assertion would
	// fail while the spinner count stayed at 1.
	require.Zero(t, rec.MessageTypeCount(types.MessageTypeSpinner),
		"the actual ErrorEvent must remove the real spinner via AddErrorMessage before the stream stops")
	require.Equal(t, 1, rec.MessageTypeCount(types.MessageTypeError),
		"the guard's error must be added as a real error message")
	assert.Zero(t, rec.removeSpinnerCalls,
		"the exported RemoveSpinner must not have been invoked yet; removal so far is AddErrorMessage's internal one")

	handled, _ = p.handleRuntimeEvent(runtime.StreamStopped(sessionID, agentName, "error"))
	require.True(t, handled, "expected StreamStoppedEvent to be a recognized runtime event")

	assert.Equal(t, 1, rec.removeSpinnerCalls,
		"the outermost stream-stop cleanup still calls the exported RemoveSpinner once")
	assert.Zero(t, rec.MessageTypeCount(types.MessageTypeSpinner),
		"the spinner count must stay at zero across StreamStoppedEvent: its RemoveSpinner call is a no-op here, "+
			"not the mechanism that removed the spinner")

	require.Len(t, rec.errorMessages, 1, "the guard's error must reach the message list exactly once")
	assert.Equal(t, visibleError, rec.errorMessages[0],
		"the TUI must show the exact same fixed error text modelerrors.FormatError produced")
	assert.Contains(t, rec.errorMessages[0], "output_capabilities.image")
	assert.Contains(t, rec.errorMessages[0], "tools")
	assert.Zero(t, rec.appendCalls, "no assistant text may be appended after a rejected request")
	assert.Zero(t, rec.appendReasoningCalls, "no reasoning may be appended after a rejected request")
	assert.False(t, p.working, "the chat page must not be left in a working state after the stream stops")
}
