package root

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/mcp"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

// attachLatest is the value pflag assigns to --attach when it is given without
// a value: it selects the most recent run.
const attachLatest = "latest"

// attachDiscoveryTimeout bounds how long --attach spends discovering the live
// sessions of every registered run. A TUI suspended with Ctrl+Z (SIGSTOP) keeps
// its registry record and its listener, but the stopped process cannot answer
// HTTP; probing it serially would wait for the whole request timeout before the
// attach proxy could start for a healthy run. Probing the runs concurrently under
// one short deadline keeps such an unrelated stalled run from delaying the attach.
const attachDiscoveryTimeout = 2 * time.Second

type mcpFlags struct {
	agentName      string
	http           bool
	listenAddr     string
	attach         string
	safety         string
	authToken      string
	insecureNoAuth bool
	runConfig      config.RuntimeConfig

	// runRegistry resolves --attach targets. Nil means the process-wide
	// runregistry.Default(); tests inject an isolated registry so the CLI
	// lookup never reads the user's real run registry.
	runRegistry attachRuns

	// startAttach runs the attach proxy once the target is resolved. Nil means
	// mcp.StartAttachStdio; tests inject a recorder so the advertised command
	// can be executed without starting a stdio server.
	startAttach func(ctx context.Context, addr, sessionID string) error
}

// mcpCmdOption customises the mcp command's flags before they are bound.
type mcpCmdOption func(*mcpFlags)

func newMCPCmd(opts ...mcpCmdOption) *cobra.Command {
	var flags mcpFlags
	for _, opt := range opts {
		opt(&flags)
	}

	cmd := &cobra.Command{
		Use:   "mcp <agent-file>|<registry-ref>",
		Short: "Start an agent as an MCP (Model Context Protocol) server",
		Long:  "Start an MCP server that exposes the agent via the Model Context Protocol. By default, uses stdio transport. Use --http to start a streaming HTTP server instead. Use --attach to expose a running TUI's session instead of an agent file.",
		Example: `  docker-agent serve mcp ./agent.yaml
  docker-agent serve mcp ./team.yaml
  docker-agent serve mcp myorg/agent:tag
  docker-agent serve mcp ./agent.yaml --http --listen 127.0.0.1:9090
  docker-agent serve mcp --attach                              # the most recent TUI run
  docker-agent serve mcp --attach=<session-id>                 # a specific TUI tab, including a spawned one`,
		Args: cobra.MaximumNArgs(1),
		RunE: flags.runMCPCommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (all agents if not specified)")
	cmd.PersistentFlags().BoolVar(&flags.http, "http", false, "Use streaming HTTP transport instead of stdio")
	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8081", "Address to listen on")
	cmd.PersistentFlags().StringVar(&flags.attach, "attach", "", "Attach to a running TUI run by pid, address, or session id, including a tab spawned after the run started; pass a target as --attach=<target>, or --attach alone for the most recent run")
	cmd.PersistentFlags().Lookup("attach").NoOptDefVal = attachLatest
	cmd.PersistentFlags().StringVar(&flags.safety, "safety", "", "Tool safety policy (strict, balanced, restricted, autonomous); only valid with --http")
	cmd.PersistentFlags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for HTTP MCP requests; only valid with --http")
	cmd.PersistentFlags().BoolVar(&flags.insecureNoAuth, "insecure-no-auth", false, "Allow unauthenticated non-loopback HTTP binding (insecure); only valid with --http")
	cmd.PersistentFlags().StringVar(&flags.runConfig.MCPToolName, "tool-name", "", "Override the MCP tool identifier clients call (defaults to agent name); only valid when exposing a single agent")
	cmd.PersistentFlags().DurationVar(&flags.runConfig.MCPKeepAlive, "mcp-keepalive", 0, "Interval between MCP keep-alive pings (e.g. 30s); 0 disables keep-alive; only valid when serving an agent over stdio (not --http or --attach)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *mcpFlags) runMCPCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"mcp"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"mcp"}, args...), commandErr)
	}()

	if f.attach != "" {
		if f.http || f.safety != "" || f.authToken != "" || f.insecureNoAuth {
			return errors.New("--http-only safety and authentication flags cannot be used with --attach")
		}
		if f.runConfig.MCPKeepAlive != 0 {
			return errors.New("--mcp-keepalive cannot be used with --attach: the attach proxy ignores runtime configuration")
		}
		// pflag gives --attach a NoOptDefVal, so it never consumes the next
		// argument: "--attach <session-id>" parses as --attach=latest plus one
		// positional argument. Honour that spelling by using the positional as
		// the target instead of silently attaching to the most recent run.
		if f.attach == attachLatest && len(args) == 1 {
			f.attach = args[0]
		}
		return f.runAttach(ctx)
	}

	if !f.http && (f.safety != "" || f.authToken != "" || f.insecureNoAuth) {
		return errors.New("--safety, --auth-token, and --insecure-no-auth require --http")
	}
	if f.http && f.runConfig.MCPKeepAlive != 0 {
		return errors.New("--mcp-keepalive is not supported with --http: stateless HTTP MCP does not support server-initiated keep-alive pings; use the stdio transport instead")
	}
	if err := validateSafetyFlag(f.safety); err != nil {
		return err
	}

	if len(args) == 0 {
		return errors.New("agent file is required (or use --attach)")
	}
	agentFilename := args[0]

	if !f.http {
		return mcp.StartMCPServer(ctx, agentFilename, f.agentName, &f.runConfig)
	}

	if !isLoopbackListenAddr(f.listenAddr) && f.authToken == "" && !f.insecureNoAuth {
		return errors.New("non-loopback MCP HTTP listeners require --auth-token or --insecure-no-auth")
	}

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	return mcp.StartHTTPServer(ctx, agentFilename, f.agentName, &f.runConfig, ln, mcp.HTTPOptions{
		CLISafety: session.SafetyPolicy(f.safety),
		AuthToken: f.authToken,
		Out:       cmd.OutOrStdout(),
		OnSafetyPolicy: func(resolved servesafety.Resolved) {
			fmt.Fprintf(cmd.OutOrStdout(), "Tool safety policy: %s (source: %s)\n", resolved.Policy, resolved.Source)
		},
	})
}

func (f *mcpFlags) runAttach(ctx context.Context) error {
	addr, sessionID, err := resolveAttachTarget(ctx, f.attachRuns(), f.attach)
	if err != nil {
		return err
	}
	if f.startAttach != nil {
		return f.startAttach(ctx, addr, sessionID)
	}
	return mcp.StartAttachStdio(ctx, addr, sessionID)
}

// attachRuns is the subset of the run registry the --attach lookup needs.
// *runregistry.Registry is the production implementation; tests inject an
// in-memory fake so the CLI lookup never reads the user's real registry.
type attachRuns interface {
	Find(target string) (runregistry.Record, error)
	List() ([]runregistry.Record, error)
}

var _ attachRuns = (*runregistry.Registry)(nil)

// attachRuns returns the registry used to resolve --attach targets.
func (f *mcpFlags) attachRuns() attachRuns {
	if f.runRegistry != nil {
		return f.runRegistry
	}
	return runregistry.Default()
}

// attachSession is a live tab resolved on a discovered run.
type attachSession struct {
	addr      string
	sessionID string
}

// resolveAttachTarget maps a --attach value to the control-plane address of the
// live run that serves it and the session ID the MCP proxy should drive.
//
// A pid, an http(s) address, or an empty/"latest" target discovers a run through
// the registry; the session is then resolved inside that run: its recorded
// initial session while that is still attached, the only live session when the
// initial tab was closed, or a clear error listing the candidates.
//
// A session ID selects that live session. A run's registry record only stores
// the session the run started with, so a TUI tab spawned after startup is not on
// any record. The lookup therefore resolves the ID against the attached sessions
// of every live run, which is what makes "serve mcp --attach=<spawned-tab-id>"
// work. An explicitly selected session that is attached nowhere fails clearly
// instead of being silently redirected to another tab.
func resolveAttachTarget(ctx context.Context, runs attachRuns, target string) (addr, sessionID string, err error) {
	target = strings.TrimSpace(target)
	if target == attachLatest {
		target = ""
	}

	if !attachTargetIsSessionID(target) {
		rec, err := runs.Find(target)
		if err != nil {
			return "", "", err
		}
		sessionID, err := mcp.ResolveAttachSession(ctx, rec.Addr, rec.SessionID, false)
		if err != nil {
			return "", "", err
		}
		return rec.Addr, sessionID, nil
	}

	return resolveExplicitAttachSession(ctx, runs, target)
}

// resolveExplicitAttachSession resolves an explicitly named session ID against
// the live tabs of every discovered run.
//
// A run's registry record only names the session the run started with, and that
// ID may contain (or be contained by) another tab's ID, so resolving the record
// first can silently bind the proxy to the wrong tab. Resolve from the live view
// in order instead:
//
//  1. a live exact match always wins, so an initial ID that contains a spawned
//     tab's full ID cannot shadow that tab;
//  2. an exactly named recorded initial session whose tab is closed keeps the
//     resolver's clear "tab was closed" error, even when a live tab's ID
//     extends it;
//  3. a partial ID (the same substring match the run registry uses) selects a
//     live tab only when it matches a single live session; several matches are
//     reported as ambiguous instead of picking one of them.
func resolveExplicitAttachSession(ctx context.Context, runs attachRuns, target string) (addr, sessionID string, err error) {
	records, err := runs.List()
	if err != nil {
		return "", "", fmt.Errorf("listing live runs: %w", err)
	}

	// Probe every discovered run concurrently: an unrelated suspended TUI can
	// otherwise consume the full HTTP timeout before a healthy run is queried.
	probes := probeAttachSessions(ctx, records)

	var sessions []attachSession
	var unreachable []string
	for i, rec := range records {
		if probes[i].err != nil {
			// One unreachable run must not hide a tab that lives on another;
			// report it only when nothing matches anywhere.
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", rec.Addr, probes[i].err))
			continue
		}
		for _, id := range probes[i].ids {
			sessions = append(sessions, attachSession{addr: rec.Addr, sessionID: id})
		}
	}

	var exact []attachSession
	for _, sess := range sessions {
		if sess.sessionID == target {
			exact = append(exact, sess)
		}
	}
	switch {
	case len(exact) > 1:
		return "", "", ambiguousAttachSessionError(target, exact)
	case len(exact) == 1:
		return exact[0].addr, exact[0].sessionID, nil
	}

	for _, rec := range records {
		if rec.SessionID != target {
			continue
		}
		resolved, err := resolveRecordedAttachSession(ctx, rec.Addr, rec.SessionID)
		if err != nil {
			return "", "", err
		}
		return rec.Addr, resolved, nil
	}

	var partial []attachSession
	for _, sess := range sessions {
		if strings.Contains(sess.sessionID, target) {
			partial = append(partial, sess)
		}
	}
	switch len(partial) {
	case 1:
		return partial[0].addr, partial[0].sessionID, nil
	case 0:
		return "", "", noLiveAttachSessionError(target, sessions, unreachable)
	default:
		return "", "", ambiguousAttachSessionError(target, partial)
	}
}

// attachProbe is the live-session probe of one discovered run.
type attachProbe struct {
	ids []string
	err error
}

// probeAttachSessions queries every discovered run concurrently, under one short
// overall deadline, and returns the probes in the same order as records so callers
// keep a deterministic view. A run whose TUI is suspended still has a listener
// that accepts the connection but never answers, so the per-request timeout is
// what makes a serial scan slow; probing concurrently and canceling the whole
// discovery at attachDiscoveryTimeout bounds that wait for every run at once.
func probeAttachSessions(ctx context.Context, records []runregistry.Record) []attachProbe {
	probes := make([]attachProbe, len(records))

	ctx, cancel := context.WithTimeout(ctx, attachDiscoveryTimeout)
	defer cancel()

	var group sync.WaitGroup
	for i, rec := range records {
		group.Go(func() {
			ids, err := mcp.LiveAttachSessions(ctx, rec.Addr)
			probes[i] = attachProbe{ids: ids, err: err}
		})
	}
	group.Wait()

	return probes
}

// resolveRecordedAttachSession resolves the recorded initial session of the run
// whose record names the requested target. It is bounded like discovery so a
// suspended target run cannot reintroduce the long request timeout after the
// concurrent probe returned.
func resolveRecordedAttachSession(ctx context.Context, addr, recorded string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, attachDiscoveryTimeout)
	defer cancel()
	return mcp.ResolveAttachSession(ctx, addr, recorded, true)
}

// ambiguousAttachSessionError reports every live tab a target matches instead of
// silently selecting one of them.
func ambiguousAttachSessionError(target string, matches []attachSession) error {
	slices.SortFunc(matches, func(a, b attachSession) int {
		if c := strings.Compare(a.sessionID, b.sessionID); c != 0 {
			return c
		}
		return strings.Compare(a.addr, b.addr)
	})
	options := make([]string, 0, len(matches))
	for _, match := range matches {
		options = append(options, fmt.Sprintf("%s (run at %s)", match.sessionID, match.addr))
	}
	return fmt.Errorf("session %q is ambiguous: it matches %s; select one explicitly with --attach=<session-id>", target, strings.Join(options, ", "))
}

// noLiveAttachSessionError reports that an explicitly named session is not
// attached anywhere and lists the live tabs that could be selected instead.
func noLiveAttachSessionError(target string, sessions []attachSession, unreachable []string) error {
	live := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		live = append(live, sess.sessionID)
	}
	slices.Sort(live)
	msg := fmt.Sprintf("session %s is not attached to any live docker-agent run; live sessions: %s", target, formatLiveSessions(live))
	if len(unreachable) > 0 {
		slices.Sort(unreachable)
		msg += "; could not query runs: " + strings.Join(unreachable, ", ")
	}
	return errors.New(msg)
}

func formatLiveSessions(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// attachTargetIsSessionID reports whether a --attach value names a session ID
// explicitly rather than discovering the run by pid, address, or "latest".
func attachTargetIsSessionID(target string) bool {
	if target == "" || target == "latest" {
		return false
	}
	if _, err := strconv.Atoi(target); err == nil {
		return false
	}
	return !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://")
}
