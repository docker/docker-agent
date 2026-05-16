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
)

// CallbackServer handles OAuth callback requests
type CallbackServer struct {
	server   *http.Server
	listener net.Listener
	mu       sync.Mutex

	// Channels for communicating the authorization code and state
	codeCh  chan string
	stateCh chan string
	errCh   chan error

	// Expected state parameter for CSRF protection
	expectedState string
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
		codeCh:   make(chan string, 1),
		stateCh:  make(chan string, 1),
		errCh:    make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cs.handleCallback)

	cs.server = &http.Server{
		Handler:      mux,
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

		cs.errCh <- fmt.Errorf("OAuth error: %s", errMsg)

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
		cs.errCh <- errors.New("no authorization code received")
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
		cs.errCh <- errors.New("OAuth state mismatch (possible CSRF attempt or stale callback)")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Invalid state parameter")
		return
	}

	cs.codeCh <- code
	cs.stateCh <- state

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

func (cs *CallbackServer) WaitForCallback(ctx context.Context) (code, state string, err error) {
	select {
	case code = <-cs.codeCh:
		select {
		case state = <-cs.stateCh:
			return code, state, nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	case err = <-cs.errCh:
		return "", "", err
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
