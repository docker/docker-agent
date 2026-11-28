package a2a

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	// go.mod pins adk/v2 to pseudo-version v2.2.1-0.20260818092052-16e33673bc9e:
	// OTel Logs 0.21 support (google/adk-go#1335) is merged upstream but not yet
	// tagged. Replace with the first v2 tag that contains that commit.
	"google.golang.org/adk/v2/runner"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	adksession "google.golang.org/adk/v2/session"

	"github.com/docker/docker-agent/pkg/a2a/a2astore"
	"github.com/docker/docker-agent/pkg/a2a/pushsender"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
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
	// PushAllowPrivateNetworks disables the SSRF guard on the push-notification
	// sender so webhooks resolving to loopback/private addresses are allowed.
	// Off by default (guard on, environment proxies ignored) per A2A spec
	// §13.2; enable only for trusted internal deployments or local testing.
	PushAllowPrivateNetworks bool
	// ToolApproval opts the server into out-of-band tool confirmation: a tool
	// call that would otherwise be denied in the non-interactive A2A session
	// instead parks the task in TASK_STATE_INPUT_REQUIRED until the client
	// replies with an approve/reject decision. Off by default.
	ToolApproval bool

	// approvals overrides the tool-approval registry (tests).
	approvals *approvalRegistry
}

// pushDrainTimeout bounds how long shutdown waits for queued webhooks.
const pushDrainTimeout = 5 * time.Second

// taskDeps holds the SDK task and push-notification dependencies.
type taskDeps struct {
	tasks       taskstore.Store
	pushConfigs push.ConfigStore
	pushSender  push.Sender
}

func Run(ctx context.Context, agentFilename, agentName, sessionDB string, runConfig *config.RuntimeConfig, ln net.Listener, options RunOptions) error {
	slog.DebugContext(ctx, "Starting A2A server", "source", agentFilename, "agent", agentName, "addr", ln.Addr().String())

	agentSource, err := sources.Resolve(agentFilename, nil)
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

	// Tasks and their push configs persist so webhooks survive a restart (the
	// SDK checks the task exists before any push-config operation). The store
	// is scoped to this session database: tasks carry no per-server owner.
	a2aStore, err := a2astore.New(ctx, a2astore.DirFor(expandedSessionDB))
	if errors.Is(err, a2astore.ErrInUse) {
		return fmt.Errorf("%w; give each A2A server its own --session-db", err)
	}
	if err != nil {
		return fmt.Errorf("failed to open A2A task store: %w", err)
	}
	defer func() {
		if err := a2aStore.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close A2A task store", "error", err)
		}
	}()

	pushSender := pushsender.New(pushsender.Config{AllowPrivateNetworks: options.PushAllowPrivateNetworks})
	// Runs before the stores close (defers are LIFO).
	defer func() {
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushDrainTimeout)
		defer cancel()
		if err := pushSender.Close(drainCtx); err != nil {
			slog.WarnContext(ctx, "Pending push notifications dropped on shutdown", "error", err)
		}
	}()
	if err := recoverInterruptedTasks(ctx, a2aStore, pushSender); err != nil {
		return err
	}

	baseURL := &url.URL{Scheme: "http", Host: routableAddr(ln.Addr().String())}
	slog.DebugContext(ctx, "A2A server listening", "url", baseURL.String())

	if options.approvals == nil {
		options.approvals = newApprovalRegistry(ctx, options.ToolApproval)
	}
	// Runs before the push drain and store closes: failing parked tasks pushes
	// and writes, and stopped runtimes persist their final state.
	defer options.approvals.close()

	workingDir, err := session.CaptureLocalWorkingDir(runConfig.WorkingDir)
	if err != nil {
		return err
	}

	e, err := newServer(ctx, t, agentFilename, agentName, sessStore, taskDeps{
		tasks:       a2aStore.Tasks(a2asrv.NewTaskStoreAuthenticator()),
		pushConfigs: a2aStore,
		pushSender:  pushSender,
	}, resolvedSafety, workingDir, ln.Addr().String(), options)
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

// interruptedReason is the status message of tasks failed on startup.
const interruptedReason = "Task interrupted by an A2A server restart"

// recoverInterruptedTasks fails the tasks a previous process was still running
// and notifies their webhooks: nothing would ever complete them.
func recoverInterruptedTasks(ctx context.Context, store *a2astore.Store, sender push.Sender) error {
	tasks, err := store.FailInterrupted(ctx, interruptedReason)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		slog.InfoContext(ctx, "Failed A2A task interrupted by restart", "task_id", task.ID)
		configs, err := store.List(ctx, task.ID)
		if err != nil {
			return err
		}
		event := &a2a.TaskStatusUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Status: task.Status}
		for _, cfg := range configs {
			if err := sender.SendPush(ctx, cfg, event); err != nil {
				slog.WarnContext(ctx, "Failed to notify interrupted task", "task_id", task.ID, "error", err)
			}
		}
	}
	return nil
}

// failAbandonedTask moves the task of a parked run dropped without an answer
// to state and notifies its webhooks: nothing would ever resume it.
func failAbandonedTask(deps taskDeps) abandonFunc {
	return func(ctx context.Context, run *pausedRun, state a2a.TaskState, reason string) *a2a.Task {
		// The task store resolves the owner from the call context.
		ctx, callCtx := a2asrv.NewCallContext(ctx, nil)
		if run.user != nil {
			callCtx.User = run.user
		}
		stored, err := deps.tasks.Get(ctx, run.taskID)
		if err != nil {
			slog.WarnContext(ctx, "Failed to load abandoned A2A task", "task_id", run.taskID, "error", err)
			return nil
		}
		if stored.Task.Status.State != a2a.TaskStateInputRequired {
			return nil
		}
		task := *stored.Task
		var msg *a2a.Message
		if reason != "" {
			msg = a2a.NewMessageForTask(a2a.MessageRoleAgent, &task, a2a.NewTextPart(reason))
		}
		event := a2a.NewStatusUpdateEvent(&task, state, msg)
		task.Status = event.Status
		if _, err := deps.tasks.Update(ctx, &taskstore.UpdateRequest{
			Task: &task, Event: event, PrevTask: stored.Task, PrevVersion: stored.Version,
		}); err != nil {
			// A concurrent reply already moved the task on.
			slog.WarnContext(ctx, "Failed to update abandoned A2A task", "task_id", run.taskID, "error", err)
			return nil
		}
		slog.InfoContext(ctx, "Ended A2A task with an abandoned tool approval", "task_id", run.taskID, "state", state)
		configs, err := deps.pushConfigs.List(ctx, run.taskID)
		if err != nil {
			slog.WarnContext(ctx, "Failed to list push configs of abandoned task", "task_id", run.taskID, "error", err)
			return &task
		}
		for _, cfg := range configs {
			if err := deps.pushSender.SendPush(ctx, cfg, event); err != nil {
				slog.WarnContext(ctx, "Failed to notify abandoned task", "task_id", run.taskID, "error", err)
			}
		}
		return &task
	}
}

func newServer(ctx context.Context, t *team.Team, agentFilename, agentName string, sessStore session.Store, deps taskDeps, safety servesafety.Resolved, workingDir, listenAddr string, options RunOptions) (*echo.Echo, error) {
	// The registry keeps runtimes alive between the request that parks on a
	// tool confirmation and the request that answers it. ctx is the server
	// lifetime: cancelling it (shutdown) cancels every parked run.
	approvals := options.approvals
	if approvals == nil {
		approvals = newApprovalRegistry(ctx, options.ToolApproval)
	}
	approvals.setAbandonHandler(failAbandonedTask(deps))

	adkAgent, err := newDockerAgentAdapter(t, agentName, sessStore, safety, workingDir, approvals)
	if err != nil {
		return nil, err
	}

	baseURL := &url.URL{Scheme: "http", Host: routableAddr(listenAddr)}
	name := strings.TrimSuffix(filepath.Base(agentFilename), filepath.Ext(agentFilename))

	agentPath := "/invoke"
	invokeURL := baseURL.JoinPath(agentPath).String()
	agentCard := &a2a.AgentCard{
		Name:        name,
		Description: adkAgent.Description(),
		Skills: []a2a.AgentSkill{{
			ID:          fmt.Sprintf("%s_%s", name, agentName),
			Name:        agentName,
			Description: adkAgent.Description(),
			Tags:        []string{"llm", "docker agent"},
		}},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(invokeURL, a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		Version:             version.Version,
		DefaultInputModes:   []string{},
		DefaultOutputModes:  []string{},
	}
	if approvals.toolApproval() {
		agentCard.Capabilities.Extensions = []a2a.AgentExtension{*toolApprovalExtension()}
	}

	executor := newExecutorWrapper(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:        name,
			Agent:          adkAgent,
			SessionService: adksession.InMemoryService(),
		},
	}, approvals)

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
	// Push notifications let clients register a webhook (via
	// tasks/pushNotificationConfig/set or the message config) that receives
	// task status/artifact updates as the agent runs.
	jsonrpcHandler := otelhttp.NewHandler(
		a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor,
			a2asrv.WithTaskStore(deps.tasks),
			a2asrv.WithPushNotifications(deps.pushConfigs, deps.pushSender),
			a2asrv.WithCapabilityChecks(&agentCard.Capabilities),
			a2asrv.WithCallInterceptors(&approvalInterceptor{approvals: approvals}),
		)),
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
		AllowHeaders: []string{"Authorization", "Content-Type", "Accept", a2a.SvcParamExtensions},
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
