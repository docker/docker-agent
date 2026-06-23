package shell

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/tools"
)

// AskpassCommandName is the hidden CLI subcommand sudo runs (via the generated
// SUDO_ASKPASS wrapper) to fetch a password. It is exported so the command
// registration in cmd/root stays in sync with the wrapper script built here.
const AskpassCommandName = "__askpass"

// Environment variables that bridge the hidden `__askpass` helper (which sudo
// runs via the generated SUDO_ASKPASS wrapper) back to the running agent.
const (
	envAskpassSocket = "CAGENT_ASKPASS_SOCKET"
	envAskpassToken  = "CAGENT_ASKPASS_TOKEN"
)

// askpassPromptTimeout caps how long a single password prompt stays open
// waiting for the user before the request is abandoned.
const askpassPromptTimeout = 2 * time.Minute

// askpassRequest is sent by the helper to the agent over the private socket.
type askpassRequest struct {
	Token  string `json:"token"`
	Prompt string `json:"prompt"`
}

// askpassResponse is the agent's reply. When OK is false the helper exits
// non-zero so sudo aborts cleanly instead of retrying the prompt.
type askpassResponse struct {
	OK       bool   `json:"ok"`
	Password string `json:"password,omitempty"`
}

// askpassSupported reports whether the sudo askpass flow can run here. It is a
// Unix-only feature (sudo + a unix socket + a /bin/sh wrapper script); Windows
// and js/wasm are no-ops.
func askpassSupported() bool {
	return runtime.GOOS != "windows" && runtime.GOOS != "js"
}

// askpassServer listens on a private unix socket for password requests coming
// from the `__askpass` helper that sudo spawns, and answers them by asking the
// user through the toolset's elicitation handler.
type askpassServer struct {
	listener net.Listener
	dir      string // 0700 temp dir holding the socket + wrapper script
	socket   string
	script   string // SUDO_ASKPASS wrapper script path
	token    string

	// handler is consulted on every request so the server always sees the
	// current elicitation handler (it is re-applied each turn).
	handler func() tools.ElicitationHandler

	// promptSem serializes prompts to one at a time (buffered, size 1). The
	// runtime elicitation pipe carries a single request, so two concurrent sudo
	// calls (e.g. `sudo a & sudo b`) must not raise two dialogs at once.
	promptSem chan struct{}

	closeOnce sync.Once
	closed    chan struct{} // closed by close() to cancel in-flight prompts
}

// sudoWordRe matches sudo as a whole word, so substrings like "pseudo",
// "sudoku" or "sudoers" do not trigger the askpass wiring.
var sudoWordRe = regexp.MustCompile(`\bsudo\b`)

// commandInvokesSudo reports whether command appears to call sudo. It uses a
// word boundary to avoid false positives on similar substrings. It can still
// match a non-invoking mention (e.g. `grep sudo file`); in that case the
// injected shell function is defined but never called and the bridge env is
// unused, so the only cost is harmless extra env on that one command.
func commandInvokesSudo(command string) bool {
	return sudoWordRe.MatchString(command)
}

// resolveSelfExecutable returns the absolute path of the running binary,
// following symlinks (it may be installed as a docker cli-plugin symlink).
func resolveSelfExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// shQuote wraps s in POSIX single quotes, escaping embedded single quotes,
// so it is safe to embed in a /bin/sh command line. Inside single quotes no
// expansion (`$`, backticks, `\`) occurs.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// startAskpassServer creates the private socket plus the SUDO_ASKPASS wrapper
// script and starts accepting connections. handler is consulted per request so
// it always reflects the current elicitation handler.
func startAskpassServer(handler func() tools.ElicitationHandler) (*askpassServer, error) {
	if !askpassSupported() {
		return nil, errors.New("sudo askpass is not supported on this platform")
	}

	exe, err := resolveSelfExecutable()
	if err != nil {
		return nil, fmt.Errorf("resolving executable for askpass: %w", err)
	}

	dir, err := os.MkdirTemp("", "cagent-askpass-")
	if err != nil {
		return nil, err
	}

	token, err := randomToken()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	socket := filepath.Join(dir, "sock")
	var lnConfig net.ListenConfig
	listener, err := lnConfig.Listen(context.Background(), "unix", socket)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	// sudo runs SUDO_ASKPASS as a single program with the prompt as its only
	// argument, so the `__askpass` subcommand cannot be embedded directly.
	// This wrapper forwards to the running binary; `--` keeps a prompt that
	// looks like a flag from being parsed as one. The executable path is
	// single-quoted (shQuote) so metacharacters in it are not interpreted.
	script := filepath.Join(dir, "askpass.sh")
	content := fmt.Sprintf("#!/bin/sh\nexec %s %s -- \"$@\"\n", shQuote(exe), AskpassCommandName)
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil { //nolint:gosec // the wrapper must be executable by its owner; the parent dir is 0700
		_ = listener.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}

	s := &askpassServer{
		listener:  listener,
		dir:       dir,
		socket:    socket,
		script:    script,
		token:     token,
		handler:   handler,
		promptSem: make(chan struct{}, 1),
		closed:    make(chan struct{}),
	}
	go s.serve()
	return s, nil
}

// env returns the environment entries to add to a shell child so `sudo -A`
// reaches back to this server.
func (s *askpassServer) env() []string {
	if s == nil {
		return nil
	}
	return []string{
		"SUDO_ASKPASS=" + s.script,
		envAskpassSocket + "=" + s.socket,
		envAskpassToken + "=" + s.token,
	}
}

func (s *askpassServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleConn(conn)
	}
}

func (s *askpassServer) handleConn(conn net.Conn) {
	defer conn.Close()

	// Bound the whole exchange; the prompt (askUser) is the only slow part.
	ctx, cancel := context.WithTimeout(context.Background(), askpassPromptTimeout)
	defer cancel()

	// The request is sent immediately, so a short read deadline is enough.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear; the prompt has its own timeout

	var req askpassRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_ = writeAskpassResponse(conn, askpassResponse{OK: false})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
		slog.WarnContext(ctx, "Rejected sudo askpass request with invalid token")
		_ = writeAskpassResponse(conn, askpassResponse{OK: false})
		return
	}

	// Cancel the prompt if the helper goes away (its command was killed/timed
	// out, so its process group was SIGTERM'd) or the server is torn down. This
	// dismisses the elicitation handler and frees the slot instead of blocking
	// on a dead client for the full prompt timeout.
	go s.watchConn(ctx, conn, cancel)

	password, ok := s.askUser(ctx, req.Prompt)
	_ = writeAskpassResponse(conn, askpassResponse{OK: ok, Password: password})
}

// watchConn cancels the prompt when the connection closes (helper died) or the
// server shuts down. It uses an inner goroutine because a net.Conn read cannot
// be selected on directly.
//
// Neither goroutine leaks: watchConn returns as soon as ctx is done (which
// handleConn always triggers via its deferred cancel()), and the inner read
// goroutine unblocks when handleConn's deferred conn.Close() runs on return,
// which makes the blocked Read fail and close connClosed.
func (s *askpassServer) watchConn(ctx context.Context, conn net.Conn, cancel context.CancelFunc) {
	connClosed := make(chan struct{})
	go func() {
		defer close(connClosed)
		// The client sends nothing more after its request; this Read blocks
		// until the connection closes (EOF/error), i.e. the helper is gone.
		_, _ = conn.Read(make([]byte, 1))
	}()
	select {
	case <-connClosed:
		cancel()
	case <-s.closed:
		cancel()
	case <-ctx.Done():
	}
}

// askUser prompts the user for the sudo password through the elicitation
// handler. It returns ("", false) when no interactive handler is available or
// the user declines/cancels (or ctx is cancelled because the helper died).
func (s *askpassServer) askUser(ctx context.Context, prompt string) (string, bool) {
	handler := s.handler()
	if handler == nil {
		return "", false
	}

	// Only one prompt at a time. If a concurrent prompt is already open, wait
	// for it (unless this request's helper goes away first, ctx cancellation).
	select {
	case s.promptSem <- struct{}{}:
		defer func() { <-s.promptSem }()
	case <-ctx.Done():
		return "", false
	}

	message := strings.TrimSpace(prompt)
	if message == "" {
		message = "sudo is requesting your password."
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"password": map[string]any{
				"type":   "string",
				"title":  "Password",
				"format": "password",
			},
		},
		"required": []any{"password"},
	}

	result, err := handler(ctx, &mcp.ElicitParams{
		Message:         message,
		RequestedSchema: schema,
		Meta:            mcp.Meta{"cagent/title": "sudo password"},
	})
	if err != nil || result.Action != tools.ElicitationActionAccept {
		return "", false
	}
	pw, _ := result.Content["password"].(string)
	if pw == "" {
		return "", false
	}
	return pw, true
}

func (s *askpassServer) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.listener.Close()
		_ = os.RemoveAll(s.dir)
	})
}

func writeAskpassResponse(conn net.Conn, resp askpassResponse) error {
	// The password is the payload we must return to the askpass helper over
	// the private local socket; this is the intended transmission, not a leak.
	data, err := json.Marshal(resp) //nolint:gosec // the password is the intended payload returned to the askpass helper over the private local socket
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write(append(data, '\n'))
	return err
}

// RunAskpassClient is the body of the hidden `__askpass` subcommand. sudo runs
// it (through the generated wrapper script) to obtain a password: it dials the
// agent's private socket, forwards the prompt, and prints the returned password
// to out. It returns an error (a non-zero exit, which tells sudo to abort) when
// the socket is unavailable or the user declines.
func RunAskpassClient(ctx context.Context, prompt string, out io.Writer) error {
	socket := os.Getenv(envAskpassSocket)
	token := os.Getenv(envAskpassToken)
	if socket == "" || token == "" {
		return errors.New("askpass: not running under a docker-agent shell (missing socket env)")
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "unix", socket)
	if err != nil {
		return fmt.Errorf("askpass: dial: %w", err)
	}
	defer conn.Close()

	req, err := json.Marshal(askpassRequest{Token: token, Prompt: prompt})
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("askpass: send: %w", err)
	}

	// The user may take a while to type; allow a little more than the server's
	// own prompt timeout.
	_ = conn.SetReadDeadline(time.Now().Add(askpassPromptTimeout + 30*time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("askpass: read: %w", err)
	}
	var resp askpassResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("askpass: decode: %w", err)
	}
	if !resp.OK {
		return errors.New("askpass: password request was declined")
	}
	if _, err := io.WriteString(out, resp.Password+"\n"); err != nil {
		return err
	}
	return nil
}

// posixShellForFunc reports whether the configured shell understands the POSIX
// function syntax used to force `sudo -A`. Non-POSIX shells (e.g. fish) are
// left untouched.
func posixShellForFunc(shell string) bool {
	switch filepath.Base(shell) {
	case "sh", "bash", "zsh", "dash", "ash", "ksh", "mksh":
		return true
	}
	return false
}

// wrapSudoCommand prepends a shell function that transparently adds `-A` to
// every sudo invocation so sudo uses SUDO_ASKPASS instead of a TTY. It only
// rewrites commands that invoke sudo, and only for POSIX shells.
func wrapSudoCommand(command, shell string) string {
	if !commandInvokesSudo(command) || !posixShellForFunc(shell) {
		return command
	}
	return "sudo() { command sudo -A \"$@\"; }\n" + command
}

// --- shellHandler askpass glue ---

func (h *shellHandler) setElicitationHandler(handler tools.ElicitationHandler) {
	h.elicitationMu.Lock()
	h.elicitationHandler = handler
	h.elicitationMu.Unlock()
}

func (h *shellHandler) currentElicitationHandler() tools.ElicitationHandler {
	h.elicitationMu.RLock()
	defer h.elicitationMu.RUnlock()
	return h.elicitationHandler
}

// askpassActive reports whether the sudo askpass flow should be wired into the
// next command: enabled in config, supported on this platform, and an
// interactive elicitation handler is available to answer the prompt.
func (h *shellHandler) askpassActive() bool {
	return h.sudoAskpass && askpassSupported() && h.currentElicitationHandler() != nil
}

// ensureAskpass lazily starts the askpass server the first time it is needed.
// It returns nil (askpass disabled for this command) on any startup failure.
// The mutex makes lazy start safe against concurrent commands and a concurrent
// stopAskpass.
func (h *shellHandler) ensureAskpass() *askpassServer {
	h.askpassMu.Lock()
	defer h.askpassMu.Unlock()
	if h.askpassStarted {
		return h.askpass
	}
	h.askpassStarted = true
	srv, err := startAskpassServer(h.currentElicitationHandler)
	if err != nil {
		slog.Warn("Failed to start sudo askpass helper; sudo will run without it", "error", err)
		return nil
	}
	h.askpass = srv
	return h.askpass
}

// applyAskpass prepares a command for execution under the sudo askpass flow.
// It returns the command to run (with the `sudo -A` wrapper prepended when
// applicable) and the environment to use. When the flow is inactive, or the
// command does not invoke sudo, it is a no-op: the original command and the
// shared base env are returned unchanged, and the askpass server is not even
// started. This keeps normal shell behaviour and the env surface untouched for
// the common (non-sudo) case.
func (h *shellHandler) applyAskpass(command string) (string, []string) {
	if !h.askpassActive() || !commandInvokesSudo(command) {
		return command, h.env
	}
	srv := h.ensureAskpass()
	if srv == nil {
		return command, h.env
	}
	env := append(append([]string(nil), h.env...), srv.env()...)
	return wrapSudoCommand(command, h.shell), env
}

// stopAskpass tears down the askpass server (socket + wrapper script) and
// resets the lazy-start state so a later command can start a fresh server if
// the handler is reused after a Stop.
func (h *shellHandler) stopAskpass() {
	h.askpassMu.Lock()
	srv := h.askpass
	h.askpass = nil
	h.askpassStarted = false
	h.askpassMu.Unlock()
	srv.close()
}
