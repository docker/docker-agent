package acp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coder/acp-go-sdk"
	"golang.org/x/net/http/httpguts"

	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

const clientMCPSetupTimeout = 30 * time.Second

type clientMCPServer struct {
	stdio     *acp.McpServerStdio
	url       string
	transport string
	headers   map[string]string
}

func validateClientMCPServers(servers []acp.McpServer) ([]clientMCPServer, error) {
	result := make([]clientMCPServer, 0, len(servers))
	seen := make(map[string]bool)
	for i, server := range servers {
		var spec clientMCPServer
		var name string
		var headers []acp.HttpHeader
		variants := 0
		if server.Stdio != nil {
			variants++
		}
		if server.Http != nil {
			variants++
		}
		if server.Sse != nil {
			variants++
		}
		if server.Acp != nil {
			variants++
		}
		if variants != 1 || server.Acp != nil {
			return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: expected one stdio, http, or sse transport", i))
		}
		switch {
		case server.Stdio != nil:
			s := *server.Stdio
			name = s.Name
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
			s.Args, s.Env, s.Meta = slices.Clone(s.Args), slices.Clone(s.Env), nil
			spec.stdio = &s
		case server.Http != nil:
			s := server.Http
			if s.Type != "http" {
				return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: invalid http discriminator", i))
			}
			name, spec.url, spec.transport, headers = s.Name, s.Url, "streamable", s.Headers
		case server.Sse != nil:
			s := server.Sse
			if s.Type != "sse" {
				return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: invalid sse discriminator", i))
			}
			name, spec.url, spec.transport, headers = s.Name, s.Url, "sse", s.Headers
		}
		if strings.TrimSpace(name) == "" || seen[name] {
			return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: name must be nonempty and unique", i))
		}
		seen[name] = true
		if spec.stdio == nil {
			u, err := url.Parse(spec.url)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.Contains(spec.url, "#") || u.Opaque != "" {
				return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: expected an absolute HTTP(S) URL without userinfo or fragment", i))
			}
			spec.headers = make(map[string]string, len(headers))
			for _, header := range headers {
				key := http.CanonicalHeaderKey(header.Name)
				_, duplicate := spec.headers[key]
				if !httpguts.ValidHeaderFieldName(header.Name) || !httpguts.ValidHeaderFieldValue(header.Value) || duplicate || reservedClientMCPHeader(key) {
					return nil, acp.NewInvalidParams(fmt.Sprintf("MCP server %d: invalid, duplicate, or reserved HTTP header", i))
				}
				spec.headers[key] = header.Value
			}
		}
		result = append(result, spec)
	}
	return result, nil
}

func reservedClientMCPHeader(name string) bool {
	if strings.HasPrefix(name, "Mcp-") || strings.HasPrefix(name, "Proxy-") {
		return true
	}
	switch name {
	case "Host", "Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Trailer", "Upgrade", "Content-Length", "Content-Type", "Content-Encoding", "Accept", "Accept-Encoding", "Expect", "Last-Event-Id", "Idempotency-Key", "X-Idempotency-Key":
		return true
	}
	return false
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

func prepareClientMCP(ctx context.Context, servers []clientMCPServer, workingDir string) (*clientMCPGeneration, error) {
	ctx, cancel := context.WithTimeout(ctx, clientMCPSetupTimeout)
	defer cancel()
	lifetime, stop := context.WithCancel(context.WithoutCancel(ctx))
	g := &clientMCPGeneration{ctx: lifetime, cancel: stop, namespace: uuid.NewV4().String()}
	prefix := "acp_" + strings.ReplaceAll(uuid.NewV4().String(), "-", "")[:16]
	var instructions []string
	for i, spec := range servers {
		name := prefix + "_" + strconv.Itoa(i)
		var server *mcptools.Toolset
		if spec.stdio != nil {
			s := spec.stdio
			env := os.Environ()
			for _, variable := range s.Env {
				env = append(env, variable.Name+"="+variable.Value)
			}
			server = mcptools.NewSessionToolsetCommand(name, s.Command, s.Args, env, workingDir)
		} else {
			server = mcptools.NewSessionRemoteToolset(name, spec.url, spec.transport, spec.headers)
		}
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
