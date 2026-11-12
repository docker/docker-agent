// Package programstatus reports application state using the OSC 7501 protocol.
package programstatus

import (
	"encoding/base64"
	"io"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Status describes one aggregate application state.
type Status struct {
	State string
	Kind  string
	Msg   string
}

// Sequence encodes a root record, replacing all its previous keys.
func (s Status) Sequence() string {
	body := "state=" + s.State + ":app=docker-agent"
	if s.State == "blocked" {
		body += ":kind=" + s.Kind
	}
	if s.Msg != "" {
		body += ":msg=" + base64.StdEncoding.EncodeToString([]byte(singleLine(s.Msg)))
	}
	return "\x1b]7501;" + body + "\x1b\\"
}

func singleLine(text string) string {
	var b strings.Builder
	for _, r := range ansi.Strip(text) {
		if unicode.IsControl(r) {
			r = ' '
		}
		if b.Len()+len(string(r)) > 2048 {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// Aggregate gives unanswered prompts precedence over work and results.
// The caller supplies statuses in a stable order to break ties.
func Aggregate(statuses []Status) Status {
	result := Status{State: "idle"}
	for _, status := range statuses {
		if priority(status.State) > priority(result.State) {
			result = status
		}
	}
	return result
}

func priority(state string) int {
	switch state {
	case "blocked":
		return 4
	case "working":
		return 3
	case "error":
		return 2
	case "done":
		return 1
	default:
		return 0
	}
}

// Reporter coalesces output. All methods run on the UI loop, or after it exits.
type Reporter struct {
	current Status
	last    Status
}

// Set updates the desired status and reports whether it changed.
func (r *Reporter) Set(status Status) bool {
	if r.current == status {
		return false
	}
	r.current = status
	return true
}

// String encodes the latest state at write time, not when a command was queued.
// This prevents asynchronous Bubble Tea commands from publishing stale states.
func (r *Reporter) String() string {
	if r.current == r.last {
		return ""
	}
	r.last = r.current
	return r.current.Sequence()
}

// Finish retires transient state while preserving completed results on exit.
func (r *Reporter) Finish(out io.Writer) {
	if r.current.State == "" {
		return
	}
	if r.current.State != "done" && r.current.State != "error" {
		r.Set(Status{State: "idle"})
	}
	_, _ = io.WriteString(out, r.String())
}
