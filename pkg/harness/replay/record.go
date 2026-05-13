// Package replay provides recording and playback of harness event streams.
// Used by adapter integration tests to generate fixture files that can be
// replayed without the real harness binary.
package replay

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

// Recorder wraps an EventSink and writes all events to a NDJSON file.
// Each line is a JSON object with fields: t (type name), at (timestamp), data (event).
// Use NewRecorder in adapter integration tests to generate testdata/ fixtures.
type Recorder struct {
	inner harness.EventSink
	mu    sync.Mutex
	w     io.Writer
}

// NewRecorder creates a Recorder that forwards events to inner and writes
// NDJSON records to w.
func NewRecorder(inner harness.EventSink, w io.Writer) *Recorder {
	return &Recorder{inner: inner, w: w}
}

type record struct {
	T    string          `json:"t"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// Emit implements harness.EventSink.
func (r *Recorder) Emit(e harness.Event) {
	r.inner.Emit(e)
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	rec := record{T: eventTypeName(e), At: e.EventTime(), Data: data}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.w.Write(append(line, '\n'))
}

func eventTypeName(e harness.Event) string {
	switch e.(type) {
	case harness.RunStart:
		return "RunStart"
	case harness.TextStart:
		return "TextStart"
	case harness.TextDelta:
		return "TextDelta"
	case harness.TextEnd:
		return "TextEnd"
	case harness.ReasoningStart:
		return "ReasoningStart"
	case harness.ReasoningDelta:
		return "ReasoningDelta"
	case harness.ReasoningEnd:
		return "ReasoningEnd"
	case harness.ToolCallStart:
		return "ToolCallStart"
	case harness.ToolCallArgsDelta:
		return "ToolCallArgsDelta"
	case harness.ToolCallEnd:
		return "ToolCallEnd"
	case harness.ToolCallResult:
		return "ToolCallResult"
	case harness.PermissionPending:
		return "PermissionPending"
	case harness.PermissionResolved:
		return "PermissionResolved"
	case harness.Heartbeat:
		return "Heartbeat"
	case harness.RunEnd:
		return "RunEnd"
	case harness.RunError:
		return "RunError"
	default:
		return "Unknown"
	}
}
