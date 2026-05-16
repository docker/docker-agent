//go:build darwin && !no_audio

// Package transcribe provides real-time audio transcription using OpenAI's Realtime API.
package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"github.com/docker/docker-agent/pkg/audio/capture"
)

const openAIRealtimeURL = "wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview"

// TranscriptHandler is called when new transcription text is received.
type TranscriptHandler func(delta string)

// Transcriber provides real-time audio transcription using OpenAI's Realtime API.
type Transcriber struct {
	apiKey  string
	conn    *websocket.Conn
	capture *capture.Capturer
	running atomic.Bool
	cancel  context.CancelFunc
	writeMu sync.Mutex
}

// serverEvent represents events from the OpenAI Realtime API.
type serverEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta,omitempty"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// New creates a new Transcriber with the given OpenAI API key.
func New(apiKey string) *Transcriber {
	return &Transcriber{
		apiKey:  apiKey,
		capture: capture.NewCapturer(capture.SampleRate24000),
	}
}

// Start begins audio capture and transcription. The handler is called for each
// transcription delta received. Returns an error if already running or if
// connection fails. Call Stop to end transcription.
func (t *Transcriber) Start(ctx context.Context, handler TranscriptHandler) error {
	if t.apiKey == "" {
		return errors.New("speech-to-text requires the OPENAI_API_KEY environment variable to be set")
	}

	if wasRunning := t.running.Swap(true); wasRunning {
		return errors.New("transcriber already running")
	}

	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel

	// Connect to OpenAI Realtime API
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, openAIRealtimeURL, http.Header{ //nolint:bodyclose // websocket upgrade response
		"Authorization": []string{"Bearer " + t.apiKey},
		"OpenAI-Beta":   []string{"realtime=v1"},
	})
	if err != nil {
		t.running.Store(false)
		return fmt.Errorf("connect to OpenAI: %w", err)
	}
	t.conn = conn

	// Configure session for transcription only
	if err := conn.WriteJSON(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"modalities":         []string{"text"},
			"input_audio_format": "pcm16",
			"input_audio_transcription": map[string]string{
				"model": "whisper-1",
			},
			"turn_detection": map[string]any{
				"type": "server_vad",
			},
		},
	}); err != nil {
		conn.Close()
		t.running.Store(false)
		return fmt.Errorf("configure session: %w", err)
	}

	// Start reading WebSocket messages
	go t.readLoop(ctx, handler)

	// Start audio capture
	err = t.capture.Start("", func(data []byte) {
		t.writeMu.Lock()
		defer t.writeMu.Unlock()

		if t.conn != nil {
			_ = t.conn.WriteJSON(map[string]string{
				"type":  "input_audio_buffer.append",
				"audio": base64.StdEncoding.EncodeToString(data),
			})
		}
	})
	if err != nil {
		conn.Close()
		t.running.Store(false)
		return fmt.Errorf("start capture: %w", err)
	}

	return nil
}

// Stop ends the transcription session and releases resources.
func (t *Transcriber) Stop() {
	if wasRunning := t.running.Swap(false); !wasRunning {
		return
	}

	// Cancel the context to stop the read loop
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}

	// Stop audio capture
	_ = t.capture.Stop()

	// Commit any remaining audio buffer
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.conn != nil {
		_ = t.conn.WriteJSON(map[string]string{"type": "input_audio_buffer.commit"})
		_ = t.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		t.conn.Close()
		t.conn = nil
	}
}

// IsRunning returns true if transcription is currently active.
func (t *Transcriber) IsRunning() bool {
	return t.running.Load()
}

// IsSupported returns true on macOS where audio capture is available.
func (t *Transcriber) IsSupported() bool {
	return true
}

// readLoop reads messages from the WebSocket and calls the handler for transcription deltas.
func (t *Transcriber) readLoop(ctx context.Context, handler TranscriptHandler) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, msg, err := t.conn.ReadMessage()
		if err != nil {
			// Connection closed or error
			return
		}

		var event serverEvent
		if json.Unmarshal(msg, &event) != nil {
			continue
		}

		switch event.Type {
		case "conversation.item.input_audio_transcription.delta":
			if handler != nil && event.Delta != "" {
				handler(event.Delta)
			}
		case "error":
			// Ignore empty buffer commit errors
			if event.Error != nil && event.Error.Code != "input_audio_buffer_commit_empty" {
				// Log or handle error
				return
			}
		}
	}
}
