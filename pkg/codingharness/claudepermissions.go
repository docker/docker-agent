package codingharness

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	baseharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/version"
)

const (
	// Claude Code consults this MCP tool, through --permission-prompt-tool,
	// before a tool that waits for the user. The flag is also what enables
	// AskUserQuestion and the plan mode tools under --print.
	claudePermissionServerName = "docker-agent-harness"
	claudePermissionToolName   = "permission_prompt"

	// claudePermissionTimeout is how long a question may wait for the user:
	// without it, Claude Code abandons an HTTP tool call after 60 seconds.
	claudePermissionTimeout = 24 * time.Hour
)

// WithElicitation returns a provider whose turns put Claude Code's questions
// and plan approvals to the user through elicit.
func (p claudeCodeProvider) WithElicitation(elicit tools.ElicitationHandler) harness.Provider {
	p.elicit = elicit
	return p
}

func (p claudeCodeProvider) Run(ctx context.Context, prompt string, handle func(harness.Event)) error {
	return p.withPermissionPrompts(ctx, func(cli baseharness.Provider) error {
		return baseharness.Run(ctx, cli, prompt, adapt(handle))
	})
}

func (p claudeCodeProvider) Resume(ctx context.Context, sessionID, prompt string, handle func(harness.Event)) error {
	return p.withPermissionPrompts(ctx, func(cli baseharness.Provider) error {
		return baseharness.Resume(ctx, cli, sessionID, prompt, adapt(handle))
	})
}

// withPermissionPrompts runs one turn with the permission prompt server up.
// When it cannot start, the turn still runs: Claude Code then asks in plain
// text, as it does without elicitation.
func (p claudeCodeProvider) withPermissionPrompts(ctx context.Context, run func(baseharness.Provider) error) error {
	cli, ok := p.Provider.(baseharness.ResumableCommandProvider)
	if p.elicit == nil || !ok {
		return run(p.Provider)
	}
	server, err := startClaudePermissionServer(ctx, p.elicit)
	if err != nil {
		slog.WarnContext(ctx, "Claude Code questions will be asked in plain text: the permission prompt server did not start", "error", err)
		return run(p.Provider)
	}
	defer server.Close()
	return run(claudePermissionArgs{ResumableCommandProvider: cli, flags: server.flags})
}

// claudePermissionArgs adds the permission prompt flags to the upstream
// command, so its hardened runner still drives the CLI.
type claudePermissionArgs struct {
	baseharness.ResumableCommandProvider

	flags []string
}

func (a claudePermissionArgs) PrintArgs(prompt string) []string {
	return withClaudeFlags(a.ResumableCommandProvider.PrintArgs(prompt), a.flags)
}

func (a claudePermissionArgs) ResumeArgs(sessionID, prompt string) []string {
	return withClaudeFlags(a.ResumableCommandProvider.ResumeArgs(sessionID, prompt), a.flags)
}

// withClaudeFlags puts flags right after the binary, ahead of the prompt.
func withClaudeFlags(args, flags []string) []string {
	if len(args) == 0 {
		return args
	}
	return slices.Concat(args[:1], flags, args[1:])
}

// claudePermissionServer serves the permission prompt tool on loopback for
// one turn. A bearer token keeps other local processes from reaching it; it
// lives in the MCP config file (0600, in a 0700 directory) rather than on
// the command line, which other users can read.
type claudePermissionServer struct {
	server *http.Server
	cancel context.CancelFunc
	dir    string
	flags  []string
}

func startClaudePermissionServer(ctx context.Context, elicit tools.ElicitationHandler) (*claudePermissionServer, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := rand.Text()
	// Close cancels it, so a question still open when the CLI exits ends
	// with the turn.
	ctx, cancel := context.WithCancel(ctx)

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: claudePermissionServerName, Version: version.Version}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{},
	})
	mcpServer.AddTool(&mcp.Tool{
		Name:        claudePermissionToolName,
		Description: "Asks the docker-agent user before a Claude Code tool that needs them.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name":   map[string]any{"type": "string"},
				"input":       map[string]any{"type": "object"},
				"tool_use_id": map[string]any{"type": "string"},
			},
			"required": []any{"tool_name", "input"},
		},
	}, claudePermissionHandler(ctx, elicit))

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true})
	server := &http.Server{
		Handler:           requireBearerToken(token, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.WarnContext(ctx, "Claude Code permission prompt server stopped", "error", err)
		}
	}()

	s := &claudePermissionServer{server: server, cancel: cancel}
	s.dir, err = os.MkdirTemp("", "docker-agent-claude-")
	if err != nil {
		s.Close()
		return nil, err
	}
	config, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			claudePermissionServerName: map[string]any{
				"type":    "http",
				"url":     "http://" + listener.Addr().String() + "/mcp",
				"headers": map[string]string{"Authorization": "Bearer " + token},
				"timeout": claudePermissionTimeout.Milliseconds(),
			},
		},
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	configPath := filepath.Join(s.dir, "mcp.json")
	if err := atomicfile.Write(configPath, bytes.NewReader(config), 0o600); err != nil {
		s.Close()
		return nil, err
	}
	s.flags = []string{
		"--mcp-config", configPath,
		"--permission-prompt-tool", "mcp__" + claudePermissionServerName + "__" + claudePermissionToolName,
	}
	return s, nil
}

func (s *claudePermissionServer) Close() {
	s.cancel()
	_ = s.server.Close()
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

func requireBearerToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// claudePermissionHandler answers Claude Code's permission prompts. The
// elicitation runs under the turn's context, whose values the runtime reads,
// and stops early if Claude Code drops the request.
func claudePermissionHandler(turnCtx context.Context, elicit tools.ElicitationHandler) mcp.ToolHandler {
	return func(reqCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var call struct {
			ToolName string         `json:"tool_name"`
			Input    map[string]any `json:"input"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &call); err != nil {
			return nil, err
		}

		ctx, cancel := context.WithCancel(turnCtx)
		defer cancel()
		defer context.AfterFunc(reqCtx, cancel)()

		decision, err := json.Marshal(decideClaudePermission(ctx, elicit, call.ToolName, call.Input))
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(decision)}}}, nil
	}
}
