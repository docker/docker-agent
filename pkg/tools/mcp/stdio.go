package mcp

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/desktop"
)

type stdioMCPClient struct {
	sessionClient

	command       string
	argsMu        sync.RWMutex
	args          []string
	env           []string
	cwd           string
	sessionOwned  bool
	processMu     sync.Mutex
	processCancel context.CancelFunc
}

func newStdioCmdClient(command string, args, env []string, cwd string) *stdioMCPClient {
	return &stdioMCPClient{
		// stdio has no real "server address" in the OTel HTTP sense; using
		// the command as a stand-in keeps spans triageable when the agent
		// has multiple stdio MCPs wired up. Span readers see the
		// executable name (e.g. `foo-mcp-server`) on `server.address`.
		sessionClient: sessionClient{serverAddress: command},
		command:       command,
		args:          args,
		env:           env,
		cwd:           cwd,
	}
}

func (c *stdioMCPClient) setArgs(args []string) {
	c.argsMu.Lock()
	defer c.argsMu.Unlock()
	c.args = slices.Clone(args)
}

func (c *stdioMCPClient) getArgs() []string {
	c.argsMu.RLock()
	defer c.argsMu.RUnlock()
	return slices.Clone(c.args)
}

func (c *stdioMCPClient) Initialize(ctx context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	// First, let's see if DD is running. This will help produce a better error message
	// Skip this check on Linux where Docker runs natively without Docker Desktop
	if c.command == "docker" && runtime.GOOS != "linux" && !desktop.IsDockerDesktopRunning(ctx) {
		return nil, errors.New("Docker Desktop is not running") //nolint:staticcheck // Don't lowercase Docker Desktop
	}

	toolChanged, promptChanged := c.notificationHandlers()

	// Create client options with elicitation and notification support.
	// Override the SDK default that advertises deprecated roots support.
	opts := &gomcp.ClientOptions{
		Capabilities:             &gomcp.ClientCapabilities{},
		ElicitationHandler:       c.handleElicitationRequest,
		ToolListChangedHandler:   toolChanged,
		PromptListChangedHandler: promptChanged,
	}

	client := gomcp.NewClient(&gomcp.Implementation{
		Name:    "docker agent",
		Version: "1.0.0",
	}, opts)

	connectCtx, processCtx := ctx, ctx
	var cancel context.CancelFunc
	var stopCancel func() bool
	if c.sessionOwned {
		connectCtx = cancellableParentFromContext(ctx)
		if connectCtx == nil {
			connectCtx = ctx
		}
		if err := connectCtx.Err(); err != nil {
			return nil, err
		}
		processCtx, cancel = context.WithCancel(context.WithoutCancel(ctx))
		c.processMu.Lock()
		c.processCancel = cancel
		c.processMu.Unlock()
		stopCancel = context.AfterFunc(connectCtx, cancel)
		defer stopCancel()
	}
	cmd := exec.CommandContext(processCtx, c.command, c.getArgs()...)
	cmd.Env = c.env
	cmd.Dir = c.cwd
	transport := &trackedCommandTransport{CommandTransport: gomcp.CommandTransport{Command: cmd}}
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		if cancel != nil {
			cancel()
			if transport.connection != nil {
				_ = transport.connection.Close()
			}
		}
		return nil, err
	}
	if stopCancel != nil && (!stopCancel() || connectCtx.Err() != nil) {
		cancel()
		_ = session.Close()
		return nil, connectCtx.Err()
	}

	c.setSession(session)

	return session.InitializeResult(), nil
}

// trackedCommandTransport also closes SDK discovery failures that return no session.
type trackedCommandTransport struct {
	gomcp.CommandTransport

	connection gomcp.Connection
}

func (t *trackedCommandTransport) Connect(ctx context.Context) (gomcp.Connection, error) {
	conn, err := t.CommandTransport.Connect(ctx)
	t.connection = conn
	return conn, err
}

func (c *stdioMCPClient) cancelProcess() {
	c.processMu.Lock()
	cancel := c.processCancel
	c.processMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
