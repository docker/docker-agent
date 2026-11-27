package httpx

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
)

// SSEEvent represents a single Server-Sent Event frame.
type SSEEvent struct {
	Event string
	Data  []byte
	ID    string
}

// SSEReader reads Server-Sent Events from an io.ReadCloser.
// Unlike bufio.Scanner, it uses bufio.Reader so arbitrarily large data lines
// (e.g. inline base64 images or large tool-call payloads) are never truncated
// by a fixed token buffer limit.
type SSEReader struct {
	rc     io.ReadCloser
	br     *bufio.Reader
	event  SSEEvent
	err    error
	closed atomic.Bool
}

// NewSSEReader returns a reader that decodes SSE frames from rc.
func NewSSEReader(rc io.ReadCloser) *SSEReader {
	return &SSEReader{
		rc: rc,
		br: bufio.NewReaderSize(rc, 64*1024),
	}
}

// Next advances to the next SSE event with non-empty data.
// Returns false on EOF or error; check Err() afterwards.
func (r *SSEReader) Next() bool {
	if r.closed.Load() || r.err != nil {
		return false
	}

	var eventType string
	var eventID string
	var dataBuf bytes.Buffer
	hasData := false

	for {
		line, err := r.br.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineEnding(line)
			if len(line) == 0 {
				if hasData {
					r.event = SSEEvent{
						Event: eventType,
						Data:  slices.Clone(dataBuf.Bytes()),
						ID:    eventID,
					}
					return true
				}
				eventType = ""
				eventID = ""
				continue
			}

			// Comment lines start with ':' and are ignored.
			if line[0] != ':' {
				field, value, _ := bytes.Cut(line, []byte{':'})
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				switch string(field) {
				case "event":
					eventType = string(value)
				case "data":
					if hasData {
						dataBuf.WriteByte('\n')
					}
					dataBuf.Write(value)
					hasData = true
				case "id":
					if !bytes.ContainsRune(value, 0) {
						eventID = string(value)
					}
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				if hasData {
					r.event = SSEEvent{
						Event: eventType,
						Data:  slices.Clone(dataBuf.Bytes()),
						ID:    eventID,
					}
					r.err = io.EOF
					return true
				}
				return false
			}
			r.err = err
			return false
		}
	}
}

// Event returns the most recently read SSE event.
func (r *SSEReader) Event() SSEEvent {
	return r.event
}

// Err returns any non-EOF error encountered while reading the stream.
func (r *SSEReader) Err() error {
	if errors.Is(r.err, io.EOF) {
		return nil
	}
	return r.err
}

// Close closes the underlying reader.
func (r *SSEReader) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	if r.rc != nil {
		return r.rc.Close()
	}
	return nil
}

func trimLineEnding(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte{'\n'})
	b = bytes.TrimSuffix(b, []byte{'\r'})
	return b
}

// IsSSEDone reports whether an SSE data payload is the OpenAI "[DONE]" sentinel.
func IsSSEDone(data []byte) bool {
	return strings.TrimSpace(string(data)) == "[DONE]"
}
