package replay

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

// Recorder wraps a func(harness.Event) callback and writes all events to a
// NDJSON file. Used by adapter integration tests to generate fixture files.
type Recorder struct {
	inner func(harness.Event)
	mu    sync.Mutex
	w     io.Writer
}

type record struct {
	T    string        `json:"t"`
	At   time.Time     `json:"at"`
	Data harness.Event `json:"data"`
}

// NewRecorder creates a Recorder that forwards events to inner and writes
// NDJSON records to w.
func NewRecorder(inner func(harness.Event), w io.Writer) *Recorder {
	return &Recorder{inner: inner, w: w}
}

// Emit forwards the event and writes it to the NDJSON file.
func (r *Recorder) Emit(e harness.Event) {
	r.inner(e)
	rec := record{T: string(e.Type), At: time.Now(), Data: e}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.w.Write(append(line, '\n'))
}
