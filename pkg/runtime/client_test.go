package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClient_StreamSessionEvents_DeliversMultipleEvents verifies that the
// SSE stream stays open across multiple events instead of being torn down
// when StreamSessionEvents returns. This is a regression test for a bug
// where a deferred cancel() on the streaming context killed the in-flight
// HTTP request as soon as the function returned, turning the stream into
// a one-shot read.
func TestClient_StreamSessionEvents_DeliversMultipleEvents(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter must support flushing")
			return
		}

		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"type\":\"session_title\",\"session_id\":\"s\",\"title\":\"t%d\"}\n\n", i)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL)
	require.NoError(t, err)

	ch, err := c.StreamSessionEvents(t.Context(), "s")
	require.NoError(t, err)

	var titles []string
	for ev := range ch {
		titleEv, ok := ev.(*SessionTitleEvent)
		if !ok {
			continue
		}
		titles = append(titles, titleEv.Title)
	}

	assert.Equal(t, []string{"t1", "t2", "t3"}, titles)
}

// TestClient_StreamSessionEvents_StopsWhenContextCancelled verifies that
// cancelling the caller's context tears down the stream and closes the
// returned channel.
func TestClient_StreamSessionEvents_StopsWhenContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, "data: {\"type\":\"session_title\",\"session_id\":\"s\",\"title\":\"x\"}\n\n")
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	ch, err := c.StreamSessionEvents(ctx, "s")
	require.NoError(t, err)

	// Drain at least one event to confirm the stream is live.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no events received before cancel")
	}

	cancel()

	// Channel must close in a bounded time after cancel.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel was not closed after context cancel")
		}
	}
}
