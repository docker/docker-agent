//go:build !darwin || no_audio

// Package capture provides audio capture functionality.
// This is a stub implementation for non-macOS platforms.
package capture

import (
	"errors"
	"time"
)

// Common sample rates
const (
	SampleRate44100 = 44100 // CD quality, used for WAV recording
	SampleRate24000 = 24000 // Required by OpenAI Realtime API
)

// ErrNotSupported is returned when audio capture is not supported on the current platform.
var ErrNotSupported = errors.New("audio capture is only supported on macOS")

// AudioHandler is called for each chunk of captured audio data.
// The data is mono, 16-bit signed PCM (little-endian).
type AudioHandler func(data []byte)

// Capturer records audio from the microphone.
// On non-macOS platforms, this is a stub that returns errors.
type Capturer struct {
	sampleRate int
}

// NewCapturer creates a new audio capturer with the specified sample rate.
func NewCapturer(sampleRate int) *Capturer {
	return &Capturer{sampleRate: sampleRate}
}

// Start returns ErrNotSupported on non-macOS platforms.
func (c *Capturer) Start(filePath string, handler AudioHandler) error {
	return ErrNotSupported
}

// Stop returns ErrNotSupported on non-macOS platforms.
func (c *Capturer) Stop() error {
	return ErrNotSupported
}

// IsCapturing always returns false on non-macOS platforms.
func (c *Capturer) IsCapturing() bool {
	return false
}

// Record returns ErrNotSupported on non-macOS platforms.
func (c *Capturer) Record(filePath string, duration time.Duration) error {
	return ErrNotSupported
}

// Stream returns ErrNotSupported on non-macOS platforms.
func (c *Capturer) Stream(duration time.Duration, handler AudioHandler) error {
	return ErrNotSupported
}
