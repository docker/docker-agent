// Package agui serves an agent over the AG-UI protocol: an HTTP endpoint
// takes a RunAgentInput and streams the run back as AG-UI events over SSE.
package agui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/echolog"
	"github.com/docker/docker-agent/pkg/httpsec"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/version"
)

const (
	// Path is the endpoint that takes a RunAgentInput.
	Path = "/ag-ui"

	// sessionOrigin marks the sessions this server may resume.
	sessionOrigin = "ag-ui"

	// maxRequestBytes bounds a RunAgentInput: clients resend their history.
	maxRequestBytes = 8 << 20

	// runDrainTimeout bounds how long shutdown waits for canceled runs.
	runDrainTimeout = 10 * time.Second
)

var (
	errThreadUnavailable = errors.New("threadId is not available")
	errNoNewMessage      = errors.New("messages: no new user message for this thread")
)

type RunOptions struct {
	CLISafety      session.SafetyPolicy
	OnSafetyPolicy func(servesafety.Resolved)
	AuthToken      string
	CORSOrigin     string
}

func Run(ctx context.Context, agentFilename, agentName, sessionDB string, runConfig *config.RuntimeConfig, ln net.Listener, options RunOptions) error {
	slog.DebugContext(ctx, "Starting AG-UI server", "source", agentFilename, "agent", agentName, "addr", ln.Addr().String())

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

	var configuredWd string
	if runConfig != nil {
		configuredWd = runConfig.WorkingDir
	}
	workingDir, err := session.CaptureLocalWorkingDir(configuredWd)
	if err != nil {
		return err
	}

	srv := newServer(t, selectedAgent.Name(), sessStore, resolvedSafety, workingDir)
	handler, err := srv.handler(options)
	if err != nil {
		return fmt.Errorf("failed to create AG-UI server: %w", err)
	}
	return srv.serve(ctx, &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}, ln)
}

type server struct {
	team       *team.Team
	agentName  string
	store      session.Store
	safety     servesafety.Resolved
	workingDir string

	// active holds the threads with a run in progress.
	active concurrent.Map[string, struct{}]

	// mu orders new runs against shutdown: none starts once closing is set.
	mu      sync.Mutex
	closing bool
	runs    sync.WaitGroup
}

// beginRun registers a run, unless the server is shutting down.
func (s *server) beginRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.runs.Add(1)
	return true
}

func newServer(t *team.Team, agentName string, store session.Store, safety servesafety.Resolved, workingDir string) *server {
	return &server{team: t, agentName: agentName, store: store, safety: safety, workingDir: workingDir}
}

func (s *server) handler(options RunOptions) (http.Handler, error) {
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
		// Preflights never get here: the CORS middleware answers them.
		e.Use(echo.WrapMiddleware(httpsec.BearerAuth(options.AuthToken)))
	}
	e.Use(echolog.RedactedRequestLogger())
	e.Use(middleware.BodyLimit(strconv.Itoa(maxRequestBytes)))

	e.POST(Path, s.handleRun)

	// otelhttp extracts the caller's trace context; the span covers the stream.
	return otelhttp.NewHandler(e, "agui.run"), nil
}

// serve runs httpServer until ctx is canceled, then returns once the runs it
// was serving have stopped using the session store.
func (s *server) serve(ctx context.Context, httpServer *http.Server, ln net.Listener) error {
	// Closing the connections cancels the in-flight runs.
	stop := context.AfterFunc(ctx, func() { _ = httpServer.Close() })
	defer stop()

	err := httpServer.Serve(ln)

	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		s.runs.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(runDrainTimeout):
		slog.WarnContext(ctx, "AG-UI runs still active at shutdown")
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Failed to start server", "error", err)
		return err
	}
	return nil
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

func writeError(c echo.Context, status int, message string) error {
	return c.JSON(status, map[string]string{"error": message})
}

func (s *server) handleRun(c echo.Context) error {
	in, err := decodeRunInput(c.Request().Body)
	if err != nil {
		if httpErr, ok := errors.AsType[*echo.HTTPError](err); ok {
			return httpErr
		}
		return writeError(c, http.StatusBadRequest, err.Error())
	}

	if !s.beginRun() {
		return writeError(c, http.StatusServiceUnavailable, "the server is shutting down")
	}
	defer s.runs.Done()
	if _, running := s.active.LoadOrStore(in.threadID, struct{}{}); running {
		return writeError(c, http.StatusConflict, "a run is already in progress for this threadId")
	}
	defer s.active.Delete(in.threadID)

	ctx := c.Request().Context()
	sess, err := s.resolveSession(ctx, in)
	switch {
	case errors.Is(err, errThreadUnavailable):
		return writeError(c, http.StatusConflict, err.Error())
	case errors.Is(err, errNoNewMessage):
		return writeError(c, http.StatusBadRequest, err.Error())
	case err != nil:
		slog.ErrorContext(ctx, "Failed to resolve AG-UI session", "thread_id", in.threadID, "error", err)
		return writeError(c, http.StatusInternalServerError, "failed to load the thread")
	}

	rt, err := runtime.New(ctx, s.team,
		runtime.WithCurrentAgent(s.agentName),
		runtime.WithModelsDevFallback(embedded.Snapshot),
		runtime.WithSessionStore(s.store),
		// Same tracer scope as the CLI, or runtime spans are no-ops.
		runtime.WithTracer(otel.Tracer(version.AppName)),
	)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create runtime", "error", err)
		return writeError(c, http.StatusInternalServerError, "failed to create runtime")
	}

	s.stream(c, rt, sess, in)
	return nil
}

// resolveSession returns the session of the thread with the new user messages
// appended, creating it for an unknown threadId.
func (s *server) resolveSession(ctx context.Context, in *runInput) (*session.Session, error) {
	a, err := s.team.AgentOrDefault(s.agentName)
	if err != nil {
		return nil, err
	}
	limits := []session.Opt{
		session.WithMaxIterations(a.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(a.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(a.MaxOldToolCallTokens()),
		session.WithMaxToolResultTokens(a.MaxToolResultTokens()),
	}

	stored, err := s.store.GetSessionByOrigin(ctx, in.threadID, sessionOrigin)
	switch {
	case err == nil:
		// Some stores hand out their own object: never append to it directly.
		sess := stored.Clone()
		messages := sess.GetAllMessages()
		known := make(map[string]bool, len(messages))
		for _, m := range messages {
			if m.Message.Role == chat.MessageRoleUser && m.Message.MessageID != "" {
				known[m.Message.MessageID] = true
			}
		}
		var fresh []userMessage
		for _, m := range in.newMessages {
			if !known[m.id] {
				fresh = append(fresh, m)
			}
		}
		// A retry of a failed run resends a message the thread already has.
		unanswered := len(messages) > 0 && messages[len(messages)-1].Message.Role == chat.MessageRoleUser
		if len(fresh) == 0 && !unanswered {
			return nil, errNoNewMessage
		}

		// Tool and history limits are not persisted.
		for _, opt := range limits {
			opt(sess)
		}
		sess.SetSafetyPolicy(servesafety.ResumeCeiling(sess.GetSafetyPolicy(), s.safety.Policy))
		sess.NonInteractive = true
		// The runtime stores the prompt it announces: not again on a retry.
		sess.SendUserMessage = len(fresh) > 0
		if err := s.addUserMessages(ctx, sess, fresh); err != nil {
			return nil, err
		}
		return sess, nil

	case !errors.Is(err, session.ErrNotFound):
		return nil, fmt.Errorf("look up AG-UI session: %w", err)
	}

	// The ID may belong to a session created through another entry point.
	switch _, err := s.store.GetSession(ctx, in.threadID); {
	case err == nil:
		return nil, errThreadUnavailable
	case !errors.Is(err, session.ErrNotFound):
		return nil, fmt.Errorf("check AG-UI threadId: %w", err)
	}

	sess := session.New(append(limits,
		session.WithID(in.threadID),
		session.WithOrigin(sessionOrigin),
		session.WithSafetyPolicy(s.safety.Policy),
		session.WithNonInteractive(true),
		session.WithWorkingDir(s.workingDir),
	)...)
	if err := s.addUserMessages(ctx, sess, in.newMessages); err != nil {
		return nil, err
	}
	return sess, nil
}

// addUserMessages appends messages to sess. The runtime stores the session
// and its prompt (the last message) when the run starts; the messages before
// the prompt are stored here.
func (s *server) addUserMessages(ctx context.Context, sess *session.Session, messages []userMessage) error {
	if len(messages) > 1 {
		if err := s.store.UpdateSession(ctx, sess); err != nil {
			return fmt.Errorf("store AG-UI session: %w", err)
		}
	}
	for i, m := range messages {
		msg := session.UserMessage(m.text)
		// Keeps the client ID so a resent message is recognised.
		msg.Message.MessageID = m.id
		if i < len(messages)-1 {
			if _, err := s.store.AddMessage(ctx, sess.ID, msg); err != nil {
				return fmt.Errorf("store AG-UI message: %w", err)
			}
		}
		sess.AddMessage(msg)
	}
	return nil
}

// runner is the part of the runtime a run needs.
type runner interface {
	RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event
	ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error
}

// stream runs the agent and writes its events as AG-UI events. It returns
// once the runtime has stopped, also when the client disconnects.
func (s *server) stream(c echo.Context, rt runner, sess *session.Session, in *runInput) {
	w := c.Response()
	w.Header().Set(echo.HeaderContentType, "text/event-stream")
	w.Header().Set(echo.HeaderCacheControl, "no-cache")
	w.Header().Set(echo.HeaderConnection, "keep-alive")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	ctx, cancel := context.WithCancel(c.Request().Context())
	defer cancel()

	// The SDK writer refuses a canceled context and logs every failed write.
	writer := sse.NewSSEWriter().WithLogger(slog.New(slog.DiscardHandler))
	writeCtx := context.WithoutCancel(ctx)
	writable := true
	write := func(evts []events.Event) {
		for _, ev := range evts {
			if !writable || ctx.Err() != nil {
				return
			}
			if err := writer.WriteEvent(writeCtx, w, ev); err != nil {
				// The client is gone: stop the run, keep draining it.
				slog.DebugContext(ctx, "AG-UI stream closed", "thread_id", in.threadID, "event_type", ev.Type(), "error", err)
				writable = false
				cancel()
			}
		}
	}

	tr := newTranslator(in.threadID, in.runID)
	for event := range rt.RunStream(ctx, sess) {
		// Nobody can answer over AG-UI yet, and the runtime waits for a reply.
		if e, ok := event.(*runtime.ElicitationRequestEvent); ok {
			_ = rt.ResumeElicitation(ctx, tools.ElicitationActionDecline, nil, e.ElicitationID)
		}
		write(tr.translate(event))
	}
	write(tr.end(ctx.Err() != nil))
}
