// Package transcribe provides real-time audio transcription using OpenAI's Realtime API.
package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/docker/docker-agent/pkg/audio/capture"
	"github.com/docker/docker-agent/pkg/environment"
)

const (
	openAIRealtimeURL = "wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview"
	finalCommitID     = "transcribe-final-commit"
)

// ErrNotSupported is returned when transcription is unavailable on this platform.
var ErrNotSupported = errors.New("speech-to-text is only supported on macOS")

// TranscriptHandler receives completed transcripts in audio commit order.
// It must return when ctx is canceled.
type TranscriptHandler func(ctx context.Context, delta string)

type audioCapture interface {
	Run(ctx context.Context, filePath string, handler capture.AudioHandler, ready func()) error
}

// Transcriber holds configuration; each Run owns its connection and audio capture.
type Transcriber struct {
	env           environment.Provider
	endpoint      string
	capture       audioCapture
	supported     bool
	finishTimeout time.Duration
}

// New creates a transcriber without opening a connection or microphone. The
// OpenAI key is read from env on each Run, so building a TUI never triggers
// secret resolution such as an `op` unlock prompt.
func New(env environment.Provider) *Transcriber {
	return &Transcriber{
		env:       env,
		endpoint:  openAIRealtimeURL,
		capture:   capture.NewCapturer(capture.SampleRate24000),
		supported: audioSupported,
	}
}

// IsSupported reports whether microphone capture is available on this platform.
func (t *Transcriber) IsSupported() bool { return t.supported }

// Run aborts promptly on ctx cancellation. Closing finish stops capture, drains
// captured audio, and waits up to ten seconds for final transcription. A nil
// finish channel disables graceful finishing. Finishing before connection setup
// completes cancels setup without starting capture.
//
// Ready runs after capture starts, with a context canceled on finish or abort.
// Transcript handlers remain active during finish. Callbacks must honor their
// contexts; Run joins them and capture cleanup before returning. Cancellation
// itself returns nil, but capture, server and finalization errors are preserved.
func (t *Transcriber) Run(ctx context.Context, finish <-chan struct{}, handler TranscriptHandler, ready func(context.Context)) (err error) {
	if !t.IsSupported() {
		return ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return nil
	case <-finish:
		return nil
	default:
	}
	runCtx, cancel := context.WithCancel(ctx)
	captureCtx, stopCapture := context.WithCancel(runCtx)
	finished := make(chan struct{})
	var finishErr error
	go func() {
		defer close(finished)
		select {
		case <-runCtx.Done():
			return
		case <-finish:
		}
		stopCapture()
		timeout := t.finishTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-runCtx.Done():
		case <-timer.C:
			finishErr = fmt.Errorf("finish transcription: %w", context.DeadlineExceeded)
			cancel()
		}
	}()
	defer func() {
		cancel()
		stopCapture()
		<-finished
		err = errors.Join(err, finishErr)
	}()

	// The lookup can block on an `op` unlock prompt; finish and cancel must
	// interrupt it like any other setup step.
	apiKey, _ := t.env.Get(captureCtx, "OPENAI_API_KEY")
	if captureCtx.Err() != nil {
		return nil
	}
	if apiKey == "" {
		return errors.New("/speak sends audio directly to OpenAI; set a separate OPENAI_API_KEY environment variable even when chat uses Docker's models gateway, which does not support /speak")
	}

	conn, response, err := dial(captureCtx, t.endpoint, http.Header{
		"Authorization": []string{"Bearer " + apiKey},
		"OpenAI-Beta":   []string{"realtime=v1"},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if expectedClose(captureCtx, err) {
			return nil
		}
		return fmt.Errorf("connect /speak directly to OpenAI Realtime API (not Docker's models gateway): %w", err)
	}
	defer conn.Close()
	closed := make(chan struct{})
	stopClose := context.AfterFunc(runCtx, func() {
		conn.Close() // Interrupt blocked reads and writes before joining workers.
		close(closed)
	})
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	if captureCtx.Err() != nil {
		return nil
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"modalities":         []string{"text"},
			"input_audio_format": "pcm16",
			"input_audio_transcription": map[string]string{
				"model": "whisper-1",
			},
			"turn_detection": map[string]any{"type": "server_vad"},
		},
	}); err != nil {
		if expectedClose(runCtx, err) {
			return nil
		}
		return fmt.Errorf("configure transcription: %w", err)
	}

	var workers sync.WaitGroup
	var errorMu sync.Mutex
	var runErr error
	fail := func(err error) {
		if err != nil {
			errorMu.Lock()
			runErr = errors.Join(runErr, err)
			errorMu.Unlock()
			cancel()
		}
	}
	// Never block the native audio callback on network I/O.
	audio := make(chan []byte, 32)
	var overflow sync.Once
	workers.Go(func() {
		defer close(audio)
		err := t.capture.Run(captureCtx, "", func(data []byte) {
			if runCtx.Err() != nil {
				return
			}
			select {
			case audio <- data:
			default:
				overflow.Do(func() { fail(errors.New("audio upload fell behind capture")) })
			}
		}, func() {
			if ready != nil && captureCtx.Err() == nil {
				ready(captureCtx)
			}
		})
		if err != nil {
			fail(fmt.Errorf("capture audio: %w", err))
		} else if captureCtx.Err() == nil {
			fail(errors.New("audio capture stopped unexpectedly"))
		}
	})
	vadDisabled := make(chan struct{})
	workers.Go(func() {
		fail(writeAudio(runCtx, conn, audio, vadDisabled))
	})
	workers.Go(func() {
		defer cancel()
		fail(readLoop(runCtx, conn, handler, vadDisabled))
	})
	workers.Wait()
	return runErr
}

// Only suppress I/O errors attributable to our own cancellation/connection close.
func expectedClose(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, net.ErrClosed) || errors.Is(err, ctx.Err()))
}

// Gorilla applies handshake deadlines but does not close an established TCP
// connection when its context is canceled during the HTTP upgrade.
func dial(ctx context.Context, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := *websocket.DefaultDialer
	var stopClose func() bool
	var closed chan struct{}
	dialer.NetDialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		closed = make(chan struct{})
		stopClose = context.AfterFunc(ctx, func() {
			conn.Close()
			close(closed)
		})
		return conn, nil
	}
	defer func() {
		if stopClose != nil && !stopClose() {
			<-closed
		}
	}()
	return dialer.DialContext(ctx, endpoint, headers)
}

func writeAudio(ctx context.Context, conn *websocket.Conn, audio <-chan []byte, vadDisabled <-chan struct{}) error {
	write := func(event any) error {
		if ctx.Err() != nil {
			return nil
		}
		if err := conn.WriteJSON(event); err != nil && !expectedClose(ctx, err) {
			return fmt.Errorf("upload audio: %w", err)
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case data, ok := <-audio:
			if !ok {
				// Disable new VAD turns before requesting the manual final commit.
				if err := write(map[string]any{
					"type": "session.update", "session": map[string]any{"turn_detection": nil},
				}); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return nil
				case <-vadDisabled:
				}
				return write(map[string]string{"type": "input_audio_buffer.commit", "event_id": finalCommitID})
			}
			if err := write(map[string]string{
				"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(data),
			}); err != nil {
				return err
			}
		}
	}
}

type serverEvent struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Transcript string `json:"transcript"`
	Session    *struct {
		TurnDetection json.RawMessage `json:"turn_detection"`
	} `json:"session"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		EventID string `json:"event_id"`
	} `json:"error"`
}

func readLoop(ctx context.Context, conn *websocket.Conn, handler TranscriptHandler, vadDisabled chan<- struct{}) error {
	type transcript struct {
		text      string
		completed bool
		committed bool
		started   bool
		automatic bool
	}
	items := make(map[string]*transcript)
	var pending []*transcript
	var disabled, finalResolved bool
	var last rune
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if expectedClose(ctx, err) {
				return nil
			}
			return fmt.Errorf("read transcription: %w", err)
		}
		var event serverEvent
		if json.Unmarshal(msg, &event) != nil {
			continue
		}
		switch event.Type {
		case "session.updated":
			if !disabled && event.Session != nil && string(event.Session.TurnDetection) == "null" {
				disabled = true
				close(vadDisabled)
			}
		case "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "input_audio_buffer.committed", "conversation.item.input_audio_transcription.completed":
			item := items[event.ItemID]
			if item == nil {
				item = &transcript{}
				items[event.ItemID] = item
			}
			switch event.Type {
			case "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped":
				item.started = true
				if event.Type == "input_audio_buffer.speech_stopped" {
					item.automatic = true
				}
				if !item.committed {
					item.committed = true
					pending = append(pending, item)
				}
			case "input_audio_buffer.committed":
				if disabled && !item.automatic {
					// A manual commit may replace a started-only VAD item with a new ID.
					for i, previous := range pending {
						if previous != item && previous.started && !previous.automatic && !previous.completed {
							pending[i] = item
							item.committed = true
							break
						}
					}
				}
				if !item.committed {
					item.committed = true
					pending = append(pending, item)
				}
				if disabled && !item.automatic {
					finalResolved = true
				}
			case "conversation.item.input_audio_transcription.completed":
				item.text, item.completed = event.Transcript, true
			}
		case "conversation.item.input_audio_transcription.failed":
			if event.Error != nil {
				return fmt.Errorf("transcription failed for item %s (%s): %s", event.ItemID, event.Error.Code, event.Error.Message)
			}
			return fmt.Errorf("transcription failed for item %s", event.ItemID)
		case "error":
			if event.Error != nil {
				if disabled && event.Error.Code == "input_audio_buffer_commit_empty" && event.Error.EventID == finalCommitID {
					finalResolved = true
				} else {
					return fmt.Errorf("transcription server (%s): %s", event.Error.Code, event.Error.Message)
				}
			}
		}
		// Completed text is authoritative; streaming deltas can interleave across items.
		for len(pending) > 0 && pending[0].completed {
			if handler != nil && pending[0].text != "" && ctx.Err() == nil {
				text := pending[0].text
				first, _ := utf8.DecodeRuneInString(text)
				if last != 0 && !unicode.IsSpace(last) && !unicode.IsSpace(first) {
					text = " " + text
				}
				handler(ctx, text)
				last, _ = utf8.DecodeLastRuneInString(text)
			}
			pending[0].text = ""
			pending = pending[1:]
		}
		if finalResolved && len(pending) == 0 {
			return nil
		}
	}
}
