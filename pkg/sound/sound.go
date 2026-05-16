// Package sound provides cross-platform sound notification support.
// It plays system sounds asynchronously to notify users of task completion or failure.
package sound

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"
)

// Event represents the type of sound to play.
type Event int

const (
	// Success is played when a task completes successfully.
	Success Event = iota
	// Failure is played when a task fails.
	Failure
)

// Play plays a notification sound for the given event in the background.
// It is non-blocking and safe to call from any goroutine.
// If the sound cannot be played, the error is logged and silently ignored.
func Play(event Event) {
	go func() {
		if err := playSound(event); err != nil {
			slog.Debug("Failed to play sound", "event", event, "error", err)
		}
	}()
}

func playSound(event Event) error {
	switch runtime.GOOS {
	case "darwin":
		return playDarwin(event)
	case "linux":
		return playLinux(event)
	case "windows":
		return playWindows(event)
	default:
		return nil
	}
}

// runDetached executes a command for fire-and-forget audio playback. The
// process is short-lived and not tied to any caller-provided context.
func runDetached(name string, args ...string) error {
	return exec.CommandContext(context.Background(), name, args...).Run()
}

func playDarwin(event Event) error {
	// Use macOS built-in system sounds via afplay
	var soundFile string
	switch event {
	case Success:
		soundFile = "/System/Library/Sounds/Glass.aiff"
	case Failure:
		soundFile = "/System/Library/Sounds/Basso.aiff"
	}
	return runDetached("afplay", soundFile)
}

func playLinux(event Event) error {
	// Try paplay (PulseAudio) first, then fall back to terminal bell
	var soundFile string
	switch event {
	case Success:
		soundFile = "/usr/share/sounds/freedesktop/stereo/complete.oga"
	case Failure:
		soundFile = "/usr/share/sounds/freedesktop/stereo/dialog-error.oga"
	}

	if path, err := exec.LookPath("paplay"); err == nil {
		return runDetached(path, soundFile)
	}

	// Fallback: terminal bell via printf
	return runDetached("printf", `\a`)
}

func playWindows(event Event) error {
	// Use PowerShell to play system sounds
	var script string
	switch event {
	case Success:
		script = `[System.Media.SystemSounds]::Asterisk.Play()`
	case Failure:
		script = `[System.Media.SystemSounds]::Hand.Play()`
	}
	return runDetached("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
}
