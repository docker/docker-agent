package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/server/adka2a"
	adksession "google.golang.org/adk/session"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/httpsec"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/version"
)

// routableAddr replaces wildcard listen addresses (like "0.0.0.0" or "::") with
// "localhost" so the agent card URL is actually usable by clients.
func routableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("localhost", port)
	}
	return addr
}

type RunOptions struct {
	CLISafety      session.SafetyPolicy
	OnSafetyPolicy func(servesafety.Resolved)
	AuthToken      string
	CORSOrigin     string
}

func Run(ctx context.Context, agentFilename, agentName, sessionDB string, runConfig *config.RuntimeConfig, ln net.Listener, options RunOptions) error {
	slog.DebugContext(ctx, "Starting A2A server", "source", agentFilename, "agent", agentName, "addr", ln.Addr().String())

	agentSource, err := config.Resolve(agentFilename, nil)
	if err != nil {
		return err
	}

	t, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	if err != nil {
		return fmt.Errorf("failed to load agents: %w", err)
	}
	defer func() {
		if err := t.StopToolSets(ctx); err != nil {
			slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
		}
	}()

	selectedAgent, err := t.AgentOrDefault(agentName)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}
	resolvedSafety, err := servesafety.Resolve(options.CLISafety, string(selectedAgent.Safety()), string(t.RuntimeSafety()))
	if err != nil {
		return fmt.Errorf("resolve serve safety policy: %w", err)
	}
	if options.OnSafetyPolicy != nil {
		options.OnSafetyPolicy(resolvedSafety)
	}

	expandedSessionDB, err := pathx.ExpandHomeDir(sessionDB)
	if err != nil {
		return fmt.Errorf("failed to expand session db path: %w", err)
	}
	sessStore, err := sqlitestore.New(ctx, expandedSessionDB)
	if err != nil {
		return fmt.Errorf("failed to open session store: %w", err)
	}
	defer func() {
		if err := sessStore.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close session store", "error", err)
		}
	}()

	baseURL := &url.URL{Scheme: "http", Host: routableAddr(ln.Addr().String())}
	slog.DebugContext(ctx, "A2A server listening", "url", baseURL.String())

	e, err := newServer(t, agentFilename, agentName, sessStore, resolvedSafety, ln.Addr().String(), options)
	if err != nil {
		return fmt.Errorf("failed to create A2A server: %w", err)
	}

	// Stop serving when ctx is canceled so Run returns and the deferred
	// cleanups (session store, tool sets) release their resources.
	stop := context.AfterFunc(ctx, func() {
		_ = e.Server.Close()
	})
	defer stop()

	if err := e.Server.Serve(ln); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Failed to start server", "error", err)
		return err
	}

	return nil
}

func newServer(t *team.Team, agentFilename, agentName string, sessStore session.Store, safety servesafety.Resolved, listenAddr string, options RunOptions) (*echo.Echo, error) {
	adkAgent, err := newDockerAgentAdapter(t, agentName, sessStore, safety)
	if err != nil {
		return nil, err
	}

	baseURL := &url.URL{Scheme: "http", Host: routableAddr(listenAddr)}
	name := strings.TrimSuffix(filepath.Base(agentFilename), filepath.Ext(agentFilename))

	agentPath := "/invoke"
	agentCard := &a2a.AgentCard{
		Name:        name,
		Description: adkAgent.Description(),
		Skills: []a2a.AgentSkill{{
			ID:          fmt.Sprintf("%s_%s", name, agentName),
			Name:        agentName,
			Description: adkAgent.Description(),
			Tags:        []string{"llm", "docker agent"},
		}},
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		URL:                baseURL.JoinPath(agentPath).String(),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		Version:            version.Version,
		DefaultInputModes:  []string{},
		DefaultOutputModes: []string{},
	}

	executor := newExecutorWrapper(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:        name,
			Agent:          adkAgent,
			SessionService: adksession.InMemoryService(),
		},
	})

	// Start server
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	if options.CORSOrigin != "" {
		cfg, err := corsMiddlewareConfig(options.CORSOrigin)
		if err != nil {
			return nil, fmt.Errorf("invalid CORS origin: %w", err)
		}
		e.Use(middleware.CORSWithConfig(cfg))
	}
	if options.AuthToken != "" {
		e.Use(bearerAuthMiddleware(options.AuthToken))
	}
	e.Use(middleware.RequestLogger())

	// Wrap both A2A endpoints with otelhttp so the configured W3C
	// propagator extracts `traceparent` / `tracestate` / `baggage`
	// from incoming requests. The agent runtime started inside
	// `runDockerAgent` then chains its spans onto the calling agent's
	// trace, and the `gen_ai.conversation.id` baggage seeded by the
	// caller flows through into our local runtime spans without
	// per-call plumbing. The agent-card endpoint is included so
	// discovery requests carry the same trace context as the
	// downstream invocation — propagation is uniform across all
	// public surfaces of the server.
	cardHandler := otelhttp.NewHandler(
		a2asrv.NewStaticAgentCardHandler(agentCard),
		"a2a.agent_card",
	)
	jsonrpcHandler := otelhttp.NewHandler(
		a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)),
		"a2a.message",
	)
	e.GET(a2asrv.WellKnownAgentCardPath, echo.WrapHandler(cardHandler))
	e.POST(agentPath, echo.WrapHandler(jsonrpcHandler))

	return e, nil
}

func corsMiddlewareConfig(spec string) (middleware.CORSConfig, error) {
	origins, err := httpsec.ParseOrigins(spec)
	if err != nil {
		return middleware.CORSConfig{}, err
	}
	cfg := middleware.CORSConfig{
		AllowOrigins: origins.Literals(),
		AllowMethods: []string{http.MethodPost, http.MethodOptions},
		AllowHeaders: []string{"Authorization", "Content-Type", "Accept"},
		MaxAge:       86400,
	}
	if origins.HasPatterns() {
		cfg.AllowOriginFunc = func(origin string) (bool, error) { return origins.MatchPattern(origin), nil }
	}
	return cfg, nil
}

func bearerAuthMiddleware(token string) echo.MiddlewareFunc {
	auth := httpsec.BearerAuth(token)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodOptions {
				return next(c)
			}
			return echo.WrapMiddleware(auth)(next)(c)
		}
	}
}
