package transcribe

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/audio/capture"
)

func readClientEvent(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	var event map[string]any
	if err := conn.ReadJSON(&event); err != nil {
		t.Error(err)
		return nil
	}
	assert.Equal(t, want, event["type"])
	return event
}

func writeServerEvent(t *testing.T, conn *websocket.Conn, event any) {
	t.Helper()
	assert.NoError(t, conn.WriteJSON(event))
}

func acknowledgeFinish(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	update := readClientEvent(t, conn, "session.update")
	assert.Equal(t, map[string]any{"turn_detection": nil}, update["session"])
	writeServerEvent(t, conn, map[string]any{
		"type": "session.updated", "session": map[string]any{"turn_detection": nil},
	})
	commit := readClientEvent(t, conn, "input_audio_buffer.commit")
	assert.Equal(t, finalCommitID, commit["event_id"])
}

func commitItem(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	writeServerEvent(t, conn, map[string]string{"type": "input_audio_buffer.committed", "item_id": id})
}

func completeItem(t *testing.T, conn *websocket.Conn, id, text string) {
	t.Helper()
	writeServerEvent(t, conn, map[string]string{
		"type": "conversation.item.input_audio_transcription.completed", "item_id": id, "transcript": text,
	})
}

func emptyFinalCommit(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	writeServerEvent(t, conn, map[string]any{
		"type": "error", "error": map[string]string{
			"code": "input_audio_buffer_commit_empty", "message": "buffer empty", "event_id": finalCommitID,
		},
	})
}

func TestRun_FinishDrainsAndCommits(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		for _, text := range []string{"first", "second", "cleanup tail"} {
			event := readClientEvent(t, conn, "input_audio_buffer.append")
			assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(text)), event["audio"])
		}
		acknowledgeFinish(t, conn)
		commitItem(t, conn, "final")
		completeItem(t, conn, "final", "all captured audio")
		_, _, _ = conn.ReadMessage()
	})
	var cleaned atomic.Bool
	transcriber.capture = captureFunc(func(ctx context.Context, _ string, handler capture.AudioHandler, ready func()) error {
		defer cleaned.Store(true)
		handler([]byte("first"))
		handler([]byte("second"))
		ready()
		<-ctx.Done()
		handler([]byte("cleanup tail"))
		return nil
	})
	var texts []string
	err := transcriber.Run(t.Context(), finish, func(ctx context.Context, text string) {
		if err := ctx.Err(); err != nil {
			t.Errorf("finish canceled the transcript handler: %v", err)
		}
		texts = append(texts, text)
	}, func(context.Context) { close(finish) })
	require.NoError(t, err)
	assert.True(t, cleaned.Load())
	assert.Equal(t, []string{"all captured audio"}, texts)
}

func TestRun_FinishWaitsForVADItemsInOrder(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "final audio", true: "empty final commit"}[empty], func(t *testing.T) {
			t.Parallel()
			finish := make(chan struct{})
			completed := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			transcriber := testTranscriber(t, func(conn *websocket.Conn) {
				commitItem(t, conn, "first-vad")
				writeServerEvent(t, conn, map[string]string{
					"type": "conversation.item.input_audio_transcription.delta", "item_id": "first-vad", "delta": "first",
				})
				close(finish)
				update := readClientEvent(t, conn, "session.update")
				assert.Equal(t, map[string]any{"turn_detection": nil}, update["session"])
				// A VAD commit already in flight is still before the disable acknowledgement.
				commitItem(t, conn, "last-vad")
				writeServerEvent(t, conn, map[string]any{
					"type": "session.updated", "session": map[string]any{"turn_detection": nil},
				})
				readClientEvent(t, conn, "input_audio_buffer.commit")
				if empty {
					emptyFinalCommit(t, conn)
				} else {
					commitItem(t, conn, "final")
					completeItem(t, conn, "final", "third")
				}
				completeItem(t, conn, "last-vad", "second")
				close(completed)
				<-release
				completeItem(t, conn, "first-vad", "first")
				_, _, _ = conn.ReadMessage()
			})
			var texts []string
			done := make(chan error, 1)
			go func() {
				done <- transcriber.Run(t.Context(), finish, func(_ context.Context, text string) {
					texts = append(texts, text)
				}, nil)
			}()
			select {
			case <-completed:
			case <-time.After(5 * time.Second):
				t.Fatal("server did not finish later items")
			}
			select {
			case err := <-done:
				t.Fatalf("returned before first VAD item completed: %v", err)
			default:
			}
			release <- struct{}{}
			require.NoError(t, waitRun(t, done))
			want := []string{"first", " second"}
			if !empty {
				want = append(want, " third")
			}
			assert.Equal(t, want, texts, "delta and completed text must not be duplicated")
		})
	}
}

func TestRun_FinishEmptyWithoutItems(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		acknowledgeFinish(t, conn)
		emptyFinalCommit(t, conn)
		_, _, _ = conn.ReadMessage()
	})
	require.NoError(t, transcriber.Run(t.Context(), finish, nil, func(context.Context) { close(finish) }))
}

func TestRun_EmptyCommitMustMatchFinalRequest(t *testing.T) {
	t.Parallel()
	for _, finishFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "before finish", true: "different request"}[finishFirst], func(t *testing.T) {
			t.Parallel()
			finish := make(chan struct{})
			transcriber := testTranscriber(t, func(conn *websocket.Conn) {
				if finishFirst {
					close(finish)
					acknowledgeFinish(t, conn)
				}
				writeServerEvent(t, conn, map[string]any{
					"type": "error", "error": map[string]string{
						"code": "input_audio_buffer_commit_empty", "event_id": "unrelated",
					},
				})
				_, _, _ = conn.ReadMessage()
			})
			require.ErrorContains(t, transcriber.Run(t.Context(), finish, nil, nil), "input_audio_buffer_commit_empty")
		})
	}
}

func TestRun_TranscriptionFailure(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		close(finish)
		acknowledgeFinish(t, conn)
		commitItem(t, conn, "failed-item")
		writeServerEvent(t, conn, map[string]any{
			"type": "conversation.item.input_audio_transcription.failed", "item_id": "failed-item",
			"error": map[string]string{"code": "transcription_failed", "message": "cannot transcribe"},
		})
		_, _, _ = conn.ReadMessage()
	})
	err := transcriber.Run(t.Context(), finish, nil, nil)
	require.ErrorContains(t, err, "failed-item")
	require.ErrorContains(t, err, "cannot transcribe")
}

func TestRun_FinishBeforeStart(t *testing.T) {
	t.Parallel()
	transcriber := New(keyEnv(""))
	transcriber.supported = true
	finish := make(chan struct{})
	close(finish)
	require.NoError(t, transcriber.Run(t.Context(), finish, nil, func(context.Context) { t.Error("finished run became ready") }))
}

func TestRun_FinishDuringUpgrade(t *testing.T) {
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
	transcriber.capture = captureFunc(func(context.Context, string, capture.AudioHandler, func()) error {
		t.Error("finished upgrade started capture")
		return nil
	})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- transcriber.Run(t.Context(), finish, nil, nil) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade request not received")
	}
	close(finish)
	require.NoError(t, waitRun(t, done))
}

func TestRun_FinishUnblocksReady(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		acknowledgeFinish(t, conn)
		emptyFinalCommit(t, conn)
		_, _, _ = conn.ReadMessage()
	})
	require.NoError(t, transcriber.Run(t.Context(), finish, nil, func(ctx context.Context) {
		close(finish)
		<-ctx.Done()
	}))
}

func TestRun_AbortDuringFinish(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	committed := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		acknowledgeFinish(t, conn)
		close(committed)
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- transcriber.Run(ctx, finish, nil, func(context.Context) { close(finish) }) }()
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("final commit not sent")
	}
	cancel()
	require.NoError(t, waitRun(t, done))
}

func TestRun_PreservesCaptureCleanupError(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"abort", "server error", "server disconnect", "finish"} {
		t.Run(cause, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finish := make(chan struct{})
			capturing := make(chan struct{})
			transcriber := testTranscriber(t, func(conn *websocket.Conn) {
				<-capturing
				if cause == "server disconnect" {
					return
				}
				if cause == "server error" {
					writeServerEvent(t, conn, map[string]any{
						"type": "error", "error": map[string]string{"code": "denied", "message": "not allowed"},
					})
				}
				_, _, _ = conn.ReadMessage()
			})
			failure := errors.New("native cleanup failed")
			transcriber.capture = captureFunc(func(ctx context.Context, _ string, _ capture.AudioHandler, ready func()) error {
				close(capturing)
				ready()
				<-ctx.Done()
				return failure
			})
			err := transcriber.Run(ctx, finish, nil, func(context.Context) {
				switch cause {
				case "abort":
					cancel()
				case "finish":
					close(finish)
				}
			})
			require.ErrorIs(t, err, failure)
			if cause == "server error" {
				require.ErrorContains(t, err, "not allowed")
			}
			if cause == "server disconnect" {
				require.ErrorContains(t, err, "read transcription")
			}
		})
	}
}

func TestRun_FinishTimeout(t *testing.T) {
	t.Parallel()
	for _, blocked := range []string{"VAD acknowledgement", "commit acknowledgement", "transcript", "handler", "write"} {
		t.Run(blocked, func(t *testing.T) {
			t.Parallel()
			finish := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			transcriber := testTranscriber(t, func(conn *websocket.Conn) {
				switch blocked {
				case "VAD acknowledgement":
					readClientEvent(t, conn, "session.update")
				case "commit acknowledgement":
					acknowledgeFinish(t, conn)
				case "transcript", "handler":
					acknowledgeFinish(t, conn)
					commitItem(t, conn, "final")
					if blocked == "handler" {
						completeItem(t, conn, "final", "hello")
					}
				}
				<-release
			})
			transcriber.finishTimeout = time.Second
			if blocked == "write" {
				transcriber.capture = captureFunc(func(ctx context.Context, _ string, handler capture.AudioHandler, ready func()) error {
					ready()
					handler(make([]byte, 16<<20))
					<-ctx.Done()
					return nil
				})
			}
			done := make(chan error, 1)
			go func() {
				done <- transcriber.Run(t.Context(), finish, func(ctx context.Context, _ string) { <-ctx.Done() }, func(context.Context) { close(finish) })
			}()
			require.ErrorIs(t, waitRun(t, done), context.DeadlineExceeded)
		})
	}
}

func TestRun_AbortSkipsFinalization(t *testing.T) {
	t.Parallel()
	peerClosed := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		defer close(peerClosed)
		_, data, err := conn.ReadMessage()
		if err == nil {
			t.Error("abort did not close the connection")
		}
		assert.Empty(t, data, "abort must not send a final update or commit")
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, transcriber.Run(ctx, nil, nil, func(context.Context) { cancel() }))
	select {
	case <-peerClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not close the connection")
	}
}

func TestRun_CompletedItemsDeliveredOnce(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		commitItem(t, conn, "first")
		completeItem(t, conn, "first", "one")
		completeItem(t, conn, "first", "one")
		commitItem(t, conn, "first")
		completeItem(t, conn, "second", "two")
		commitItem(t, conn, "second")
		close(finish)
		acknowledgeFinish(t, conn)
		emptyFinalCommit(t, conn)
		_, _, _ = conn.ReadMessage()
	})
	var texts []string
	require.NoError(t, transcriber.Run(t.Context(), finish, func(_ context.Context, text string) {
		texts = append(texts, text)
	}, nil))
	assert.Equal(t, []string{"one", " two"}, texts)
}

func TestRun_UnexpectedCaptureStop(t *testing.T) {
	t.Parallel()
	transcriber := testTranscriber(t, func(conn *websocket.Conn) { _, _, _ = conn.ReadMessage() })
	transcriber.capture = captureFunc(func(context.Context, string, capture.AudioHandler, func()) error {
		return nil
	})
	require.ErrorContains(t, transcriber.Run(t.Context(), nil, nil, nil), "audio capture stopped unexpectedly")
}

func announceVAD(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	writeServerEvent(t, conn, map[string]string{"type": "input_audio_buffer.speech_started", "item_id": id})
	writeServerEvent(t, conn, map[string]string{"type": "input_audio_buffer.speech_stopped", "item_id": id})
}

func TestRun_FinishWaitsForLateVAD(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual final", true: "empty final"}[empty], func(t *testing.T) {
			t.Parallel()
			finish := make(chan struct{})
			transcriber := testTranscriber(t, func(conn *websocket.Conn) {
				announceVAD(t, conn, "late-vad")
				close(finish)
				acknowledgeFinish(t, conn)
				if empty {
					emptyFinalCommit(t, conn)
				}
				commitItem(t, conn, "late-vad")
				completeItem(t, conn, "late-vad", "first")
				if !empty {
					commitItem(t, conn, "manual-final")
					completeItem(t, conn, "manual-final", "second")
				}
				_, _, _ = conn.ReadMessage()
			})
			var texts []string
			require.NoError(t, transcriber.Run(t.Context(), finish, func(_ context.Context, text string) {
				texts = append(texts, text)
			}, nil))
			want := []string{"first"}
			if !empty {
				want = append(want, " second")
			}
			assert.Equal(t, want, texts)
		})
	}
}

func TestRun_ManualCommitOfActiveSpeech(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		writeServerEvent(t, conn, map[string]string{"type": "input_audio_buffer.speech_started", "item_id": "active"})
		close(finish)
		acknowledgeFinish(t, conn)
		commitItem(t, conn, "active")
		completeItem(t, conn, "active", "last words")
		_, _, _ = conn.ReadMessage()
	})
	var text string
	require.NoError(t, transcriber.Run(t.Context(), finish, func(_ context.Context, delta string) { text += delta }, nil))
	assert.Equal(t, "last words", text)
}

func TestRun_ManualCommitReplacesProvisionalVADItem(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	transcriber := testTranscriber(t, func(conn *websocket.Conn) {
		writeServerEvent(t, conn, map[string]string{"type": "input_audio_buffer.speech_started", "item_id": "provisional"})
		close(finish)
		acknowledgeFinish(t, conn)
		commitItem(t, conn, "manual-final")
		completeItem(t, conn, "manual-final", "last words")
		_, _, _ = conn.ReadMessage()
	})
	var text string
	require.NoError(t, transcriber.Run(t.Context(), finish, func(_ context.Context, delta string) { text += delta }, nil))
	assert.Equal(t, "last words", text)
}
