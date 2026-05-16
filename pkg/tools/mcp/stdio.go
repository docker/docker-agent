package mcp

import (
	"context"
	"errors"
	"os/exec"
	"runtime"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/desktop"
)

type stdioMCPClient struct {
	sessionClient

	command string
	args    []string
	env     []string
	cwd     string
}

func newStdioCmdClient(command string, args, env []string, cwd string) *stdioMCPClient {
	return &stdioMCPClient{
		command: command,
		args:    args,
		env:     env,
		cwd:     cwd,
	}
}

func (c *stdioMCPClient) Initialize(ctx context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	// First, let's see if DD is running. This will help produce a better error message
	// Skip this check on Linux where Docker runs natively without Docker Desktop
	if c.command == "docker" && runtime.GOOS != "linux" && !desktop.IsDockerDesktopRunning(ctx) {
		return nil, errors.New("Docker Desktop is not running") //nolint:staticcheck // Don't lowercase Docker Desktop
	}

	toolChanged, promptChanged := c.notificationHandlers()

	// Create client options with elicitation and notification support
	opts := &gomcp.ClientOptions{
		ElicitationHandler:       c.handleElicitationRequest,
		ToolListChangedHandler:   toolChanged,
		PromptListChangedHandler: promptChanged,
	}

	client := gomcp.NewClient(&gomcp.Implementation{
		Name:    "docker agent",
		Version: "1.0.0",
	}, opts)

	cmd := exec.CommandContext(ctx, c.command, c.args...)
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
