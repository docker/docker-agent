package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	otelmcp "github.com/docker/docker-agent/pkg/telemetry/mcp"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/version"
)

type ToolInput struct {
	Message string `json:"message" jsonschema:"the message to send to the agent"`
}

type ToolOutput struct {
	Response string `json:"response" jsonschema:"the response from the agent"`
}

func StartMCPServer(ctx context.Context, agentFilename, agentName string, runConfig *config.RuntimeConfig) error {
	slog.DebugContext(ctx, "Starting MCP server", "agent", agentFilename)

	server, cleanup, err := createMCPServer(ctx, agentFilename, agentName, runConfig)
	if err != nil {
		return err
	}
	defer cleanup()

	slog.DebugContext(ctx, "MCP server starting with stdio transport")

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("MCP server error: %w", err)
	}

	return nil
}

// StartHTTPServer starts a streaming HTTP MCP server on the given listener
func StartHTTPServer(ctx context.Context, agentFilename, agentName string, runConfig *config.RuntimeConfig, ln net.Listener) error {
	slog.DebugContext(ctx, "Starting HTTP MCP server", "agent", agentFilename, "addr", ln.Addr())

	server, cleanup, err := createMCPServer(ctx, agentFilename, agentName, runConfig)
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Printf("MCP HTTP server listening on http://%s\n", ln.Addr())

	// Wrap with otelhttp so the MCP-over-HTTP transport extracts
	// `traceparent` / `baggage` from incoming requests just like the
	// stdio transport extracts them from `params._meta`. Without this
	// HTTP-mode MCP clients lose trace context at the boundary.
	httpServer := &http.Server{
		Handler: otelhttp.NewHandler(
			mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
				return server
			}, nil),
			"mcp.http",
		),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		return httpServer.Shutdown(context.Background())
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func createMCPServer(ctx context.Context, agentFilename, agentName string, runConfig *config.RuntimeConfig) (*mcp.Server, func(), error) {
	agentSource, err := config.Resolve(agentFilename, nil)
	if err != nil {
		return nil, nil, err
	}

	t, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load agents: %w", err)
	}

	cleanup := func() {
		if err := t.StopToolSets(ctx); err != nil {
			slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
		}
	}

	// The SDK only starts keep-alive when KeepAlive > 0.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "docker agent",
		Version: version.Version,
	}, &mcp.ServerOptions{
		KeepAlive: runConfig.MCPKeepAlive,
	})

	agentNames := t.AgentNames()
	if agentName != "" {
		if !slices.Contains(agentNames, agentName) {
			cleanup()
			return nil, nil, fmt.Errorf("agent %s not found in %s", agentName, agentFilename)
		}
		agentNames = []string{agentName}
	}

	if runConfig.MCPToolName != "" && len(agentNames) > 1 {
		cleanup()
		return nil, nil, errors.New("--tool-name can only be used when exactly one agent is exposed")
	}

	slog.DebugContext(ctx, "Adding MCP tools for agents", "count", len(agentNames))

	for _, agentName := range agentNames {
		ag, err := t.Agent(agentName)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("failed to get agent %s: %w", agentName, err)
		}

		description := cmp.Or(ag.Description(), fmt.Sprintf("Run the %s agent", agentName))

		slog.DebugContext(ctx, "Adding MCP tool", "agent", agentName, "description", description)

		annotations, err := agentToolAnnotations(ctx, ag)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("failed to compute annotations for agent %s: %w", agentName, err)
		}

		annotations.Title = description

		toolDef := &mcp.Tool{
			Name:         cmp.Or(runConfig.MCPToolName, agentName),
			Description:  description,
			Annotations:  annotations,
			InputSchema:  tools.MustSchemaFor[ToolInput](),
			OutputSchema: tools.MustSchemaFor[ToolOutput](),
		}

		mcp.AddTool(server, toolDef, CreateToolHandler(t, agentName))
	}

	return server, cleanup, nil
}

func CreateToolHandler(t *team.Team, agentName string) func(context.Context, *mcp.CallToolRequest, ToolInput) (*mcp.CallToolResult, ToolOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ToolInput) (result *mcp.CallToolResult, output ToolOutput, err error) {
		// Extract W3C trace context from `params._meta` (per the OTel
		// MCP semconv) so the SERVER span chains onto the calling
		// CLIENT span. Then start a `tools/call {agent}` SERVER span
		// covering the full handler execution.
		if req != nil && req.Params != nil {
			ctx = otelmcp.ExtractMeta(ctx, req.Params.Meta)
		}
		ctx, span := otelmcp.StartServer(ctx, otelmcp.CallOptions{
			Method:   otelmcp.MethodToolsCall,
			ToolName: agentName,
		})
		defer func() {
			if err != nil {
				span.RecordError(err, "")
			}
			span.End()
		}()

		slog.DebugContext(ctx, "MCP tool called", "agent", agentName, "message", input.Message)

		ag, err := t.Agent(agentName)
		if err != nil {
			return nil, ToolOutput{}, fmt.Errorf("failed to get agent: %w", err)
		}

		sess := session.New(
			session.WithTitle("MCP tool call"),
			session.WithMaxIterations(ag.MaxIterations()),
			session.WithMaxConsecutiveToolCalls(ag.MaxConsecutiveToolCalls()),
			session.WithMaxOldToolCallTokens(ag.MaxOldToolCallTokens()),
			session.WithUserMessage(input.Message),
			session.WithToolsApproved(true),
			session.WithNonInteractive(true),
		)

		rt, err := runtime.New(t,
			runtime.WithCurrentAgent(agentName),
			runtime.WithNonInteractive(true),
			// See pkg/a2a/adapter.go for rationale — without this
			// the runtime's startSpan is a no-op when cagent runs as
			// an MCP server, so all our runtime.* spans go silent.
			runtime.WithTracer(otel.Tracer("cagent")),
		)
		if err != nil {
			return nil, ToolOutput{}, fmt.Errorf("failed to create runtime: %w", err)
		}

		_, err = rt.Run(ctx, sess)
		if err != nil {
			slog.ErrorContext(ctx, "Agent execution failed", "agent", agentName, "error", err)
			return nil, ToolOutput{}, fmt.Errorf("agent execution failed: %w", err)
		}

		response := cmp.Or(sess.GetLastAssistantMessageContent(), "No response from agent")

		slog.DebugContext(ctx, "Agent execution completed", "agent", agentName, "response_length", len(response))

		return nil, ToolOutput{Response: response}, nil
	}
}

// agentToolAnnotations inspects the agent's tools and derives
// [mcp.ToolAnnotations] that describe the aggregate behaviour of the agent.
//
//   - ReadOnlyHint is true when every tool is read-only.
//   - DestructiveHint is explicitly false when no tool is destructive.
//   - IdempotentHint is true when every tool is idempotent.
//   - OpenWorldHint is explicitly false when no tool interacts with an open world.
func agentToolAnnotations(ctx context.Context, ag *agent.Agent) (*mcp.ToolAnnotations, error) {
	allTools, err := ag.Tools(ctx)
	if err != nil {
		return nil, err
	}

	readOnly := true
	destructive := false
	idempotent := true
	openWorld := false

	for _, tool := range allTools {
		a := tool.Annotations
		if !a.ReadOnlyHint {
			readOnly = false
		}
		if !a.IdempotentHint {
			idempotent = false
		}
		// *bool hints default to true per the MCP spec; nil means "assumed true".
		if optionalBool(a.DestructiveHint, true) {
			destructive = true
		}
		if optionalBool(a.OpenWorldHint, true) {
			openWorld = true
		}
	}

	annotations := &mcp.ToolAnnotations{
		ReadOnlyHint:   readOnly,
		IdempotentHint: idempotent,
	}
	// Only set *bool fields explicitly when they differ from the spec default
	// (true), so that nil keeps its "default" semantics on the wire.
	if !destructive {
		annotations.DestructiveHint = new(bool)
	}
	if !openWorld {
		annotations.OpenWorldHint = new(bool)
	}

	return annotations, nil
}

// optionalBool returns the value of p, or fallback when p is nil.
func optionalBool(p *bool, fallback bool) bool {
	if p != nil {
		return *p
	}
	return fallback
}
