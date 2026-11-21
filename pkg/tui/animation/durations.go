package animation

import "time"

// Canonical animation durations shared across the TUI. Keeping them together
// provides consistent pacing wherever content appears, expands, hides, or
// collapses.
//
// Medium transitions remain visible without feeling sluggish, while short
// transitions keep dismissals responsive.
const (
	// MediumDuration is the canonical medium transition duration.
	MediumDuration = 500 * time.Millisecond

	// ShortDuration is the canonical short transition duration.
	ShortDuration = 350 * time.Millisecond

	// LoadingMinDuration is the minimum on-screen time for a loading
	// placeholder before it can be replaced with real content. Together with
	// MediumDuration and ShortDuration this ensures fast network responses still
	// produce a visually-pleasant transition rather than a jarring flash.
	LoadingMinDuration = 250 * time.Millisecond

	// DefaultSpinnerFrameDuration is the fallback cadence for catalog spinners
	// that do not need a specialized speed.
	DefaultSpinnerFrameDuration = 100 * time.Millisecond

	// ChatSpinnerFrameDuration is the default cadence for chat/tool activity
	// spinners.
	ChatSpinnerFrameDuration = 100 * time.Millisecond

	// CardSpinnerFrameDuration is the default cadence for compact activity
	// indicators.
	CardSpinnerFrameDuration = 125 * time.Millisecond

	// WaitingSpinnerFrameDuration is the default cadence for attention/waiting
	// indicators.
	WaitingSpinnerFrameDuration = 100 * time.Millisecond

	// BoardSpinnerFrameDuration is the cadence for busy board cards.
	BoardSpinnerFrameDuration = 120 * time.Millisecond

	// TransferRailStepDuration is the dwell on each cell of the agent-transfer rail.
	TransferRailStepDuration = 2 * (time.Second / 14)

	// TranscriptSlackRowDuration is the time to remove one blank row at the tail.
	TranscriptSlackRowDuration = time.Second / 14

	// RecordingDotsStepDuration is the cadence for voice recording indicator dots.
	RecordingDotsStepDuration = 400 * time.Millisecond

	// SpinnerLightStepDuration is the default cadence for the loading text
	// highlight sweep in the reusable spinner component.
	SpinnerLightStepDuration = 100 * time.Millisecond

	// SpinnerLightPauseDuration is the default dwell before the loading text
	// highlight reverses direction.
	SpinnerLightPauseDuration = 600 * time.Millisecond
)
