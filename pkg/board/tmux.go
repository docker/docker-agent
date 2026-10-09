package board

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/paths"
)

// tmuxSessions manages the tmux sessions the board runs its agents in.
type tmuxSessions struct {
	// ctx is the board-lifetime context tmux commands run under.
	ctx context.Context //nolint:containedctx // sessionManager methods are context-free
}

// sessionManager abstracts tmux so tests can inject a fake.
type sessionManager interface {
	// NewSession creates a tmux session running the docker agent for the
	// given docker-agent session ID, from workDir. The agent exposes its
	// control plane on listenSocket. A non-empty worktreeName marks the
	// first run: docker agent creates an isolated worktree of that name,
	// branched from worktreeBase, and workDir is the repository. On resume,
	// worktreeName is empty and workDir is the worktree directory. A
	// non-empty prompt is sent as the first message.
	NewSession(name, workDir, agent, sessionID, listenSocket, worktreeName, worktreeBase, prompt string) error
	KillSession(name string) error
	// Alive reports whether the session exists and its agent pane is still
	// running. It lets the controller tell a control plane that is merely
	// slow to start from a session whose agent has died and must be
	// relaunched.
	Alive(name string) (bool, error)
	// Exists reports whether the session exists at all, dead pane included.
	// It tells a session holding a dead agent's output (attachable) from one
	// that is gone entirely.
	Exists(name string) (bool, error)
}

// socketDir creates and validates, once per process, the per-user
// directory holding the board's unix sockets: its private tmux socket and
// each card's agent control-plane socket. It lives under the system temp
// dir — never under the data dir: ~/.cagent may be bind-mounted into a
// docker sandbox, where unix sockets cannot be bound. The checks fail
// closed: a pre-existing path owned by another user, or not a real
// directory, must never be used for the sockets.
var socketDir = sync.OnceValues(func() (string, error) {
	dir := filepath.Join(os.TempDir(), "cagent-board-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create board socket dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("board socket dir %s is not a directory", dir)
	}
	if err := checkOwner(dir, info); err != nil {
		return "", err
	}
	// Tighten a dir that pre-existed with looser permissions; ownership was
	// verified above, so chmod cannot be tricked into loosening someone
	// else's directory.
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is the tightest usable mode for a directory
			return "", fmt.Errorf("tighten board socket dir permissions: %w", err)
		}
	}
	return dir, nil
})

// TmuxSocketPath is the dedicated tmux socket the board runs its sessions
// on. The board shares the host's tmux binary but not its default server: a
// private socket keeps the board's server-wide options from leaking into the
// user's interactive tmux. The path is stable across board restarts so the
// controller can reattach to sessions left running on it.
//
// Like tmux's own /tmp/tmux-<uid> convention, the socket lives in a
// per-user 0700 directory so other local users cannot pre-create or reach
// it. The directory is created and validated before tmux binds the socket.
func TmuxSocketPath() (string, error) {
	dir, err := socketDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tmux.sock"), nil
}

// serverDefaults are tmux options the board applies to its private server so
// every session feels like a native terminal when attached: no tmux chrome,
// keys passed straight through, full terminal fidelity, and client-driven
// sizing.
var serverDefaults = [][]string{
	// Visual chrome: hide every bit of tmux UI.
	{"set", "-g", "status", "off"},
	{"set", "-g", "bell-action", "none"},
	{"set", "-g", "monitor-activity", "off"},
	{"set", "-g", "monitor-bell", "off"},

	// Input behavior: every keystroke reaches the agent, ESC is instant.
	// With no prefix bound, C-b reaches the agent too. extended-keys
	// forwards CSI-u sequences so modified keys reach the agent.
	{"set", "-g", "prefix", "none"},
	{"set", "-g", "prefix2", "none"},
	{"set", "-g", "escape-time", "0"},
	{"set", "-g", "mouse", "on"},
	{"set", "-g", "extended-keys", "always"},

	// Terminal fidelity: truecolor, clipboard, focus events.
	{"set", "-g", "allow-passthrough", "on"},
	{"set", "-g", "focus-events", "on"},
	{"set", "-g", "set-clipboard", "on"},
	{"set", "-g", "default-terminal", "tmux-256color"},
	{"set", "-g", "terminal-features", ",xterm-256color:clipboard:ccolour:cstyle:extkeys:focus:title:mouse:RGB"},

	// Sizing: follow the attached client, not the smallest one.
	{"set", "-g", "aggressive-resize", "on"},
	{"set", "-g", "window-size", "latest"},

	// The board may itself run as a docker CLI plugin (`docker agent
	// board`). Its panes must not inherit the plugin handshake variables:
	// the direct `docker-agent run` launch would detect them, take the
	// plugin code path, and die printing docker usage (exit 125).
	{"set-environment", "-g", "-r", "DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND"},
	{"set-environment", "-g", "-r", "DOCKER_CLI_PLUGIN_SOCKET"},

	// With no prefix bound, this is the one key the board reserves: it
	// detaches the client and returns the user to the board.
	{"bind-key", "-n", "C-q", "detach-client"},
	// Header buttons (see setSessionHeader). The "ctrl+q board" hint is
	// wrapped in range=right: clicking it detaches. diff/editor are user
	// ranges dispatched by name; their commands live in per-session
	// @board-diff/@board-editor options, expanded when the popup or shell
	// runs.
	{"bind-key", "-n", "MouseDown1StatusRight", "detach-client"},
	{
		"bind-key", "-n", "MouseDown1Status",
		"if", "-F", "#{==:#{mouse_status_range},diff}",
		// display-popup does not format-expand its shell command, so the
		// per-session diff command is resolved inside the popup via $TMUX.
		`display-popup -E -w 90% -h 85% -T " diff — q closes " 'eval "$(tmux show-option -v @board-diff)"'`,
		`if -F "#{==:#{mouse_status_range},editor}" 'run-shell "#{@board-editor}"'`,
	},
}

// tmuxRun runs `tmux -S <socket> <args...>` against the board's private
// server and returns combined output as part of any error.
func tmuxRun(ctx context.Context, args ...string) (string, error) {
	socket, err := TmuxSocketPath()
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// applyServerDefaults applies serverDefaults to the board's private server.
func applyServerDefaults(ctx context.Context) {
	for _, args := range serverDefaults {
		_, _ = tmuxRun(ctx, args...)
	}
}

// agentCommand builds the docker agent invocation for a session. The board
// owns sessionID and passes it via --session: the first run creates that
// session, later runs resume it.
//
// --config-dir, --data-dir, and --cache-dir forward the board's own
// directories so the agent resolves the same aliases, creates its worktree
// and session under the data dir the board watches, and shares the board's
// caches. Directory overrides are process-local flags, not inherited env,
// so without forwarding an agent spawned in an environment whose $HOME
// differs from the board's directories (e.g. a docker sandbox with the
// host dirs bind-mounted) would silently use its own empty config and a
// data dir the board never looks at. On a host with no overrides this
// forwards the defaults — a no-op.
//
// --listen exposes the run's control plane on listenSocket (a unix socket
// the board owns), so the board can observe and drive the session over HTTP
// instead of scraping the terminal.
//
// On the first run, worktreeName is non-empty: --worktree creates an
// isolated git worktree (branched from worktreeBase) and every tool runs
// inside it. On resume, worktreeName is empty and --worktree is omitted:
// docker agent reattaches the session to its original worktree
// automatically, so passing --worktree again (which would fail, the worktree
// already exists) is avoided.
func agentCommand(agent, sessionID, listenSocket, worktreeName, worktreeBase, prompt string) string {
	// Launch through the current binary rather than assuming a `docker
	// agent` plugin is installed; the binary supports direct invocation.
	bin, err := os.Executable()
	if err != nil {
		bin = "docker-agent"
	}
	cmd := fmt.Sprintf("%s run %s --yolo --config-dir %s --data-dir %s --cache-dir %s --session %s --listen %s",
		shQuote(bin), shQuote(agent),
		shQuote(paths.GetConfigDir()), shQuote(paths.GetDataDir()), shQuote(paths.GetCacheDir()),
		shQuote(sessionID), shQuote("unix://"+listenSocket))
	if worktreeName != "" {
		cmd += fmt.Sprintf(" --worktree=%s --worktree-base %s", shQuote(worktreeName), shQuote(worktreeBase))
	}
	if prompt != "" {
		cmd += " " + shQuote(prompt)
	}
	return cmd
}

// shQuote single-quotes s for POSIX shells.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewSession creates a tmux session and runs docker agent in it. The pane
// briefly starts the user's shell (new-session), remain-on-exit is set, and
// the pane is then respawned with the agent as its process. respawn-pane
// avoids typing the command into the user's interactive shell (send-keys):
// no shell-history pollution, no dependence on the shell supporting `exec`,
// and no race with slow shell startup swallowing the input. When the agent
// exits the pane goes dead instead of dropping back to a shell (see
// remain-on-exit), so the user can read its final output and the controller
// can detect the dead pane and relaunch.
func (t tmuxSessions) NewSession(name, workDir, agent, sessionID, listenSocket, worktreeName, worktreeBase, prompt string) error {
	if _, err := tmuxRun(t.ctx, "new-session", "-d", "-s", name, "-c", workDir); err != nil {
		return err
	}

	applyServerDefaults(t.ctx)

	// Keep the pane (and thus the session) alive after the agent exits. Set
	// before the agent replaces the pane so there is no race where the
	// agent could exit before the option takes effect.
	_, _ = tmuxRun(t.ctx, "set-option", "-t", name, "remain-on-exit", "on")

	// exec makes the agent the pane's process (replacing the /bin/sh tmux
	// runs the command with): when it exits the pane goes dead.
	cmd := "exec " + agentCommand(agent, sessionID, listenSocket, worktreeName, worktreeBase, prompt)
	if _, err := tmuxRun(t.ctx, "respawn-pane", "-k", "-t", name, "-c", workDir, cmd); err != nil {
		return err
	}
	return nil
}

// setSessionHeader configures a two-row header at the top of a card's tmux
// session, shown while attached — the equivalent of the original board's
// terminal dialog header: card title and project on the left; clickable
// diff, editor, and back-to-board buttons on the right — followed by a
// blank spacer row. Called before every attach so the header shows the
// card's current title.
func setSessionHeader(ctx context.Context, name, title, project, worktree string) {
	left := " 🐳 #[bold]" + tmuxFormatEscape(title) + "#[nobold]"
	if project != "" {
		left += " · " + tmuxFormatEscape(project)
	}
	left += " "

	// The buttons' commands, stored as session options so the shared mouse
	// binding (serverDefaults) runs the right card's command. The diff
	// mirrors the board's diff view (against the merge-base with the
	// upstream base); the editor mirrors the board's o binding.
	wt := shQuote(worktree)
	diffCmd := "git -C " + wt + " diff --color=always \"$(git -C " + wt + " merge-base HEAD " +
		shQuote(upstreamBase(ctx, worktree)) + " || echo HEAD)\" | less -R"
	editorCmd := "${DOCKER_AGENT_BOARD_EDITOR:-${BOARD_EDITOR:-code}} " + wt

	right := "#[range=user|diff] diff #[norange]·#[range=user|editor] editor #[norange]·" +
		"#[range=right] #[bold]ctrl+q#[nobold] board #[norange]"

	for _, args := range [][]string{
		{"set-option", "-t", name, "@board-diff", diffCmd},
		{"set-option", "-t", name, "@board-editor", editorCmd},
		// Two status rows: the header itself and a blank spacer under it,
		// so the agent TUI does not sit glued to the header.
		{"set-option", "-t", name, "status", "2"},
		// Both rows use the terminal's default background. The range markers
		// make the buttons clickable (MouseDown1Status* bindings in
		// serverDefaults).
		{"set-option", "-t", name, "status-style", "bg=default,fg=default"},
		{
			"set-option", "-t", name, "status-format[0]",
			"#[align=left range=left]#[push-default]#{T:status-left}#[pop-default]#[norange]" +
				"#[align=right]" + right,
		},
		{"set-option", "-t", name, "status-format[1]", ""},
		{"set-option", "-t", name, "status-position", "top"},
		{"set-option", "-t", name, "status-interval", "0"},
		{"set-option", "-t", name, "status-left-length", "200"},
		{"set-option", "-t", name, "status-left", left},
	} {
		_, _ = tmuxRun(ctx, args...)
	}
	// Re-assert the shared bindings so a server started by an older binary
	// honors them on the next attach.
	applyServerDefaults(ctx)
}

// tmuxFormatEscape makes untrusted text (agent-controlled titles) safe
// inside a tmux format string: '#' introduces format expansion, so it is
// doubled; control characters are dropped.
func tmuxFormatEscape(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "#", "##")
}

// KillSession kills a tmux session. A missing session is not an error.
func (t tmuxSessions) KillSession(name string) error {
	_, err := tmuxRun(t.ctx, "kill-session", "-t", "="+name)
	if err != nil && isNoSuchSession(err) {
		return nil
	}
	return err
}

// Alive reports whether the session exists and its agent pane is still
// running. A pane goes dead when the agent exits (remain-on-exit keeps the
// session around); a missing session reports not alive.
func (t tmuxSessions) Alive(name string) (bool, error) {
	out, err := tmuxRun(t.ctx, "list-panes", "-t", "="+name, "-F", "#{pane_dead}")
	if err != nil {
		if isNoSuchSession(err) {
			return false, nil
		}
		return false, err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return first == "0", nil
}

// Exists reports whether the session exists, dead pane included.
func (t tmuxSessions) Exists(name string) (bool, error) {
	if _, err := tmuxRun(t.ctx, "has-session", "-t", "="+name); err != nil {
		if isNoSuchSession(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isNoSuchSession matches tmux's errors for a missing session or a server
// that is not running at all.
func isNoSuchSession(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "No such file or directory") ||
		strings.Contains(msg, "error connecting to")
}
