//go:build !darwin || no_audio

// Package transcribe provides real-time audio transcription using OpenAI's Realtime API.
// This is a stub implementation for non-macOS platforms.
package transcribe

import (
	"context"
	"errors"
)

// ErrNotSupported is returned when transcription is not supported on the current platform.
var ErrNotSupported = errors.New("speech-to-text is only supported on macOS")

// TranscriptHandler is called when new transcription text is received.
type TranscriptHandler func(delta string)

// Transcriber provides real-time audio transcription using OpenAI's Realtime API.
// On non-macOS platforms, this is a stub that returns errors.
type Transcriber struct{}

// New creates a new Transcriber with the given OpenAI API key.
func New(apiKey string) *Transcriber {
	return &Transcriber{}
}

// Start returns ErrNotSupported on non-macOS platforms.
func (t *Transcriber) Start(ctx context.Context, handler TranscriptHandler) error {
	return ErrNotSupported
}

// Stop is a no-op on non-macOS platforms.
func (t *Transcriber) Stop() {}

// IsRunning always returns false on non-macOS platforms.
func (t *Transcriber) IsRunning() bool {
	return false
}

// IsSupported returns false on non-macOS platforms.
func (t *Transcriber) IsSupported() bool {
	return false
}
