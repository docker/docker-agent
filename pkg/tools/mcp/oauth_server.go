package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// CallbackServer handles OAuth callback requests
type CallbackServer struct {
	server   *http.Server
	listener net.Listener
	mu       sync.Mutex

	// resultCh delivers the outcome of the first received callback.
	// It is buffered (size 1) and all sends are non-blocking so that a
	// stray duplicate or attacker-triggered callback cannot wedge the
	// HTTP handler goroutine on a full channel.
	resultCh chan callbackResult

	// Expected state parameter for CSRF protection
	expectedState string
}

// callbackResult is the outcome of a single OAuth callback. Exactly one
// of err / (code, state) is set.
type callbackResult struct {
	code  string
	state string
	err   error
}

// NewCallbackServer creates a new OAuth callback server on a random available port
func NewCallbackServer() (*CallbackServer, error) {
	return NewCallbackServerOnPort(0)
}

// NewCallbackServerOnPort creates a new OAuth callback server on a specific port.
// Use port 0 to let the OS pick a random available port.
func NewCallbackServerOnPort(port int) (*CallbackServer, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to find available port: %w", err)
	}

	cs := &CallbackServer{
		listener: listener,
		resultCh: make(chan callbackResult, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cs.handleCallback)

	// Wrap with otelhttp so the OAuth callback span chains onto the
	// caller's trace when the OAuth provider preserves trace context
	// in the redirect (most don't, but the wrap is harmless when
	// they don't, and useful when they do).
	cs.server = &http.Server{
		Handler:      otelhttp.NewHandler(mux, "oauth.callback"),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return cs, nil
}

func (cs *CallbackServer) Start() error {
	go func() {
		if err := cs.server.Serve(cs.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Callback server error", "error", err)
		}
	}()

	slog.Info("OAuth callback server started", "address", cs.GetRedirectURI())
	return nil
}

func (cs *CallbackServer) GetRedirectURI() string {
	addr := cs.listener.Addr().String()
	return fmt.Sprintf("http://%s/callback", addr)
}

// Port returns the local TCP port the callback server is listening on.
// This is useful when a fixed port was not requested (i.e. port 0 was
// passed) and the caller needs to know which port the OS assigned.
func (cs *CallbackServer) Port() int {
	if tcpAddr, ok := cs.listener.Addr().(*net.TCPAddr); ok {
		return tcpAddr.Port
	}
	// The listener is always created via net.Listen("tcp", ...), so the
	// address is always a *net.TCPAddr. Log defensively in case that ever
	// changes; returning 0 here would silently produce a broken redirect URI.
	slog.Warn("Unexpected callback server listener address type", "addr", fmt.Sprintf("%T", cs.listener.Addr()))
	return 0
}

// resolveRedirectURI returns the OAuth redirect URI to advertise to the
// authorization server.
//
// When callbackRedirectURL is empty, the local callback server's URI is
// returned unchanged (http://127.0.0.1:{port}/callback).
//
// When callbackRedirectURL is set, it is returned verbatim except that any
// occurrence of the literal placeholder ${callbackPort} is replaced with
// the actual port the local callback server is listening on. The external
// URL is expected to eventually redirect the browser back to the local
// callback server, preserving the OAuth query parameters.
func (cs *CallbackServer) resolveRedirectURI(callbackRedirectURL string) string {
	return buildRedirectURI(callbackRedirectURL, cs.GetRedirectURI(), cs.Port())
}

// buildRedirectURI is the pure string-handling core of resolveRedirectURI,
// factored out so it can be unit-tested without starting a listener.
func buildRedirectURI(override, fallback string, port int) string {
	if override == "" {
		return fallback
	}
	return strings.ReplaceAll(override, "${callbackPort}", strconv.Itoa(port))
}

func (cs *CallbackServer) SetExpectedState(state string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.expectedState = state
}

func (cs *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if errMsg := query.Get("error"); errMsg != "" {
		errDesc := query.Get("error_description")
		if errDesc != "" {
			errMsg = fmt.Sprintf("%s: %s", errMsg, errDesc)
		}

		if !cs.deliver(callbackResult{err: fmt.Errorf("OAuth error: %s", errMsg)}) {
			writeAlreadyProcessed(w)
			return
		}

		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
    <title>Authorization Failed</title>
    <style>
        body { font-family: Arial, sans-serif; padding: 50px; text-align: center; }
        .error { color: #d32f2f; }
    </style>
</head>
<body>
    <h1 class="error">Authorization Failed</h1>
    <p>%s</p>
    <p>You can close this window.</p>
</body>
</html>`, html.EscapeString(errMsg))
		return
	}

	code := query.Get("code")
	state := query.Get("state")

	if code == "" {
		if !cs.deliver(callbackResult{err: errors.New("no authorization code received")}) {
			writeAlreadyProcessed(w)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "No authorization code received")
		return
	}

	// Verify state parameter for CSRF protection.
	// Reject the callback if the expected state has not been set yet
	// (i.e. the callback arrived before SetExpectedState was called).
	cs.mu.Lock()
	expectedState := cs.expectedState
	cs.mu.Unlock()

	if expectedState == "" || subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 {
		// Don't leak whether a flow is in progress: respond identically
		// regardless of whether deliver succeeded.
		cs.deliver(callbackResult{err: errors.New("OAuth state mismatch (possible CSRF attempt or stale callback)")})
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Invalid state parameter")
		return
	}

	if !cs.deliver(callbackResult{code: code, state: state}) {
		// A previous callback already won the race. Tell the browser the
		// flow is already complete instead of misleadingly claiming this
		// stray request succeeded.
		writeAlreadyProcessed(w)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head>
    <title>Authorization Successful</title>
    <style>
        body { font-family: Arial, sans-serif; padding: 50px; text-align: center; }
        .success { color: #388e3c; }
    </style>
</head>
<body>
    <h1 class="success">Authorization Successful!</h1>
    <p>You have successfully authorized the application.</p>
    <p>You can close this window and return to the application.</p>
</body>
</html>`)
}

// deliver attempts to publish r on resultCh without blocking. The first
// callback wins (returns true); later callbacks (stale browser tabs,
// duplicate clicks, any local process probing the loopback port) are
// dropped on the floor (returns false) instead of pinning the HTTP
// handler goroutine on a full channel.
func (cs *CallbackServer) deliver(r callbackResult) bool {
	select {
	case cs.resultCh <- r:
		return true
	default:
		return false
	}
}

// writeAlreadyProcessed responds to a stray duplicate callback with HTTP
// 409 Conflict and a short HTML page. Returning a distinct status code
// rather than another "Authorization Successful!" page avoids misleading
// the user who reloaded the browser tab while still completing the request
// promptly so the handler goroutine doesn't linger.
func writeAlreadyProcessed(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head>
    <title>Authorization Already Processed</title>
    <style>
        body { font-family: Arial, sans-serif; padding: 50px; text-align: center; }
    </style>
</head>
<body>
    <h1>Authorization Already Processed</h1>
    <p>This authorization callback has already been handled.</p>
    <p>You can close this window.</p>
</body>
</html>`)
}

func (cs *CallbackServer) WaitForCallback(ctx context.Context) (code, state string, err error) {
	select {
	case r := <-cs.resultCh:
		return r.code, r.state, r.err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (cs *CallbackServer) Shutdown(ctx context.Context) error {
	if cs.server != nil {
		return cs.server.Shutdown(ctx)
	}
	return nil
}
