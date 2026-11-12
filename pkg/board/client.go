package board

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Event types the board reacts to on the session event stream. Every other
// runtime event is ignored.
const (
	eventStreamStarted = "stream_started"
	eventStreamStopped = "stream_stopped"
	eventSessionTitle  = "session_title"
	eventSessionExited = "session_exited"
	// eventUserMessage marks a real user prompt entering the session. The
	// runtime emits it only for human-authored turns (sub-agent and skill
	// sub-sessions suppress it), right before the turn's outermost
	// stream_started, which makes it a turn-boundary marker.
	eventUserMessage = "user_message"
	// eventError is emitted when a turn fails (model error, tool failure,
	// hook block…). Unlike stream_stopped it is delivered on the blocking
	// sink and buffered for replay, so it is the reliable failure signal.
	eventError = "error"
	// eventRuntimePaused is emitted when the run loop blocks at an iteration
	// boundary because /pause was toggled on. There is no matching resume
	// event: the loop simply starts emitting events again once resumed.
	eventRuntimePaused = "runtime_paused"
	eventGap           = "gap"
)

// reasonNormal is the stream_stopped reason for a turn that completed
// cleanly, as opposed to "error", "canceled", "hook_blocked"...
const reasonNormal = "normal"

// streamIdleTimeout is how long StreamEvents tolerates a silent connection
// once the server has proven it sends heartbeats (": ping" SSE comments,
// emitted every 15s). Three missed heartbeats means the transport is hung —
// e.g. the agent's VM was paused — not that the session is quiet, so the
// stream is aborted and the watcher reconnects. Servers that predate
// heartbeats never arm the watchdog, keeping long-lived idle streams working.
const streamIdleTimeout = 45 * time.Second

// errStreamIdle reports a stream aborted by the idle watchdog.
var errStreamIdle = errors.New("event stream idle: heartbeats stopped")

// event is the subset of a runtime event the board cares about.
type event struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	// Reason classifies how a stream ended (stream_stopped only). It is
	// authoritative for the turn's outcome, unlike mid-turn error events
	// which a parent agent may have recovered from.
	Reason string `json:"reason"`
	// Seq is the event's position in the session's buffer, parsed from the
	// SSE "id:" line. It is 0 when the server sent no id. Compared with
	// [snapshot.LastEventSeq] it tells replayed history from live events.
	Seq uint64 `json:"-"`
}

// snapshot is the part of GET /snapshot the board uses to (re)build a card's
// state and find the stream position to resume from.
type snapshot struct {
	Title        string `json:"title"`
	LastEventSeq uint64 `json:"last_event_seq"`
}

// client drives one session's control plane over its unix socket.
type client struct {
	http    *http.Client
	base    string
	session string
}

// newClient returns a client that reaches the control plane over the given
// unix socket and targets the given session id.
func newClient(socket, session string) *client {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &client{
		http:    &http.Client{Transport: transport},
		base:    "http://agent",
		session: session,
	}
}

func (c *client) endpoint(name string) string {
	return c.base + "/api/sessions/" + url.PathEscape(c.session) + "/" + name
}

// Snapshot reads the session's state and the stream position it corresponds to.
func (c *client) Snapshot(ctx context.Context) (snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("snapshot"), http.NoBody)
	if err != nil {
		return snapshot{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return snapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return snapshot{}, fmt.Errorf("snapshot: %s", resp.Status)
	}
	var snap snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	return snap, nil
}

// Followup enqueues a message to run after the current turn. A non-empty
// idempotencyKey makes the call safe to retry.
func (c *client) Followup(ctx context.Context, idempotencyKey, message string) error {
	body, err := json.Marshal(map[string]any{
		"messages": []map[string]string{{"content": message}},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("followup"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("followup: %s", resp.Status)
	}
	// Drain the small response body before closing it.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// StreamEvents tails the session event stream starting after `since` (0 from
// the beginning of the buffer). onEvent is called for every event; returning
// false stops the stream cleanly. It returns nil on a clean stop and an
// error when the connection fails.
func (c *client) StreamEvents(ctx context.Context, since uint64, onEvent func(event) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u := c.endpoint("events")
	if since > 0 {
		u += "?since=" + strconv.FormatUint(since, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// The idle watchdog is armed by the first heartbeat and re-armed by any
	// subsequent line; when it fires it cancels the request, failing the read.
	var idle atomic.Bool
	var watchdog *time.Timer
	defer func() {
		if watchdog != nil {
			watchdog.Stop()
		}
	}()
	resetWatchdog := func() {
		if watchdog != nil {
			watchdog.Reset(streamIdleTimeout)
		}
	}

	var seq uint64
	for scanner.Scan() {
		line := scanner.Text()
		resetWatchdog()
		if strings.HasPrefix(line, ":") {
			// Heartbeat comment: the server sends them, so silence now means
			// a hung transport. Arm the watchdog on the first one.
			if watchdog == nil {
				watchdog = time.AfterFunc(streamIdleTimeout, func() {
					idle.Store(true)
					cancel()
				})
			}
			continue
		}
		if id, ok := strings.CutPrefix(line, "id:"); ok {
			seq, _ = strconv.ParseUint(strings.TrimSpace(id), 10, 64)
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev event
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) != nil {
			continue
		}
		ev.Seq = seq
		seq = 0
		if !onEvent(ev) {
			return nil
		}
	}
	if idle.Load() {
		return errStreamIdle
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("event stream closed")
}
