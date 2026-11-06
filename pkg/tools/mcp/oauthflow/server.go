package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// CallbackAddress describes the bound loopback endpoint for one authorization.
// It contains no server resources and may be retained after the flow returns.
type CallbackAddress struct {
	port int
}

func (a CallbackAddress) Port() int { return a.port }

func (a CallbackAddress) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", a.port)
}

// ResolveRedirectURI substitutes the bound port without rewriting the override.
func (a CallbackAddress) ResolveRedirectURI(override string) string {
	return buildRedirectURI(override, a.RedirectURI(), a.port)
}

// AuthorizationRequest is prepared after the callback endpoint is bound.
type AuthorizationRequest struct {
	URL   string
	State string
}

// PrepareAuthorization resolves credentials and builds the authorization URL.
// It runs synchronously and must honor ctx, including when serving fails.
type PrepareAuthorization func(ctx context.Context, address CallbackAddress) (AuthorizationRequest, error)

type callbackResult struct {
	code, state string
	err         error
}

// callbackHandler belongs to one invocation, never to a reusable server object.
type callbackHandler struct {
	expectedState atomic.Pointer[string]
	claimed       atomic.Bool
	result        chan callbackResult
}

// RequestAuthorizationCode binds the requested loopback port (0 selects a free
// port), prepares authorization, opens the browser and waits for the callback.
// All listener, serving and request-handler resources are joined before return.
func RequestAuthorizationCode(ctx context.Context, port int, prepare PrepareAuthorization) (string, string, error) {
	var lc net.ListenConfig
	return requestAuthorizationCode(ctx, port, prepare, lc.Listen, *browserOpener.Load(), 5*time.Second)
}

// RequestAuthorizationCodeOnListenerForTesting transfers a reserved loopback
// listener into the flow without a close/rebind race. Never use in production.
func RequestAuthorizationCodeOnListenerForTesting(ctx context.Context, listener net.Listener, prepare PrepareAuthorization) (string, string, error) {
	defer listener.Close()
	return requestAuthorizationCode(ctx, 0, prepare, func(context.Context, string, string) (net.Listener, error) {
		return listener, nil
	}, *browserOpener.Load(), 5*time.Second)
}

type callbackListen func(context.Context, string, string) (net.Listener, error)

func requestAuthorizationCode(ctx context.Context, port int, prepare PrepareAuthorization, listen callbackListen, open BrowserOpener, shutdownTimeout time.Duration) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	listener, err := listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", "", fmt.Errorf("failed to start callback server: %w", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return "", "", errors.New("callback listener must have a TCP address")
	}
	address := CallbackAddress{port: addr.Port}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	handler := &callbackHandler{result: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", handler.handleCallback)
	traced := otelhttp.NewHandler(mux, "oauth.callback")
	var requests sync.WaitGroup
	var gate sync.Mutex
	closing := false
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gate.Lock()
			if closing {
				gate.Unlock()
				return
			}
			requests.Add(1)
			gate.Unlock()
			defer requests.Done()
			traced.ServeHTTP(w, r)
		}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("callback server failed: %w", err))
		}
	}()
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cleanupCancel()
		if err := server.Shutdown(cleanupCtx); err != nil {
			slog.WarnContext(ctx, "Forcing callback server shutdown", "error", err)
			_ = server.Close()
		}
		_ = listener.Close() // Serve may not have registered before Shutdown.
		<-served
		gate.Lock()
		closing = true
		gate.Unlock()
		requests.Wait()
	}()

	if err := context.Cause(runCtx); err != nil {
		return "", "", err
	}
	request, err := prepare(runCtx, address)
	if cause := context.Cause(runCtx); cause != nil {
		return "", "", cause
	}
	if err != nil {
		return "", "", err
	}
	if request.State == "" {
		return "", "", errors.New("OAuth expected state must not be empty")
	}
	handler.expectedState.Store(&request.State)
	if err := open(runCtx, request.URL); err != nil {
		if cause := context.Cause(runCtx); cause != nil {
			return "", "", cause
		}
		return "", "", err
	}
	select {
	case <-runCtx.Done():
		return "", "", context.Cause(runCtx)
	case result := <-handler.result:
		if err := context.Cause(runCtx); err != nil {
			return "", "", err
		}
		return result.code, result.state, result.err
	}
}

// buildRedirectURI leaves configured redirect overrides otherwise untouched.
func buildRedirectURI(override, fallback string, port int) string {
	if override == "" {
		return fallback
	}
	return strings.ReplaceAll(override, "${callbackPort}", strconv.Itoa(port))
}

func (cs *callbackHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
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
	// (i.e. the callback arrived before preparation completed).
	expectedState := cs.expectedState.Load()
	if expectedState == nil || !constantTimeStateEqual(state, *expectedState) {
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
func (cs *callbackHandler) deliver(r callbackResult) bool {
	if !cs.claimed.CompareAndSwap(false, true) {
		return false
	}
	cs.result <- r // One winner and a size-one buffer; no receiver is required.
	return true
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
