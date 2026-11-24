package root

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/mcp"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

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
}

func newMCPCmd() *cobra.Command {
	var flags mcpFlags

	cmd := &cobra.Command{
		Use:   "mcp <agent-file>|<registry-ref>",
		Short: "Start an agent as an MCP (Model Context Protocol) server",
		Long:  "Start an MCP server that exposes the agent via the Model Context Protocol. By default, uses stdio transport. Use --http to start a streaming HTTP server instead. Use --attach to expose a running TUI's session instead of an agent file.",
		Example: `  docker-agent serve mcp ./agent.yaml
  docker-agent serve mcp ./team.yaml
  docker-agent serve mcp myorg/agent:tag
  docker-agent serve mcp ./agent.yaml --http --listen 127.0.0.1:9090
  docker-agent serve mcp --attach`,
		Args: cobra.MaximumNArgs(1),
		RunE: flags.runMCPCommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (all agents if not specified)")
	cmd.PersistentFlags().BoolVar(&flags.http, "http", false, "Use streaming HTTP transport instead of stdio")
	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8081", "Address to listen on")
	cmd.PersistentFlags().StringVar(&flags.attach, "attach", "", "Attach to a running TUI run by pid, address, or session id, including a tab spawned after the run started (or empty for the most recent)")
	cmd.PersistentFlags().Lookup("attach").NoOptDefVal = "latest"
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
// any record. The lookup therefore probes every live run's attached sessions for
// the ID, which is what makes "serve mcp --attach <spawned-tab-id>" work. An
// explicitly selected session that is attached nowhere fails clearly instead of
// being silently redirected to another tab.
func resolveAttachTarget(ctx context.Context, runs attachRuns, target string) (addr, sessionID string, err error) {
	target = strings.TrimSpace(target)
	if target == "latest" {
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

	// An explicit session ID is either the discovered run's recorded initial
	// session (the original lookup path) or a tab spawned after startup, which
	// no registry record knows about. Try the recorded run first so its "tab was
	// closed" error is preserved when that is what the caller named, then fall
	// back to scanning every live run for the tab.
	var recordedErr error
	if rec, findErr := runs.Find(target); findErr == nil {
		sessionID, err := mcp.ResolveAttachSession(ctx, rec.Addr, rec.SessionID, true)
		if err == nil {
			return rec.Addr, sessionID, nil
		}
		recordedErr = err
		if rec.SessionID == target {
			// The caller named the run's initial session exactly and its tab is
			// closed: fail instead of prefix-matching a different live tab, so an
			// explicit closed selection is never silently redirected.
			return "", "", err
		}
	}

	resolved, err := findLiveAttachSession(ctx, runs, target)
	if err != nil {
		if recordedErr != nil {
			return "", "", recordedErr
		}
		return "", "", err
	}
	return resolved.addr, resolved.sessionID, nil
}

// findLiveAttachSession resolves an explicitly selected tab against the attached
// sessions of every discovered run. Exact IDs win over prefixes and the match
// must be unique, so a client is never silently attached to a different tab.
func findLiveAttachSession(ctx context.Context, runs attachRuns, target string) (attachSession, error) {
	records, err := runs.List()
	if err != nil {
		return attachSession{}, fmt.Errorf("listing live runs: %w", err)
	}

	var exact, prefix []attachSession
	var live, unreachable []string
	for _, rec := range records {
		ids, err := mcp.LiveAttachSessions(ctx, rec.Addr)
		if err != nil {
			// One unreachable run must not hide a tab that lives on another;
			// report it only when nothing matches anywhere.
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", rec.Addr, err))
			continue
		}
		live = append(live, ids...)
		for _, id := range ids {
			candidate := attachSession{addr: rec.Addr, sessionID: id}
			switch {
			case id == target:
				exact = append(exact, candidate)
			case strings.HasPrefix(id, target):
				prefix = append(prefix, candidate)
			}
		}
	}

	matches := exact
	if len(matches) == 0 {
		matches = prefix
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		slices.Sort(live)
		msg := fmt.Sprintf("session %s is not attached to any live docker-agent run; live sessions: %s", target, formatLiveSessions(live))
		if len(unreachable) > 0 {
			slices.Sort(unreachable)
			msg += "; could not query runs: " + strings.Join(unreachable, ", ")
		}
		return attachSession{}, errors.New(msg)
	default:
		slices.SortFunc(matches, func(a, b attachSession) int {
			if c := strings.Compare(a.sessionID, b.sessionID); c != 0 {
				return c
			}
			return strings.Compare(a.addr, b.addr)
		})
		options := make([]string, 0, len(matches))
		for _, m := range matches {
			options = append(options, fmt.Sprintf("%s (run at %s)", m.sessionID, m.addr))
		}
		return attachSession{}, fmt.Errorf("session %q is ambiguous: it matches %s", target, strings.Join(options, ", "))
	}
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
