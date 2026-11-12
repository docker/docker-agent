package a2a

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// TestEnrichCardError_RetryableStatusArmsGate verifies that a card-resolution
// failure carrying one of the fixed retryable HTTP statuses surfaces as a
// *modelerrors.StatusError, arming the StartableToolSet backoff gate exactly
// as remote MCP's enrichConnectError does (pkg/tools/mcp/remote.go).
func TestEnrichCardError_RetryableStatusArmsGate(t *testing.T) {
	t.Parallel()

	for _, status := range []int{429, 408, 500, 502, 503, 504, 529} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			toolSet := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))
			err := toolSet.Start(t.Context())
			require.Error(t, err)

			var se *modelerrors.StatusError
			require.ErrorAs(t, err, &se, "a %d response must surface as *StatusError", status)
			assert.Equal(t, status, se.StatusCode)
			assert.True(t, modelerrors.RetryableHTTPStatus(se),
				"status %d must be classified retryable by the backoff gate", status)
		})
	}
}

// TestEnrichCardError_NonRetryableStatusDoesNotArm verifies that a
// card-resolution failure carrying a non-retryable status still surfaces as
// *StatusError (for structured access) but is not classified retryable, so
// bad-config / auth / malformed-request failures fail promptly. 501 proves
// the arming set is a fixed enumeration and not a full 5xx range.
func TestEnrichCardError_NonRetryableStatusDoesNotArm(t *testing.T) {
	t.Parallel()

	for _, status := range []int{400, 401, 403, 404, 501} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			toolSet := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))
			err := toolSet.Start(t.Context())
			require.Error(t, err)

			var se *modelerrors.StatusError
			require.ErrorAs(t, err, &se, "%d must still surface as *StatusError (wrapped for structured access)", status)
			assert.Equal(t, status, se.StatusCode)
			assert.False(t, modelerrors.RetryableHTTPStatus(se),
				"status %d must NOT be classified retryable", status)
		})
	}
}

// TestEnrichCardError_NoStatusNoStatusError verifies that failures which
// never produce an HTTP response — connection refused, SSRF-blocked dial,
// or a 200 response whose body doesn't parse as an agent card — never carry
// a *StatusError, so the backoff gate does not arm on them.
func TestEnrichCardError_NoStatusNoStatusError(t *testing.T) {
	t.Parallel()

	t.Run("connection refused", func(t *testing.T) {
		t.Parallel()

		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		toolSet := NewToolset("test", "http://"+addr, nil, WithAllowPrivateIPs(true))
		err = toolSet.Start(t.Context())
		require.Error(t, err)

		var se *modelerrors.StatusError
		assert.NotErrorAs(t, err, &se, "a connection-refused failure must not carry a *StatusError")
	})

	t.Run("SSRF-blocked private IP", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		// allow_private_ips is left at its default (false): the loopback
		// httptest server must never be reached, so any 503 it would have
		// served is irrelevant to the error the toolset produces.
		toolSet := NewToolset("test", srv.URL, nil)
		err := toolSet.Start(t.Context())
		require.Error(t, err)

		var se *modelerrors.StatusError
		assert.NotErrorAs(t, err, &se, "an SSRF-blocked dial must not carry a *StatusError")
	})

	t.Run("malformed agent card", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "not valid json")
		}))
		defer srv.Close()

		toolSet := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))
		err := toolSet.Start(t.Context())
		require.Error(t, err)

		var se *modelerrors.StatusError
		assert.NotErrorAs(t, err, &se, "a malformed 200 response must not carry a *StatusError")
	})
}

// TestEnrichCardError_RetryAfterHonoured verifies that a server-supplied
// Retry-After header on the agent-card response is parsed through to the
// resulting *modelerrors.StatusError, matching the handling already in
// place for remote MCP (pkg/tools/mcp/oauth.go) and model-provider adapters.
func TestEnrichCardError_RetryAfterHonoured(t *testing.T) {
	t.Parallel()

	for _, status := range []int{503, 429} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
			}))
			defer srv.Close()

			toolSet := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))
			err := toolSet.Start(t.Context())
			require.Error(t, err)

			var se *modelerrors.StatusError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, status, se.StatusCode)
			assert.Equal(t, 120*time.Second, se.RetryAfter,
				"the server's Retry-After header must be parsed onto the StatusError")
		})
	}
}

// TestEnrichCardError_NoRetryAfterHeaderLeavesZero verifies that when the
// server does not send a Retry-After header, RetryAfter stays zero so the
// gate falls back to its own computed delay.
func TestEnrichCardError_NoRetryAfterHeaderLeavesZero(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	toolSet := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))
	err := toolSet.Start(t.Context())
	require.Error(t, err)

	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se)
	assert.Zero(t, se.RetryAfter, "no Retry-After header means the gate computes its own delay")
}
