package board

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveUnix runs an HTTP handler on a unix socket and returns the socket path.
func serveUnix(t *testing.T, handler http.Handler) string {
	t.Helper()
	// Not t.TempDir(): its per-test path is long enough to overflow the
	// ~104-byte unix sun_path limit under long test names.
	dir, err := os.MkdirTemp("", "board-client") //nolint:forbidigo,usetesting // see above
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "cp.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func TestClientUnixSocket(t *testing.T) {
	t.Parallel()
	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/sessions/sess-1/events", r.URL.Path)
		assert.Equal(t, "7", r.URL.Query().Get("since"))
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		fmt.Fprint(w, "id: 8\ndata: {\"type\":\"stream_stopped\"}\n\n")
	}))

	var got []event
	err := newClient(socket, "sess-1").StreamEvents(t.Context(), 7, func(ev event) bool {
		got = append(got, ev)
		return false
	})
	require.NoError(t, err)
	assert.Equal(t, []event{{Type: eventStreamStopped, Seq: 8}}, got)
}

// Heartbeats stay invisible to callbacks and arm the idle watchdog.
func TestStreamEventsIdleWatchdogAbortsSilentStream(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, ": ping\n\ndata: {\"type\":\"stream_started\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		c := &client{http: srv.Client(), base: srv.URL, session: "sess-1"}
		var got []event
		done := make(chan error, 1)
		go func() {
			done <- c.StreamEvents(t.Context(), 0, func(ev event) bool {
				got = append(got, ev)
				return true
			})
		}()

		synctest.Wait()
		assert.Equal(t, []event{{Type: eventStreamStarted}}, got)
		synctest.Sleep(streamIdleTimeout - time.Nanosecond)
		select {
		case err := <-done:
			t.Fatalf("stream ended before the idle timeout: %v", err)
		default:
		}
		synctest.Sleep(time.Nanosecond)
		select {
		case err := <-done:
			require.ErrorIs(t, err, errStreamIdle)
		default:
			t.Fatal("stream did not end at the idle timeout")
		}
	})
}

func TestStreamEventsHeartbeatLinesResetWatchdog(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for range 6 {
				fmt.Fprint(w, ": ping\n")
				w.(http.Flusher).Flush()
				time.Sleep(15 * time.Second) //nolint:forbidigo // Simulates the production heartbeat cadence in fake time.
			}
			fmt.Fprint(w, "data: {\"type\":\"stream_stopped\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		c := &client{http: srv.Client(), base: srv.URL, session: "sess-1"}
		start := time.Now()
		var got []event
		err := c.StreamEvents(t.Context(), 0, func(ev event) bool {
			got = append(got, ev)
			return false
		})
		require.NoError(t, err)
		assert.Equal(t, []event{{Type: eventStreamStopped}}, got)
		assert.Equal(t, 90*time.Second, time.Since(start))
	})
}

// Older servers without heartbeats never arm the watchdog.
func TestStreamEventsNoHeartbeatNoWatchdog(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "data: {\"type\":\"stream_started\"}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(2 * streamIdleTimeout) //nolint:forbidigo // Quiet fake time exceeds the armed watchdog's limit.
			fmt.Fprint(w, "data: {\"type\":\"stream_stopped\"}\n\n")
		}))
		c := &client{http: srv.Client(), base: srv.URL, session: "sess-1"}
		start := time.Now()
		var got []event
		err := c.StreamEvents(t.Context(), 0, func(ev event) bool {
			got = append(got, ev)
			return len(got) < 2
		})
		require.NoError(t, err)
		assert.Equal(t, []event{{Type: eventStreamStarted}, {Type: eventStreamStopped}}, got)
		assert.Equal(t, 2*streamIdleTimeout, time.Since(start))
	})
}
