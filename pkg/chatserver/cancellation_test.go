package chatserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type cancelingHarness struct {
	cancel context.CancelFunc
}

func (cancelingHarness) Name() string { return "cancellation-test" }

func (h cancelingHarness) Run(ctx context.Context, prompt string, emit func(harness.Event)) error {
	if strings.HasSuffix(prompt, "interrupted") {
		emit(harness.Event{Type: harness.EventText, Text: "partial"})
		h.cancel()
		return ctx.Err()
	}
	emit(harness.Event{Type: harness.EventText, Text: "answer"})
	return nil
}

func (h cancelingHarness) Resume(ctx context.Context, _, prompt string, emit func(harness.Event)) error {
	return h.Run(ctx, prompt, emit)
}

type recordingRunner struct {
	runtimeRunner

	events []runtime.Event
}

func (r *recordingRunner) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	events := make(chan runtime.Event)
	r.events = nil
	go func() {
		defer close(events)
		for event := range r.runtimeRunner.RunStream(ctx, sess) {
			r.events = append(r.events, event)
			events <- event
		}
	}()
	return events
}

type failingCancellationWriter struct {
	*httptest.ResponseRecorder

	attempted strings.Builder
}

func (w *failingCancellationWriter) Write(p []byte) (int, error) {
	_, _ = w.attempted.Write(p)
	return 0, errors.New("client disconnected")
}

func TestChatCompletionCancellationDoesNotCommit(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		for _, failingWriter := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/failingWriter=%t", stream, failingWriter), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				root := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))
				rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
					runtime.WithSessionCompaction(false),
					runtime.WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) {
						return cancelingHarness{cancel: cancel}, nil
					}))
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, rt.Close()) })
				runner := &recordingRunner{runtimeRunner: rt}
				s := newConvServer(t)
				s.runtimes = newRuntimePool(t.Context(), nil, 1)
				s.runtimes.Put("root", runner)
				seed := session.New(session.WithNonInteractive(true))
				seed.AddMessage(session.UserMessage("seed"))
				s.conversations.Put("conversation", seed)

				rec := httptest.NewRecorder()
				disconnected := &failingCancellationWriter{ResponseRecorder: rec}
				var writer http.ResponseWriter = rec
				if failingWriter {
					writer = disconnected
				}
				handler := newRouter(s, Options{})
				done := startChat(handler, writer, chatRequest(t, "interrupted", stream).WithContext(ctx))
				require.Nil(t, waitForChat(t, done))

				require.ErrorIs(t, ctx.Err(), context.Canceled)
				assert.Same(t, seed, s.conversations.Get("conversation"), "cancellation must not advance the cached turn")
				assert.Equal(t, "seed", seed.GetLastUserMessageContent())
				assert.Equal(t, 1, seed.MessageCount())
				var stopped *runtime.StreamStoppedEvent
				for _, event := range runner.events {
					_, isError := event.(*runtime.ErrorEvent)
					assert.False(t, isError, "the real runtime reports cancellation without an ErrorEvent")
					if event, ok := event.(*runtime.StreamStoppedEvent); ok {
						stopped = event
					}
				}
				require.NotNil(t, stopped, "the runtime must be drained through its terminal event")
				assert.Equal(t, "canceled", stopped.Reason)

				body := rec.Body.String()
				if failingWriter {
					body = disconnected.attempted.String()
				}
				assert.Contains(t, body, `"error":`)
				assert.Contains(t, body, context.Canceled.Error())
				assert.NotContains(t, body, `"finish_reason":"stop"`)
				if stream {
					assert.Contains(t, body, `"finish_reason":"error"`)
					assert.Contains(t, body, "data: [DONE]\n\n")
				} else {
					assert.Equal(t, http.StatusInternalServerError, rec.Code)
				}

				require.Len(t, s.runtimes.channelFor("root"), 1)
				pooled := s.runtimes.takeIdle("root")
				require.Same(t, runner, pooled)
				s.runtimes.Put("root", pooled)
				retry := httptest.NewRecorder()
				require.Nil(t, waitForChat(t, startChat(handler, retry, chatRequest(t, "retry", stream))))
				assert.Equal(t, http.StatusOK, retry.Code)
				assert.Contains(t, retry.Body.String(), `"content":"answer"`)
				assert.NotContains(t, retry.Body.String(), `"error":`)
				assert.Equal(t, "retry", s.conversations.Get("conversation").GetLastUserMessageContent())
				assert.Same(t, runner, s.runtimes.takeIdle("root"), "the canceled runtime must remain reusable")
			})
		}
	}
}

type asyncCancellationRunner struct {
	stubRunner

	run func(*session.Session, chan<- runtime.Event)
}

func (r *asyncCancellationRunner) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	events := make(chan runtime.Event)
	go func() {
		defer close(events)
		r.run(sess, events)
	}()
	return events
}

func TestChatCompletionCancellationDuringCleanupKeepsCompletedTurn(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			cleanupStarted, releaseCleanup := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseCleanup) })
			t.Cleanup(release)
			runner := &asyncCancellationRunner{run: func(sess *session.Session, events chan<- runtime.Event) {
				sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "answer"}})
				events <- runtime.AgentChoice("root", sess.ID, "answer")
				events <- runtime.StreamStopped(sess.ID, "root", "normal")
				// The second unbuffered send acknowledges that the stop was processed.
				events <- &runtime.AgentInfoEvent{}
				cancel()
				close(cleanupStarted)
				<-releaseCleanup
			}}
			s := newConvServer(t)
			s.runtimes = newRuntimePool(t.Context(), nil, 1)
			s.runtimes.Put("root", runner)
			seed := session.New()
			seed.AddMessage(session.UserMessage("seed"))
			s.conversations.Put("conversation", seed)
			rec := httptest.NewRecorder()
			done := startChat(newRouter(s, Options{}), rec, chatRequest(t, "completed", stream).WithContext(ctx))
			waitForChat(t, cleanupStarted)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			assert.Same(t, seed, s.conversations.Get("conversation"), "the turn must not commit before cleanup finishes")
			assert.Empty(t, s.runtimes.channelFor("root"), "the runtime must stay owned until channel close")
			select {
			case <-done:
				t.Fatal("handler returned before runtime cleanup finished")
			default:
			}
			release()
			require.Nil(t, waitForChat(t, done))
			cached := s.conversations.Get("conversation")
			require.NotNil(t, cached)
			assert.NotSame(t, seed, cached)
			assert.Equal(t, "completed", cached.GetLastUserMessageContent())
			assert.Equal(t, "answer", cached.GetLastAssistantMessageContent())
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), `"finish_reason":"stop"`)
			assert.NotContains(t, rec.Body.String(), `"error":`)
			assert.Same(t, runner, s.runtimes.takeIdle("root"))
		})
	}
}

func TestRunAgentLoopCancellationWithoutErrorEvent(t *testing.T) {
	t.Parallel()
	type stop struct{ sessionID, reason string }
	for _, tc := range []struct {
		name     string
		canceled bool
		stops    []stop
		wantErr  bool
	}{
		{name: "already canceled without events", canceled: true, wantErr: true},
		{name: "root canceled with live context", stops: []stop{{"root-session", "canceled"}}, wantErr: true},
		{name: "nested normal cannot mask cancellation", canceled: true, stops: []stop{{"nested-session", "normal"}}, wantErr: true},
		{name: "nested canceled is not root failure", stops: []stop{{"nested-session", "canceled"}, {"root-session", "normal"}}},
		{name: "nested canceled cannot replace root success", stops: []stop{{"root-session", "normal"}, {"nested-session", "canceled"}}},
		{name: "nested normal cannot replace root cancellation", stops: []stop{{"root-session", "canceled"}, {"nested-session", "normal"}}, wantErr: true},
		{name: "explicit normal survives canceled context", canceled: true, stops: []stop{{"root-session", "normal"}}},
		{name: "empty root reason with canceled context", canceled: true, stops: []stop{{"root-session", ""}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tc.canceled {
				cancel()
			}
			sess := session.New()
			sess.ID = "root-session"
			runner := &stubRunner{}
			for _, stop := range tc.stops {
				runner.events = append(runner.events, runtime.StreamStopped(stop.sessionID, "root", stop.reason))
			}
			err := runAgentLoop(ctx, runner, sess, agentEmit{})
			if tc.wantErr {
				assert.ErrorIs(t, err, context.Canceled)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
