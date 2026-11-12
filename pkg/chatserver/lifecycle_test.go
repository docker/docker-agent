package chatserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// Hold the actual terminal Write while another request acquires the conversation.
type terminalWriter struct {
	*httptest.ResponseRecorder

	stream  bool
	mode    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	terminal := !w.stream || strings.Contains(string(p), `"finish_reason":`) || strings.Contains(string(p), `"error":`)
	if terminal {
		w.once.Do(func() {
			close(w.entered)
			<-w.release
		})
		switch w.mode {
		case "error":
			return 0, errors.New("transport failed")
		case "panic":
			panic("writer panic")
		}
	}
	return w.ResponseRecorder.Write(p)
}

func chatRequest(t *testing.T, user string, stream bool) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"model":"client-model","messages":[{"role":"user","content":%q}],"stream":%t,"stream_options":{"include_usage":true}}`, user, stream)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Conversation-Id", "conversation")
	return req
}

func waitForChat[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("chat lifecycle gate timed out")
		var zero T
		return zero
	}
}

func startChat(handler http.Handler, w http.ResponseWriter, req *http.Request) <-chan any {
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		handler.ServeHTTP(w, req)
	}()
	return done
}

func TestChatCompletionFinalizesBeforeTerminalWrite(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			for _, mode := range []string{"success", "error", "panic"} {
				t.Run(fmt.Sprintf("stream=%t/failed=%t/writer=%s", stream, failed, mode), func(t *testing.T) {
					t.Parallel()
					s := newConvServer(t)
					s.runtimes = newRuntimePool(t.Context(), nil, 2)
					seed := session.New()
					seed.AddMessage(session.UserMessage("seed"))
					s.conversations.Put("conversation", seed)

					nextStarted := make(chan *session.Session, 1)
					nextRelease := make(chan struct{})
					releaseNext := sync.OnceFunc(func() { close(nextRelease) })
					t.Cleanup(releaseNext)
					runner := &stubRunner{}
					runner.onRun = func(_ context.Context, sess *session.Session) {
						runner.events = nil
						if sess.GetLastUserMessageContent() == "next" {
							nextStarted <- sess
							<-nextRelease
						} else {
							sess.InputTokens, sess.OutputTokens = 10, 20
							runner.events = []runtime.Event{runtime.AgentChoice("root", "session", "answer")}
							if failed {
								runner.events = append(runner.events, &runtime.ErrorEvent{Error: "run failed"})
							}
						}
						sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "answer"}})
					}
					s.runtimes.Put("root", runner)
					handler := newRouter(s, Options{})
					writer := &terminalWriter{
						ResponseRecorder: httptest.NewRecorder(), stream: stream, mode: mode,
						entered: make(chan struct{}), release: make(chan struct{}),
					}
					releaseWriter := sync.OnceFunc(func() { close(writer.release) })
					t.Cleanup(releaseWriter)
					firstDone := startChat(handler, writer, chatRequest(t, "first", stream))
					waitForChat(t, writer.entered)

					cached := s.conversations.Get("conversation")
					require.NotNil(t, cached)
					wantCount := 3
					if failed {
						assert.Same(t, seed, cached)
						wantCount = 1
					} else {
						assert.Equal(t, "first", cached.GetLastUserMessageContent())
					}
					assert.Equal(t, wantCount, cached.MessageCount())
					require.Len(t, s.runtimes.channelFor("root"), 1, "runtime must be returned before terminal Write")

					nextWriter := httptest.NewRecorder()
					nextDone := startChat(handler, nextWriter, chatRequest(t, "next", false))
					next := waitForChat(t, nextStarted)
					assert.Equal(t, wantCount+1, next.MessageCount(), "next run must see the committed state")
					assert.Empty(t, s.runtimes.channelFor("root"), "next run must reuse the returned runtime")

					assertOverlap := func() {
						t.Helper()
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, chatRequest(t, "overlap", false))
						assert.Equal(t, http.StatusConflict, rec.Code)
					}
					assertOverlap()
					releaseWriter()
					if mode == "panic" {
						assert.Equal(t, "writer panic", waitForChat(t, firstDone))
					} else {
						assert.Nil(t, waitForChat(t, firstDone))
					}
					assertOverlap()
					assert.Empty(t, s.runtimes.channelFor("root"), "old defer must not return an active runtime twice")

					releaseNext()
					assert.Nil(t, waitForChat(t, nextDone))
					assert.Equal(t, http.StatusOK, nextWriter.Code)
					assert.Equal(t, "next", s.conversations.Get("conversation").GetLastUserMessageContent())
					assert.Len(t, s.runtimes.channelFor("root"), 1)
					require.True(t, s.conversationLocks.tryAcquire("conversation"))
					s.conversationLocks.release("conversation")

					if mode == "success" {
						switch {
						case stream:
							assert.Contains(t, writer.Body.String(), "data: [DONE]\n\n")
							assert.Contains(t, writer.Body.String(), `"total_tokens":30`)
							if failed {
								assert.Contains(t, writer.Body.String(), `"finish_reason":"error"`)
								assert.Contains(t, writer.Body.String(), `"message":"run failed"`)
							} else {
								assert.Contains(t, writer.Body.String(), `"finish_reason":"stop"`)
							}
						case failed:
							assert.Equal(t, http.StatusInternalServerError, writer.Code)
							assert.Contains(t, writer.Body.String(), "run failed")
						default:
							var response ChatCompletionResponse
							require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &response))
							assert.Equal(t, "client-model", response.Model)
							assert.Equal(t, "answer", response.Choices[0].Message.Content)
							assert.EqualValues(t, 30, response.Usage.TotalTokens)
						}
					}
				})
			}
		}
	}
}

func TestChatCompletionEarlyErrorReleasesBeforeWrite(t *testing.T) {
	t.Parallel()
	for _, user := range []string{"", "no runtime"} {
		t.Run(user, func(t *testing.T) {
			t.Parallel()
			s := newConvServer(t)
			writer := &terminalWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(writer.release) })
			t.Cleanup(release)
			done := startChat(newRouter(s, Options{}), writer, chatRequest(t, user, false))
			waitForChat(t, writer.entered)
			require.True(t, s.conversationLocks.tryAcquire("conversation"))
			defer s.conversationLocks.release("conversation")
			release()
			assert.Nil(t, waitForChat(t, done))
			assert.False(t, s.conversationLocks.tryAcquire("conversation"), "old defer must not release the new owner")
			assert.Nil(t, s.conversations.Get("conversation"))
			if user == "" {
				assert.Equal(t, http.StatusBadRequest, writer.Code)
			} else {
				assert.Equal(t, http.StatusInternalServerError, writer.Code)
			}
		})
	}
}

func TestChatCompletionRunPanicDiscardsRuntime(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)
	s.runtimes = newRuntimePool(t.Context(), nil, 1)
	s.runtimes.Put("root", &stubRunner{onRun: func(context.Context, *session.Session) { panic("run panic") }})
	require.PanicsWithValue(t, "run panic", func() {
		newRouter(s, Options{}).ServeHTTP(httptest.NewRecorder(), chatRequest(t, "first", false))
	})
	assert.Empty(t, s.runtimes.channelFor("root"))
	assert.Nil(t, s.conversations.Get("conversation"))
	require.True(t, s.conversationLocks.tryAcquire("conversation"))
	s.conversationLocks.release("conversation")
}

// Pause before JSON encoding, rather than after it, to exercise the snapshot.
type gatedJSONSerializer struct {
	echo.DefaultJSONSerializer

	entered chan struct{}
	release chan struct{}
}

func (s *gatedJSONSerializer) Serialize(c echo.Context, value any, indent string) error {
	close(s.entered)
	<-s.release
	return s.DefaultJSONSerializer.Serialize(c, value, indent)
}

func TestChatCompletionSnapshotsBeforeRuntimeReuse(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			t.Parallel()
			s := newConvServer(t)
			s.runtimes = newRuntimePool(t.Context(), nil, 1)
			var previous *session.Session
			runner := &stubRunner{onRun: func(_ context.Context, sess *session.Session) {
				if previous != nil {
					previous.InputTokens, previous.OutputTokens = 100, 200
					previous.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "changed"}})
				}
				sess.InputTokens, sess.OutputTokens = 10, 20
				sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "original"}})
				previous = sess
			}}
			s.runtimes.Put("root", runner)
			entered, release := make(chan struct{}), make(chan struct{})
			releaseWriter := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseWriter)
			e := echo.New()
			e.POST("/v1/chat/completions", s.handleChatCompletions)
			rec := httptest.NewRecorder()
			var writer http.ResponseWriter = rec
			if stream {
				writer = &terminalWriter{ResponseRecorder: rec, stream: true, entered: entered, release: release}
			} else {
				e.JSONSerializer = &gatedJSONSerializer{entered: entered, release: release}
			}
			done := startChat(e, writer, chatRequest(t, "first", stream))
			waitForChat(t, entered)
			nextRec := httptest.NewRecorder()
			newRouter(s, Options{}).ServeHTTP(nextRec, chatRequest(t, "next", false))
			require.Equal(t, http.StatusOK, nextRec.Code)
			releaseWriter()
			assert.Nil(t, waitForChat(t, done))
			assert.Contains(t, rec.Body.String(), `"total_tokens":30`)
			assert.NotContains(t, rec.Body.String(), `"total_tokens":300`)
			if !stream {
				assert.Contains(t, rec.Body.String(), `"content":"original"`)
				assert.NotContains(t, rec.Body.String(), "changed")
			}
		})
	}
}
