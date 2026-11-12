// Package capture provides microphone capture using CoreAudio on macOS.
package capture

import "errors"

// Common sample rates.
const (
	SampleRate44100 = 44100 // CD quality, used for WAV recording
	SampleRate24000 = 24000 // Required by OpenAI Realtime API
)

// ErrNotSupported is returned when audio capture is unavailable on the current platform.
var ErrNotSupported = errors.New("audio capture is only supported on macOS")

// AudioHandler receives mono, 16-bit signed PCM (little-endian) audio chunks.
type AudioHandler func(data []byte)

// Capturer configures independent microphone capture runs.
type Capturer struct {
	sampleRate int
}

// NewCapturer creates a capturer with the specified sample rate.
func NewCapturer(sampleRate int) *Capturer {
	return &Capturer{sampleRate: sampleRate}
}
