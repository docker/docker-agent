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

	command string
	argsMu  sync.RWMutex
	args    []string
	env     []string
	cwd     string
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

	// Create client options with elicitation, sampling, and notification support.
	// Sampling registration is delegated to applySamplingHandlerOpts so the
	// with-tools callback is wired eagerly even when handler fields are still
	// nil at Initialize time — see that method for the ordering rationale.
	opts := &gomcp.ClientOptions{
		ElicitationHandler:       c.handleElicitationRequest,
		ToolListChangedHandler:   toolChanged,
		PromptListChangedHandler: promptChanged,
	}
	c.applySamplingHandlerOpts(opts)

	client := gomcp.NewClient(&gomcp.Implementation{
		Name:    "docker agent",
		Version: "1.0.0",
	}, opts)

	cmd := exec.CommandContext(ctx, c.command, c.getArgs()...)
	cmd.Env = c.env
	cmd.Dir = c.cwd
	session, err := client.Connect(ctx, &gomcp.CommandTransport{
		Command: cmd,
	}, nil)
	if err != nil {
		return nil, err
	}

	c.setSession(session)

	return session.InitializeResult(), nil
}
