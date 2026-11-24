package animation

import (
	"slices"
	"time"
)

// Spinner is an ordered animation with a default wall-clock cadence.
type Spinner struct {
	frames        Frames
	frameDuration time.Duration
}

// NewSpinner creates a duration-aware spinner from raw frames.
func NewSpinner(frames Frames, frameDuration time.Duration) Spinner {
	if frameDuration <= 0 {
		frameDuration = DefaultSpinnerFrameDuration
	}
	return Spinner{frames: slices.Clone(frames), frameDuration: frameDuration}
}

func (s Spinner) FrameAt(elapsed time.Duration) string {
	return s.frames.TimedFrame(elapsed, s.frameDuration)
}

func (s Spinner) FrameEveryAt(elapsed, frameDuration time.Duration) string {
	return s.frames.TimedFrame(elapsed, frameDuration)
}

func (s Spinner) FrameIndexAt(elapsed time.Duration) int {
	return s.frames.TimedFrameIndex(elapsed, s.frameDuration)
}

func (s Spinner) FrameIndexEveryAt(elapsed, frameDuration time.Duration) int {
	return s.frames.TimedFrameIndex(elapsed, frameDuration)
}

func (s Spinner) StepAt(elapsed time.Duration) int { return TimedStep(elapsed, s.frameDuration) }
func (s Spinner) StepEveryAt(elapsed, frameDuration time.Duration) int {
	return TimedStep(elapsed, frameDuration)
}
func (s Spinner) Frames() Frames                      { return slices.Clone(s.frames) }
func (s Spinner) DefaultFrameDuration() time.Duration { return s.frameDuration }
func (s Spinner) Len() int                            { return len(s.frames) }

var (
	Chat    = NewSpinner(Frames{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}, ChatSpinnerFrameDuration)
	TabBusy = NewSpinner(Frames{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}, CardSpinnerFrameDuration)
	Card    = NewSpinner(Frames{"◓", "◑", "◒", "◐"}, CardSpinnerFrameDuration)
)

// Frames is an ordered set of spinner characters.
type Frames []string

func (f Frames) TimedFrame(elapsed, frameDuration time.Duration) string {
	if len(f) == 0 {
		return ""
	}
	return f[f.TimedFrameIndex(elapsed, frameDuration)]
}

func (f Frames) TimedFrameIndex(elapsed, frameDuration time.Duration) int {
	if len(f) == 0 {
		return 0
	}
	return TimedStep(elapsed, frameDuration) % len(f)
}

// TimedStep returns the zero-based discrete step at elapsed time.
func TimedStep(elapsed, frameDuration time.Duration) int {
	if frameDuration <= 0 {
		frameDuration = DefaultSpinnerFrameDuration
	}
	return int(max(elapsed, 0) / frameDuration)
}
