package transcribe

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/audio/capture"
	"github.com/docker/docker-agent/pkg/environment"
)

type captureFunc func(context.Context, string, capture.AudioHandler, func()) error

func (f captureFunc) Run(ctx context.Context, path string, handler capture.AudioHandler, ready func()) error {
	return f(ctx, path, handler, ready)
}

func testTranscriber(t *testing.T, handler func(*websocket.Conn)) *Transcriber {
	t.Helper()
	var connections sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		defer connections.Done()
		assert.Equal(t, "Bearer test-api-key", r.Header.Get("Authorization"))
		assert.Equal(t, "realtime=v1", r.Header.Get("OpenAI-Beta"))
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer conn.Close()
		var config map[string]any
		if !assert.NoError(t, conn.ReadJSON(&config)) {
			return
		}
		assert.Equal(t, "session.update", config["type"])
		handler(conn)
	}))
	t.Cleanup(func() {
		server.Close()
		connections.Wait()
	})
	return &Transcriber{
		env: keyEnv("test-api-key"), endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), supported: true,
		capture: captureFunc(func(ctx context.Context, _ string, _ capture.AudioHandler, ready func()) error {
			if ready != nil {
				ready()
			}
			<-ctx.Done()
			return nil
		}),
	}
}

func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("transcription did not finish")
		return nil
	}
}

func keyEnv(key string) environment.Provider {
	return environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": key})
}

type countingEnv struct {
	environment.Provider

	gets atomic.Int32
}

func (c *countingEnv) Get(ctx context.Context, name string) (string, bool) {
	c.gets.Add(1)
	return c.Provider.Get(ctx, name)
}

func TestRun_ResolvesKeyLazily(t *testing.T) {
	t.Parallel()
	env := &countingEnv{Provider: keyEnv("test-api-key")}
	transcriber := New(env)
	assert.Zero(t, env.gets.Load(), "New must not resolve the key")

	transcriber.supported = true
	transcriber.endpoint = ":invalid"
	err := transcriber.Run(t.Context(), nil, nil, nil)
	require.ErrorContains(t, err, "connect /speak directly to OpenAI Realtime API")
	assert.Equal(t, int32(1), env.gets.Load())
}

// blockingEnv blocks every lookup until ctx ends, like `op read` at an unlock prompt.
type blockingEnv struct{ entered chan struct{} }

func (b blockingEnv) Get(ctx context.Context, _ string) (string, bool) {
	close(b.entered)
	<-ctx.Done()
	return "", false
}

func TestRun_FinishInterruptsKeyLookup(t *testing.T) {
	t.Parallel()
	env := blockingEnv{entered: make(chan struct{})}
	transcriber := New(env)
	transcriber.supported = true
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- transcriber.Run(t.Context(), finish, nil, nil) }()

	<-env.entered
	close(finish)
	require.NoError(t, waitRun(t, done))
}

func TestRun_CancelDuringKeyLookupIsNotAnError(t *testing.T) {
	t.Parallel()
	env := blockingEnv{entered: make(chan struct{})}
	transcriber := New(env)
	transcriber.supported = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- transcriber.Run(ctx, nil, nil, nil) }()

	<-env.entered
	cancel()
	require.NoError(t, waitRun(t, done))
}

func TestRun_MissingAPIKey(t *testing.T) {
	t.Parallel()
	transcriber := New(keyEnv(""))
	transcriber.supported = true
	err := transcriber.Run(t.Context(), nil, nil, nil)
	require.EqualError(t, err, "/speak sends audio directly to OpenAI; set a separate OPENAI_API_KEY environment variable even when chat uses Docker's models gateway, which does not support /speak")
}

func TestRun_CanceledBeforeStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	transcriber := New(keyEnv("test-api-key"))
	transcriber.supported = true
	require.NoError(t, transcriber.Run(ctx, nil, nil, func(context.Context) { t.Error("canceled run became ready") }))
}

func TestRun_Unsupported(t *testing.T) {
	t.Parallel()
	transcriber := New(keyEnv("test-api-key"))
	transcriber.supported = false
	require.ErrorIs(t, transcriber.Run(t.Context(), nil, nil, nil), ErrNotSupported)
}

func TestRun_ConnectionFailure(t *testing.T) {
	t.Parallel()
	transcriber := New(keyEnv("test-api-key"))
	transcriber.supported = true
	transcriber.endpoint = ":invalid"
	err := transcriber.Run(t.Context(), nil, nil, nil)
	require.ErrorContains(t, err, "connect /speak directly to OpenAI Realtime API")
}

func TestRun_CaptureFailure(t *testing.T) {
	t.Parallel()
	transcriber := testTranscriber(t, func(conn *websocket.Conn) { _, _, _ = conn.ReadMessage() })
	failure := errors.New("microphone unavailable")
	transcriber.capture = captureFunc(func(context.Context, string, capture.AudioHandler, func()) error {
		return failure
	})
	require.ErrorIs(t, transcriber.Run(t.Context(), nil, nil, func(context.Context) { t.Error("failed capture became ready") }), failure)
}

func TestRun_AudioAndTranscript(t *testing.T) {
	t.Parallel()
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		var audio map[string]string
		if err := conn.ReadJSON(&audio); err != nil {
			t.Error(err)
			return
		}
		assert.Equal(t, "input_audio_buffer.append", audio["type"])
		assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("audio")), audio["audio"])
		if err := conn.WriteJSON(map[string]string{"type": "input_audio_buffer.committed", "item_id": "item-1"}); err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteJSON(map[string]string{
			"type": "conversation.item.input_audio_transcription.completed", "item_id": "item-1", "transcript": "hello",
		}); err != nil {
			t.Error(err)
			return
		}
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cleaned atomic.Bool
	transcriber.capture = captureFunc(func(ctx context.Context, _ string, handler capture.AudioHandler, ready func()) error {
		defer cleaned.Store(true)
		ready()
		handler([]byte("audio"))
		<-ctx.Done()
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- transcriber.Run(ctx, nil, func(_ context.Context, delta string) {
			assert.Equal(t, "hello", delta)
			cancel()
		}, func(context.Context) {})
	}()
	require.NoError(t, waitRun(t, done))
	assert.True(t, cleaned.Load(), "Run must join capture cleanup")
}

func TestRun_ServerFailure(t *testing.T) {
	t.Parallel()
	for _, serverError := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "error event"}[serverError], func(t *testing.T) {
			transcriber := testTranscriber(t, func(conn *websocket.Conn) {
				if serverError {
					assert.NoError(t, conn.WriteJSON(map[string]any{
						"type": "error", "error": map[string]string{"code": "denied", "message": "not allowed"},
					}))
				}
			})
			var cleaned atomic.Bool
			transcriber.capture = captureFunc(func(ctx context.Context, _ string, _ capture.AudioHandler, _ func()) error {
				defer cleaned.Store(true)
				<-ctx.Done()
				return nil
			})
			err := transcriber.Run(t.Context(), nil, nil, nil)
			require.Error(t, err)
			if serverError {
				require.ErrorContains(t, err, "not allowed")
			}
			assert.True(t, cleaned.Load())
		})
	}
}

func TestRun_ConcurrentRuns(t *testing.T) {
	t.Parallel()
	transcriber := testTranscriber(t, func(conn *websocket.Conn) { _, _, _ = conn.ReadMessage() })
	first, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	second, cancelSecond := context.WithCancel(t.Context())
	defer cancelSecond()
	ready := make(chan struct{}, 2)
	doneFirst, doneSecond := make(chan error, 1), make(chan error, 1)
	go func() { doneFirst <- transcriber.Run(first, nil, nil, func(context.Context) { ready <- struct{}{} }) }()
	go func() { doneSecond <- transcriber.Run(second, nil, nil, func(context.Context) { ready <- struct{}{} }) }()
	for range 2 {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("transcription did not become ready")
		}
	}
	cancelFirst()
	require.NoError(t, waitRun(t, doneFirst))
	select {
	case err := <-doneSecond:
		t.Fatalf("canceling first run ended second run: %v", err)
	default:
	}
	cancelSecond()
	require.NoError(t, waitRun(t, doneSecond))
}

func TestRun_CancelBlockedHandler(t *testing.T) {
	t.Parallel()
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		if err := conn.WriteJSON(map[string]string{"type": "input_audio_buffer.committed", "item_id": "item-1"}); err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteJSON(map[string]string{
			"type": "conversation.item.input_audio_transcription.completed", "item_id": "item-1", "transcript": "hello",
		}); err != nil {
			t.Error(err)
			return
		}
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- transcriber.Run(ctx, nil, func(ctx context.Context, _ string) {
			close(entered)
			<-ctx.Done()
		}, nil)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not called")
	}
	cancel()
	require.NoError(t, waitRun(t, done))
}

func TestRun_AudioOverflowCancelsBlockedWrite(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	transcriber := testTranscriber(t, func(*websocket.Conn) {
		<-release // Keep the connection open without draining audio.
	})
	transcriber.capture = captureFunc(func(ctx context.Context, _ string, handler capture.AudioHandler, _ func()) error {
		chunk := make([]byte, 1<<20)
		for range 100 {
			handler(chunk)
		}
		<-ctx.Done()
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- transcriber.Run(t.Context(), nil, nil, nil) }()
	require.ErrorContains(t, waitRun(t, done), "audio upload fell behind capture")
}

func TestRun_CancelDuringUpgrade(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	t.Cleanup(server.Close)
	defer close(release)
	transcriber := New(keyEnv("test-api-key"))
	transcriber.supported = true
	transcriber.endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- transcriber.Run(ctx, nil, nil, nil) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade request not received")
	}
	cancel()
	require.NoError(t, waitRun(t, done))
}

func TestRun_ServerFailureCancelsBlockedReady(t *testing.T) {
	t.Parallel()
	ready := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		<-ready
		if err := conn.WriteJSON(map[string]any{
			"type": "error", "error": map[string]string{"code": "denied", "message": "not allowed"},
		}); err != nil {
			t.Error(err)
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- transcriber.Run(t.Context(), nil, nil, func(ctx context.Context) {
			close(ready)
			<-ctx.Done()
		})
	}()
	require.ErrorContains(t, waitRun(t, done), "not allowed")
}
