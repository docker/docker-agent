package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildRedirectURI tests the pure string-substitution logic without
// needing to open a listener.
func TestBuildRedirectURI(t *testing.T) {
	t.Parallel()

	const fallback = "http://127.0.0.1:12345/callback"
	const port = 54321

	tests := []struct {
		name     string
		override string
		want     string
	}{
		{
			name:     "empty override falls back",
			override: "",
			want:     fallback,
		},
		{
			name:     "no placeholder returns override verbatim",
			override: "https://oauth.example.com/callback",
			want:     "https://oauth.example.com/callback",
		},
		{
			name:     "single placeholder is substituted",
			override: "https://oauth.example.com/redirect?port=${callbackPort}",
			want:     fmt.Sprintf("https://oauth.example.com/redirect?port=%d", port),
		},
		{
			name:     "multiple placeholders all substituted",
			override: "https://host:${callbackPort}/x/${callbackPort}",
			want:     fmt.Sprintf("https://host:%d/x/%d", port, port),
		},
		{
			name:     "unrelated dollar sequences are left alone",
			override: "https://x.example/cb?s=$other&p=${callbackPort}",
			want:     fmt.Sprintf("https://x.example/cb?s=$other&p=%d", port),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRedirectURI(tt.override, fallback, port)
			if got != tt.want {
				t.Errorf("buildRedirectURI(%q, %q, %d) = %q, want %q", tt.override, fallback, port, got, tt.want)
			}
		})
	}
}

// newTestTransport returns a transport private to the test. Parallel tests
// must not share http.DefaultTransport: httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections, which can break another
// test's in-flight request ("transport connection broken").
func newTestTransport(t *testing.T) *http.Transport {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func TestRequestAuthorizationCodeSuccess(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: newTestTransport(t), Timeout: 5 * time.Second}
	var address CallbackAddress
	code, state, err := requestAuthorizationCode(t.Context(), 0, func(ctx context.Context, bound CallbackAddress) (AuthorizationRequest, error) {
		address = bound
		require.Positive(t, bound.Port())
		require.Equal(t, fmt.Sprintf("https://proxy/cb?port=%d", bound.Port()), bound.ResolveRedirectURI("https://proxy/cb?port=${callbackPort}"))
		return AuthorizationRequest{URL: bound.RedirectURI() + "?code=authcode&state=expected", State: "expected"}, nil
	}, (&net.ListenConfig{}).Listen, func(ctx context.Context, authURL string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, http.NoBody)
		require.NoError(t, err)
		response, err := client.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode)
		assert.True(t, strings.HasSuffix(strings.TrimSpace(string(body)), "</html>"))
		return nil
	}, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "authcode", code)
	assert.Equal(t, "expected", state)
	assertCallbackPortReleased(t, address)
}

func TestRequestAuthorizationCodeOccupiedPort(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	_, _, err = RequestAuthorizationCode(t.Context(), port, func(context.Context, CallbackAddress) (AuthorizationRequest, error) {
		t.Fatal("prepare called before a successful bind")
		return AuthorizationRequest{}, nil
	})
	require.ErrorContains(t, err, "failed to start callback server")
}

func TestRequestAuthorizationCodeFailuresReleasePort(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"prepare", "empty state", "browser", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New(kind)
			var address CallbackAddress
			_, _, err := requestAuthorizationCode(ctx, 0, func(_ context.Context, bound CallbackAddress) (AuthorizationRequest, error) {
				address = bound
				if kind == "prepare" {
					return AuthorizationRequest{}, want
				}
				state := "expected"
				if kind == "empty state" {
					state = ""
				}
				return AuthorizationRequest{URL: "https://auth.example/", State: state}, nil
			}, (&net.ListenConfig{}).Listen, func(context.Context, string) error {
				if kind == "browser" {
					return want
				}
				cancel()
				return nil
			}, time.Second)
			switch kind {
			case "empty state":
				require.ErrorContains(t, err, "expected state must not be empty")
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.ErrorIs(t, err, want)
			}
			assertCallbackPortReleased(t, address)
		})
	}
}

func TestRequestAuthorizationCodeCanceledBeforeBinding(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := requestAuthorizationCode(ctx, 0, nil, func(context.Context, string, string) (net.Listener, error) {
		t.Fatal("canceled flow attempted to bind")
		return nil, nil
	}, nil, time.Second)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCallbackHandlerResults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, query, message string
		status               int
	}{
		{"success", "code=value&state=expected", "", http.StatusOK},
		{"provider error", "error=denied&error_description=%3Cscript%3E", "denied: <script>", http.StatusBadRequest},
		{"missing code", "state=expected", "no authorization code", http.StatusBadRequest},
		{"wrong state", "code=value&state=wrong", "state mismatch", http.StatusBadRequest},
		{"empty state", "code=value", "state mismatch", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := &callbackHandler{result: make(chan callbackResult, 1)}
			state := "expected"
			handler.expectedState.Store(&state)
			response := httptest.NewRecorder()
			handler.handleCallback(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback?"+tc.query, http.NoBody))
			assert.Equal(t, tc.status, response.Code)
			result := <-handler.result
			if tc.message == "" {
				require.NoError(t, result.err)
				assert.Equal(t, "value", result.code)
			} else {
				require.ErrorContains(t, result.err, tc.message)
			}
			if tc.name == "provider error" {
				assert.Contains(t, response.Body.String(), "&lt;script&gt;")
			}
			duplicate := httptest.NewRecorder()
			handler.handleCallback(duplicate, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback?code=second&state=expected", http.NoBody))
			assert.Equal(t, http.StatusConflict, duplicate.Code, "first result remains terminal after consumption")
			assert.Empty(t, handler.result)
		})
	}
}

func TestCallbackHandlerRejectsBeforeStatePublished(t *testing.T) {
	t.Parallel()
	handler := &callbackHandler{result: make(chan callbackResult, 1)}
	response := httptest.NewRecorder()
	handler.handleCallback(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback?code=value&state=expected", http.NoBody))
	assert.Equal(t, http.StatusBadRequest, response.Code)
	require.ErrorContains(t, (<-handler.result).err, "state mismatch")
}

func TestCallbackHandlerConcurrentDuplicates(t *testing.T) {
	t.Parallel()
	handler := &callbackHandler{result: make(chan callbackResult, 1)}
	state := "expected"
	handler.expectedState.Store(&state)
	var handlers sync.WaitGroup
	statuses := make(chan int, 32)
	for range 32 {
		handlers.Go(func() {
			response := httptest.NewRecorder()
			handler.handleCallback(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback?code=value&state=expected", http.NoBody))
			statuses <- response.Code
		})
	}
	handlers.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else {
			assert.Equal(t, http.StatusConflict, status)
		}
	}
	assert.Equal(t, 1, successes)
	require.NoError(t, (<-handler.result).err)
}

func TestRequestAuthorizationCodeServeFailureCancelsPrepare(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	address := CallbackAddress{port: listener.Addr().(*net.TCPAddr).Port}
	_, _, err = requestAuthorizationCode(t.Context(), 0, func(ctx context.Context, _ CallbackAddress) (AuthorizationRequest, error) {
		require.NoError(t, listener.Close())
		<-ctx.Done()
		return AuthorizationRequest{}, ctx.Err()
	}, func(context.Context, string, string) (net.Listener, error) { return listener, nil }, func(context.Context, string) error {
		t.Error("browser opened after serve failure")
		return nil
	}, time.Second)
	require.ErrorContains(t, err, "callback server failed")
	assertCallbackPortReleased(t, address)
}

func assertCallbackPortReleased(t *testing.T, address CallbackAddress) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", address.Port()))
	require.NoError(t, err, "callback port remains bound")
	require.NoError(t, listener.Close())
}

func TestRequestAuthorizationCodeConcurrentRuns(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: newTestTransport(t), Timeout: 5 * time.Second}
	var runs sync.WaitGroup
	for i := range 8 {
		runs.Go(func() {
			want := fmt.Sprintf("code-%d", i)
			code, state, err := requestAuthorizationCode(t.Context(), 0, func(_ context.Context, address CallbackAddress) (AuthorizationRequest, error) {
				return AuthorizationRequest{URL: address.RedirectURI() + "?code=" + want + "&state=" + want, State: want}, nil
			}, (&net.ListenConfig{}).Listen, func(ctx context.Context, u string) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
				if err != nil {
					return err
				}
				response, err := client.Do(req)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				_, err = io.Copy(io.Discard, response.Body)
				return err
			}, time.Second)
			if !assert.NoError(t, err) { //nolint:testifylint // A worker must return, not call FailNow.
				return
			}
			assert.Equal(t, want, code)
			assert.Equal(t, want, state)
		})
	}
	runs.Wait()
}

func TestRequestAuthorizationCodeForcesIdleConnectionClosed(t *testing.T) {
	t.Parallel()
	var conn net.Conn
	var address CallbackAddress
	_, _, err := requestAuthorizationCode(t.Context(), 0, func(ctx context.Context, bound CallbackAddress) (AuthorizationRequest, error) {
		address = bound
		var err error
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", bound.Port()))
		if err != nil {
			return AuthorizationRequest{}, err
		}
		// Leave an incomplete HTTP request to exercise bounded shutdown.
		if _, err = io.WriteString(conn, "GET /callback HTTP/1.1\r\n"); err != nil {
			return AuthorizationRequest{}, err
		}
		return AuthorizationRequest{URL: "https://auth.example", State: "expected"}, nil
	}, (&net.ListenConfig{}).Listen, func(context.Context, string) error { return errors.New("browser failed") }, 20*time.Millisecond)
	require.ErrorContains(t, err, "browser failed")
	require.NotNil(t, conn)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	if timeout, ok := errors.AsType[net.Error](err); ok {
		require.False(t, timeout.Timeout(), "forced close left the connection open")
	}
	assertCallbackPortReleased(t, address)
}

func TestRequestAuthorizationCodeWaitsForResponseWrite(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	entered, release, clientDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	wrapped := &delayedWriteListener{Listener: listener, entered: entered, release: release}
	client := &http.Client{Transport: newTestTransport(t), Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, _, err := requestAuthorizationCode(ctx, 0, func(_ context.Context, address CallbackAddress) (AuthorizationRequest, error) {
			return AuthorizationRequest{URL: address.RedirectURI() + "?code=ok&state=expected", State: "expected"}, nil
		}, func(context.Context, string, string) (net.Listener, error) { return wrapped, nil }, func(ctx context.Context, u string) error {
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
				if err == nil {
					var response *http.Response
					response, err = client.Do(req)
					if err == nil {
						var body []byte
						body, err = io.ReadAll(response.Body)
						response.Body.Close()
						if err == nil && !strings.HasSuffix(strings.TrimSpace(string(body)), "</html>") {
							err = errors.New("response truncated")
						}
					}
				}
				clientDone <- err
			}()
			return nil
		}, time.Second)
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("response write never started")
	}
	select {
	case err := <-finished:
		t.Fatalf("returned before response was written: %v", err)
	default:
	}
	close(release)
	require.NoError(t, <-clientDone)
	require.NoError(t, <-finished)
}

type delayedWriteListener struct {
	net.Listener

	entered chan struct{}
	release chan struct{}
}

func (l *delayedWriteListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &delayedWriteConn{Conn: conn, entered: l.entered, release: l.release}, nil
}

type delayedWriteConn struct {
	net.Conn

	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *delayedWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.Write(p)
}

func TestRequestAuthorizationCodeCanceledTransferredListener(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	address := CallbackAddress{port: listener.Addr().(*net.TCPAddr).Port}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = RequestAuthorizationCodeOnListenerForTesting(ctx, listener, nil)
	require.ErrorIs(t, err, context.Canceled)
	assertCallbackPortReleased(t, address)
}
