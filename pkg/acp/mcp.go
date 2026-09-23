package acp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

const clientMCPSetupTimeout = 30 * time.Second

func validateClientMCPServers(servers []acp.McpServer) ([]acp.McpServerStdio, error) {
	result := make([]acp.McpServerStdio, 0, len(servers))
	seen := make(map[string]bool)
	for i, server := range servers {
		if server.Stdio == nil || server.Http != nil || server.Sse != nil || server.Acp != nil {
			return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: only stdio transport is supported", i))
		}
		s := *server.Stdio
		if strings.TrimSpace(s.Name) == "" || seen[s.Name] {
			return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: name must be nonempty and unique", i))
		}
		seen[s.Name] = true
		if !filepath.IsAbs(s.Command) || strings.ContainsRune(s.Command, 0) {
			return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: command must be an absolute executable path", i))
		}
		for _, arg := range s.Args {
			if strings.ContainsRune(arg, 0) {
				return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: arguments must not contain NUL", i))
			}
		}
		for _, env := range s.Env {
			if env.Name == "" || strings.ContainsAny(env.Name, "=\x00") || strings.ContainsRune(env.Value, 0) {
				return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: invalid environment variable", i))
			}
		}
		s.Args = slices.Clone(s.Args)
		s.Env = slices.Clone(s.Env)
		s.Meta = nil
		result = append(result, s)
	}
	return result, nil
}

// clientMCPTools is a stable view shared by the session's agents, not a lifecycle owner.
type clientMCPTools struct {
	mu      sync.Mutex
	current *clientMCPGeneration
}

func (v *clientMCPTools) generation() *clientMCPGeneration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.current
}

func (v *clientMCPTools) swap(next *clientMCPGeneration) *clientMCPGeneration {
	v.mu.Lock()
	defer v.mu.Unlock()
	previous := v.current
	if previous != nil {
		previous.retire()
	}
	v.current = next
	return previous
}

func (v *clientMCPTools) Tools(ctx context.Context) ([]tools.Tool, error) {
	g := v.generation()
	if g == nil {
		return nil, nil
	}
	ctx, release, err := g.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	var result []tools.Tool
	for _, server := range g.servers {
		available, err := server.Tools(ctx)
		if err != nil {
			return nil, err
		}
		for _, tool := range available {
			originalName := tool.Name
			tool.Name = clientMCPToolName(g.namespace, originalName)
			handler := tool.Handler
			tool.Handler = func(ctx context.Context, call tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
				ctx, release, err := g.acquire(ctx)
				if err != nil {
					return tools.ResultError("MCP server configuration was replaced; retry with the current tools"), nil
				}
				defer release()
				call.Function.Name = originalName
				return handler(ctx, call, rt)
			}
			result = append(result, tool)
		}
	}
	return result, nil
}

func (v *clientMCPTools) Instructions() string {
	g := v.generation()
	if g == nil {
		return ""
	}
	return g.instructions
}

// Hashing avoids provider limits and collisions for arbitrary MCP tool names.
func clientMCPToolName(namespace, name string) string {
	hash := sha256.Sum256([]byte(namespace + "\x00" + name))
	return fmt.Sprintf("acp_%x", hash[:24])
}

type clientMCPGeneration struct {
	namespace    string
	servers      []*mcptools.Toolset
	instructions string
	cancel       context.CancelFunc
	ctx          context.Context //nolint:containedctx // owns the generation independently of setup requests
	mu           sync.Mutex
	retired      bool
	calls        sync.WaitGroup
	closeOnce    sync.Once
	closeErr     error
}

func (g *clientMCPGeneration) acquire(ctx context.Context) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return nil, nil, errors.New("MCP generation retired")
	}
	g.calls.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		g.calls.Done()
	}, nil
}

func (g *clientMCPGeneration) retire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retired = true
	g.cancel()
}

func (g *clientMCPGeneration) close(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() {
		g.retire()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clientMCPSetupTimeout)
		defer cancel()
		// Start transport shutdown before joining calls: cancellation alone cannot
		// interrupt a blocked pipe write, but each Stop arms a deadline-based kill.
		errs := make([]error, len(g.servers))
		var stops sync.WaitGroup
		for i, server := range g.servers {
			stops.Go(func() { errs[i] = server.Stop(cleanupCtx) })
		}
		stops.Wait()
		g.calls.Wait()
		g.closeErr = errors.Join(errs...)
	})
	return g.closeErr
}

func prepareClientMCP(ctx context.Context, servers []acp.McpServerStdio, workingDir string) (*clientMCPGeneration, error) {
	ctx, cancel := context.WithTimeout(ctx, clientMCPSetupTimeout)
	defer cancel()
	lifetime, stop := context.WithCancel(context.WithoutCancel(ctx))
	g := &clientMCPGeneration{ctx: lifetime, cancel: stop, namespace: uuid.NewV4().String()}
	prefix := "acp_" + strings.ReplaceAll(uuid.NewV4().String(), "-", "")[:16]
	var instructions []string
	for i, spec := range servers {
		env := os.Environ()
		for _, variable := range spec.Env {
			env = append(env, variable.Name+"="+variable.Value)
		}
		server := mcptools.NewSessionToolsetCommand(prefix+"_"+strconv.Itoa(i), spec.Command, spec.Args, env, workingDir)
		tools.ConfigureHandlers(server, tools.ScopedElicitationHandler, tools.SamplingScopeHandler, tools.SamplingWithToolsScopeHandler, nil, false, "")
		g.servers = append(g.servers, server)
		if err := server.Start(ctx); err != nil {
			return g, fmt.Errorf("starting client MCP server %d: %w", i, err)
		}
		if _, err := server.Tools(ctx); err != nil {
			return g, fmt.Errorf("listing client MCP server %d tools: %w", i, err)
		}
		if instruction := server.Instructions(); instruction != "" {
			instructions = append(instructions, instruction)
		}
	}
	if err := ctx.Err(); err != nil {
		return g, err
	}
	g.instructions = strings.Join(instructions, "\n\n")
	return g, nil
}
