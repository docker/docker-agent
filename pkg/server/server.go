package server

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/echolog"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/upstream"
)

type Server struct {
	e         *echo.Echo
	sm        *SessionManager
	authToken string
	// heartbeatInterval is how often an idle /events stream emits an SSE
	// comment (": ping") so clients can tell a quiet session from a dead
	// transport. SSE comments are invisible to EventSource clients and carry
	// no id, so they never interfere with the sequenced stream or its replay.
	heartbeatInterval time.Duration
}

func New(ctx context.Context, sessionStore session.Store, runConfig *config.RuntimeConfig, refreshInterval time.Duration, agentSources config.Sources, authToken string, maxRequestBytes int64, opts ...SessionManagerOpt) (*Server, error) {
	return NewWithManager(NewSessionManager(ctx, agentSources, sessionStore, refreshInterval, runConfig, opts...), authToken, WithMaxRequestBytes(maxRequestBytes)), nil
}

const defaultMaxRequestBytes int64 = 1 << 20 // 1 MiB

// Option configures a [Server] at construction time.
type Option func(*serverOptions)

type serverOptions struct {
	maxRequestBytes int64
}

// WithMaxRequestBytes sets the maximum request body size in bytes. Requests
// whose body exceeds the limit are rejected with HTTP 413. Zero or negative
// values fall back to the default (1 MiB).
func WithMaxRequestBytes(n int64) Option {
	return func(o *serverOptions) { o.maxRequestBytes = n }
}

// NewWithManager builds a Server around an already-constructed SessionManager.
// Useful when the runtime is owned by another component (e.g. the TUI) and
// only needs to be exposed over HTTP.
func NewWithManager(sm *SessionManager, authToken string, opts ...Option) *Server {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}
	maxBytes := o.maxRequestBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxRequestBytes
	}

	e := echo.New()
	e.Use(echolog.RedactedRequestLogger())
	e.Use(middleware.BodyLimit(strconv.FormatInt(maxBytes, 10)))
	e.Use(echo.WrapMiddleware(upstream.Handler))

	// Add bearer token middleware if token is configured
	if authToken != "" {
		e.Use(BearerTokenMiddleware(authToken))
	}

	s := &Server{e: e, sm: sm, authToken: authToken, heartbeatInterval: defaultEventsHeartbeatInterval}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// Health and readiness endpoints (not under /api)
	s.e.GET("/health", s.health)
	s.e.GET("/ready", s.ready)

	group := s.e.Group("/api")

	group.GET("/agents", s.getAgents)
	group.GET("/agents/:id", s.getAgentConfig)

	group.GET("/sessions", s.getSessions)
	group.GET("/sessions/:id", s.getSession)
	group.GET("/sessions/:id/status", s.getSessionStatus)
	group.GET("/sessions/:id/snapshot", s.getSessionSnapshot)
	group.POST("/sessions/:id/resume", s.resumeSession)
	group.POST("/sessions/:id/tools/toggle", s.toggleSessionYolo)
	group.PATCH("/sessions/:id/safety-policy", s.updateSessionSafetyPolicy)
	group.PATCH("/sessions/:id/permissions", s.updateSessionPermissions)
	group.PATCH("/sessions/:id/title", s.updateSessionTitle)
	group.PATCH("/sessions/:id/tokens", s.updateSessionTokens)
	group.PATCH("/sessions/:id/starred", s.setSessionStarred)
	group.GET("/sessions/:id/models", s.getSessionModels)
	group.POST("/sessions", s.createSession)
	group.POST("/sessions/:id/fork", s.forkSession)
	group.DELETE("/sessions/:id", s.deleteSession)
	group.POST("/sessions/:id/agent/:agent", s.runAgent)
	group.POST("/sessions/:id/agent/:agent/:agent_name", s.runAgent)
	group.POST("/sessions/:id/elicitation", s.elicitation)
	group.POST("/sessions/:id/steer", s.steerSession)
	group.POST("/sessions/:id/followup", s.followUpSession)
	group.GET("/sessions/:id/events", s.sessionEvents)
	group.POST("/sessions/:id/messages", s.addMessage)
	group.PATCH("/sessions/:id/messages/:msg_id", s.updateMessage)
	group.POST("/sessions/:id/summaries", s.addSummary)
	group.GET("/sessions/:id/queue", s.getSessionQueueStatus)
	group.GET("/sessions/:id/recovery", s.getSessionRecoveryData)
	group.POST("/sessions/batch/delete", s.batchDeleteSessions)
	group.POST("/sessions/batch/export", s.batchExportSessions)

	group.GET("/agents/:id/:agent_name/tools/count", s.getAgentToolCount)

	group.POST("/mcp-oauth/callback", s.mcpOAuthCallback)

	group.GET("/ping", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	group.GET("/ready", s.sessionsReady)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Wrap the Echo handler with otelhttp so the configured W3C
	// propagator extracts `traceparent` / `tracestate` / `baggage`
	// from incoming API requests. Without this the API server's
	// runtime spans (already wired via `WithTracer` in the session
	// manager) start fresh trace ids per request rather than
	// chaining onto the calling client's trace.
	srv := http.Server{
		Handler:           otelhttp.NewHandler(s.e, "agent-api"),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Failed to start server", "error", err)
		return err
	}

	return nil
}

const maxAPITimeout = 5 * time.Minute

// ready blocks until at least one session is registered. The caller
// may supply a ?timeout=<duration> query parameter (default 30s, max 5m).
func (s *Server) sessionsReady(c echo.Context) error {
	timeout := 30 * time.Second
	if v := c.QueryParam("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid timeout: %v", err))
		}
		timeout = min(d, maxAPITimeout)
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), timeout)
	defer cancel()

	if err := s.sm.WaitReady(ctx); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "no sessions registered within timeout")
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) getAgents(c echo.Context) error {
	agents := []api.Agent{}
	for k, agentSource := range s.sm.Sources {
		slog.Debug("API source", "source", agentSource.Name())

		cfg, err := config.Load(c.Request().Context(), agentSource)
		if err != nil {
			slog.Error("Failed to load config from API source", "key", k, "error", err)
			continue
		}

		agent, ok := agentsAPIEntry(k, cfg)
		if !ok {
			slog.Warn("No agents found in config from API source", "key", k)
			continue
		}
		agents = append(agents, agent)
	}

	slices.SortFunc(agents, func(a, b api.Agent) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return c.JSON(http.StatusOK, agents)
}

// agentsAPIEntry summarizes a loaded config into the api.Agent listing
// entry for /api/agents. The len(cfg.Agents)==0 check MUST run before any
// access to cfg.Agents (e.g. First()), which panics on an empty slice: this
// guards the handler even though validateConfig already rejects agent-less
// configs at load time (defense in depth against a bypass or future
// regression in that check).
func agentsAPIEntry(name string, cfg *latest.Config) (api.Agent, bool) {
	if len(cfg.Agents) == 0 {
		return api.Agent{}, false
	}
	root := cfg.Agents.First()
	var commands []string
	if len(root.Commands) > 0 {
		commands = make([]string, 0, len(root.Commands))
		for k := range root.Commands {
			commands = append(commands, k)
		}
		slices.Sort(commands)
	}
	return api.Agent{
		Name:        name,
		Multi:       len(cfg.Agents) > 1,
		Description: root.Description,
		Commands:    commands,
	}, true
}

func (s *Server) getAgentConfig(c echo.Context) error {
	agentID := c.Param("id")

	for k, agentSource := range s.sm.Sources {
		if k != agentID {
			continue
		}

		slog.Debug("API source", "source", agentSource.Name())
		cfg, err := config.Load(c.Request().Context(), agentSource)
		if err != nil {
			slog.Error("Failed to load config from API source", "key", k, "error", err)
			continue
		}

		return c.JSON(http.StatusOK, cfg)
	}

	return echo.NewHTTPError(http.StatusNotFound)
}

func (s *Server) getSessions(c echo.Context) error {
	sessions, err := s.sm.GetSessions(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to get sessions: %v", err))
	}

	responses := make([]api.SessionsResponse, len(sessions))
	for i, sess := range sessions {
		// The in-memory store hands back live session pointers, so read the
		// mutable scalars through one locked snapshot each.
		title := sess.TitleSnapshot()
		inputTokens, outputTokens := sess.Usage()
		responses[i] = api.SessionsResponse{
			ID:           sess.ID,
			Title:        title,
			CreatedAt:    sess.CreatedAt.Format(time.RFC3339),
			NumMessages:  len(sess.GetAllMessages()),
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			WorkingDir:   sess.WorkingDir,
		}
	}
	return c.JSON(http.StatusOK, responses)
}

func (s *Server) createSession(c echo.Context) error {
	var sessionTemplate session.Session
	if err := c.Bind(&sessionTemplate); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	sess, err := s.sm.CreateSession(c.Request().Context(), &sessionTemplate)
	if err != nil {
		if errors.Is(err, ErrInvalidWorkingDir) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to create session: %v", err))
	}

	return c.JSON(http.StatusOK, sess)
}

// forkSession creates a new session whose history is a deep copy of
// an existing session up to (but excluding) the Nth user message. The
// new session uses a fork-numbered title and starts with no runtime
// attached.
func (s *Server) forkSession(c echo.Context) error {
	var req api.ForkSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	forked, err := s.sm.ForkSession(c.Request().Context(), c.Param("id"), req.UserMessageIndex)
	if err != nil {
		switch {
		case errors.Is(err, ErrForkOutOfRange),
			errors.Is(err, ErrForkInSubSession):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to fork session: %v", err))
	}

	return c.JSON(http.StatusOK, api.SessionResponse{
		ID:            forked.ID,
		Title:         forked.Title,
		CreatedAt:     forked.CreatedAt,
		Messages:      forked.GetAllMessages(),
		ToolsApproved: forked.ToolsApproved,
		SafetyPolicy:  forked.SafetyPolicy,
		InputTokens:   forked.InputTokens,
		OutputTokens:  forked.OutputTokens,
		WorkingDir:    forked.WorkingDir,
		Permissions:   forked.Permissions,
	})
}

func (s *Server) getSession(c echo.Context) error {
	sess, err := s.sm.GetSession(c.Request().Context(), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("session not found: %v", err))
	}

	title := sess.TitleSnapshot()
	inputTokens, outputTokens := sess.Usage()
	return c.JSON(http.StatusOK, api.SessionResponse{
		ID:            sess.ID,
		Title:         title,
		CreatedAt:     sess.CreatedAt,
		Messages:      sess.GetAllMessages(),
		ToolsApproved: sess.ToolsApproved,
		SafetyPolicy:  sess.SafetyPolicy,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		WorkingDir:    sess.WorkingDir,
		Permissions:   sess.Permissions,
	})
}

// getSessionStatus returns the session's current runtime state. With
// ?wait=<duration> it blocks until the session's runtime is attached (ready
// to accept follow-ups and produce events) before responding, so a client
// that just launched a run can wait for that exact session instead of polling.
func (s *Server) getSessionStatus(c echo.Context) error {
	if v := c.QueryParam("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid wait: %v", err))
		}
		if !s.sm.WaitSessionAttached(c.Request().Context(), c.Param("id"), min(d, maxAPITimeout)) {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "session not ready within timeout")
		}
	}

	status, err := s.sm.GetSessionStatus(c.Request().Context(), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("session not found: %v", err))
	}
	return c.JSON(http.StatusOK, status)
}

// getSessionSnapshot returns the full state of a session in one response
// (stored fields + live runtime state + last event sequence number) so a
// client can rebuild its view and then tail /events?since=<last_event_seq>
// without missing any transition.
//
// An unknown session yields a 404 whose body carries
// [api.ErrCodeUnknownSession], so clients can tell "this server does not
// have that session" (wait or recreate) from a route-less 404 produced by a
// binary that predates this endpoint (upgrade/relaunch).
func (s *Server) getSessionSnapshot(c echo.Context) error {
	snapshot, err := s.sm.GetSessionSnapshot(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, api.ErrorResponse{
			Code:    api.ErrCodeUnknownSession,
			Message: fmt.Sprintf("session not found: %v", err),
		})
	}
	return c.JSON(http.StatusOK, snapshot)
}

func (s *Server) resumeSession(c echo.Context) error {
	var req api.ResumeSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.ResumeSession(c.Request().Context(), c.Param("id"), req.Confirmation, req.Reason, req.ToolName); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to resume session: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "session resumed"})
}

func (s *Server) toggleSessionYolo(c echo.Context) error {
	if err := s.sm.ToggleToolApproval(c.Request().Context(), c.Param("id")); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to toggle session tool approval mode: %v", err))
	}
	return c.JSON(http.StatusOK, nil)
}

func (s *Server) updateSessionSafetyPolicy(c echo.Context) error {
	var req api.UpdateSessionSafetyPolicyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	if err := s.sm.SetSessionSafetyPolicy(c.Request().Context(), c.Param("id"), req.SafetyPolicy); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "session safety policy updated"})
}

func (s *Server) getAgentToolCount(c echo.Context) error {
	count, err := s.sm.GetAgentToolCount(c.Request().Context(), c.Param("id"), c.Param("agent_name"))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to get agent tool count: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]int{"available_tools": count})
}

func (s *Server) updateSessionPermissions(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.UpdateSessionPermissionsRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.UpdateSessionPermissions(c.Request().Context(), sessionID, req.Permissions); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to update session permissions: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "session permissions updated"})
}

func (s *Server) updateSessionTitle(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.UpdateSessionTitleRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.UpdateSessionTitle(c.Request().Context(), sessionID, req.Title); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to update session title: %v", err))
	}

	return c.JSON(http.StatusOK, api.UpdateSessionTitleResponse{
		ID:    sessionID,
		Title: req.Title,
	})
}

func (s *Server) deleteSession(c echo.Context) error {
	sessionID := c.Param("id")

	timeout := 10 * time.Second
	if v := c.QueryParam("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid timeout: %v", err))
		}
		timeout = min(d, maxAPITimeout)
	}

	if err := s.sm.DeleteSession(c.Request().Context(), sessionID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to delete session: %v", err))
	}

	// When ?wait=true, block until the runtime's stream goroutine has
	// fully exited (the streaming mutex is released) or the timeout fires.
	if c.QueryParam("wait") == "true" {
		if err := s.sm.WaitStopped(c.Request().Context(), sessionID, timeout); err != nil {
			return c.JSON(http.StatusAccepted, map[string]string{"message": "session deleted, stop still in progress"})
		}
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "session deleted"})
}

func (s *Server) runAgent(c echo.Context) error {
	sessionID := c.Param("id")
	agentFilename := c.Param("agent")
	// agent_name may be empty when the route /api/sessions/:id/agent/:agent
	// is used. In that case, the session manager resolves the team's default
	// agent (one explicitly named "root" if it exists, otherwise the first
	// agent declared).
	currentAgent := c.Param("agent_name")

	slog.Debug("Running agent", "agent_filename", agentFilename, "session_id", sessionID, "current_agent", currentAgent)

	var req api.RunAgentRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	streamChan, err := s.sm.RunSession(c.Request().Context(), sessionID, agentFilename, currentAgent, req.Messages, req.Model)
	if err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		if errors.Is(err, ErrModelSwitchingNotSupported) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to run session: %v", err))
	}

	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)
	for {
		select {
		case event, ok := <-streamChan:
			if !ok {
				return nil
			}
			data, err := json.Marshal(event)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to marshal event: %v", err))
			}
			fmt.Fprintf(c.Response(), "data: %s\n\n", string(data))
			c.Response().Flush()
		case <-c.Request().Context().Done():
			slog.DebugContext(c.Request().Context(), "Client disconnected from stream", "session_id", sessionID)
			return nil
		}
	}
}

func (s *Server) elicitation(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.ResumeElicitationRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.ResumeElicitation(c.Request().Context(), sessionID, req.Action, req.Content, req.ElicitationID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to resume elicitation: %v", err))
	}

	return c.JSON(http.StatusOK, nil)
}

// mcpOAuthCallback is the out-of-band entry point used by embedders
// that receive an OAuth deeplink (e.g. a system-wide URL-scheme handler
// or an OS-integrated launcher) and want to forward the resulting
// {code, state} to docker-agent without going through the session-keyed
// ResumeElicitation path.
//
// The state value is opaque, high-entropy and was generated in-process by
// docker-agent's unmanaged OAuth flow (see GenerateState in
// pkg/tools/mcp). Looking it up in the pending-oauth registry IS the
// per-request authorization: docker-agent only accepts callbacks for
// states it is currently awaiting. An unknown state returns 404 (which
// is the expected outcome for replays and any state value the agent did
// not itself generate).
//
// Threat model:
//
//   - The registry is the primary defence. State values are >=128-bit
//     opaque tokens from GenerateState; an attacker that has not
//     observed a live state cannot brute-force one in a useful window.
//   - State values DO appear in transit: in the elicitation Meta on the
//     session SSE stream (visible only to the connected client), and in
//     the authorize URL the user opens (visible to the user's browser
//     and the authorization server). They are also written to debug
//     logs when --debug is on.
//   - If an attacker DOES observe a live state (e.g. via leaked debug
//     logs or a compromised host), they could POST here with an
//     attacker-controlled code; the resulting token would be bound to
//     the attacker's account, not the user's. Setting --auth-token
//     blocks this regardless of state leakage, because the route then
//     also requires bearer auth.
//   - Operators running docker-agent on a network-reachable interface
//     SHOULD configure --auth-token. Defaults to localhost-only via the
//     existing socket binding when not overridden.
//
// The handler never blocks: it hands the callback to the buffered
// channel of the waiting flow and returns immediately. The token
// exchange and storage happen inside that flow's goroutine, which then
// emits the existing authorization_event on the session SSE stream.
func (s *Server) mcpOAuthCallback(c echo.Context) error {
	q := c.QueryParams()
	state := q.Get("state")
	if state == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing state query parameter")
	}
	code := q.Get("code")
	errStr := q.Get("error")
	errDesc := q.Get("error_description")
	if code == "" && errStr == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing both code and error query parameters")
	}

	err := mcp.DeliverPendingOAuthCallback(state, mcp.PendingOAuthCallback{
		Code:    code,
		Error:   errStr,
		ErrDesc: errDesc,
	})
	if errors.Is(err, mcp.ErrPendingOAuthNoWaiter) {
		return echo.NewHTTPError(http.StatusNotFound, "no pending OAuth flow for the given state")
	}
	if err != nil {
		slog.WarnContext(c.Request().Context(), "Failed to deliver pending oauth callback", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to deliver pending oauth callback: %v", err))
	}
	return c.JSON(http.StatusOK, nil)
}

func (s *Server) steerSession(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.SteerSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if len(req.Messages) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "at least one message is required")
	}

	if err := s.sm.SteerSession(c.Request().Context(), sessionID, req.Messages); err != nil {
		if strings.Contains(err.Error(), "queue full") {
			c.Response().Header().Set("Retry-After", "1")
			return echo.NewHTTPError(http.StatusTooManyRequests, "steer queue full")
		}
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("failed to steer session: %v", err))
	}

	return c.JSON(http.StatusAccepted, map[string]string{"status": "queued"})
}

// defaultEventsHeartbeatInterval is the default for [Server.heartbeatInterval].
const defaultEventsHeartbeatInterval = 15 * time.Second

// sessionEvents streams events for a session as Server-Sent Events. The
// stream lasts until the client disconnects or the session ends.
//
// Each delivered event carries its monotonic sequence number in the SSE
// "id:" field. A client that reconnects may resume from where it left off by
// supplying the last sequence number it saw, either as the standard
// Last-Event-ID request header (sent automatically by EventSource clients) or
// as a ?since=<seq> query parameter. Buffered events newer than that point are
// replayed before live tailing resumes. If the resume point has already been
// evicted from the buffer, a {"type":"gap"} event is sent first so the client
// knows to re-snapshot (GET /api/sessions/:id/snapshot) before continuing.
//
// End-of-session contract: when the session ends (the agent process exits or
// the session is deleted) a terminal {"type":"session_exited"} event is sent
// and the stream closes. A client that receives session_exited should stop. A
// stream that closes WITHOUT a session_exited event indicates a transport
// drop; the client should reconnect with its last id to replay and continue.
//
// Liveness contract: while connected, the stream emits an SSE comment
// (": ping") every [Server.heartbeatInterval] even when the session is quiet.
// A client that has seen at least one heartbeat can treat a prolonged silence
// as a hung transport and reconnect, instead of waiting forever on a
// connection that will never fail a read (e.g. across a paused VM).
func (s *Server) sessionEvents(c echo.Context) error {
	if !s.sm.HasEventSource(c.Param("id")) {
		return echo.NewHTTPError(http.StatusNotFound, "no event source for session")
	}

	since := parseSinceParam(c)

	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)
	c.Response().Flush()

	// Event writes and heartbeat writes come from different goroutines;
	// interleaved partial frames would corrupt the stream, so serialize them.
	var writeMu sync.Mutex

	heartbeatCtx, stopHeartbeat := context.WithCancel(c.Request().Context())
	heartbeatDone := make(chan struct{})
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
	}()
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				fmt.Fprint(c.Response(), ": ping\n\n")
				c.Response().Flush()
				writeMu.Unlock()
			}
		}
	}()

	s.sm.StreamEvents(c.Request().Context(), c.Param("id"), since, func(seq uint64, event any) {
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		// seq 0 marks a per-connection control event (e.g. gap) that is not
		// part of the sequenced stream, so it carries no id.
		if seq > 0 {
			fmt.Fprintf(c.Response(), "id: %d\n", seq)
		}
		fmt.Fprintf(c.Response(), "data: %s\n\n", data)
		c.Response().Flush()
	})
	return nil
}

// parseSinceParam resolves the resume point for an /events stream from the
// ?since=<seq> query parameter, falling back to the Last-Event-ID header that
// SSE clients replay automatically on reconnect. Returns nil when neither is
// present or parseable, meaning "replay the current buffer, then tail".
func parseSinceParam(c echo.Context) *uint64 {
	raw := c.QueryParam("since")
	if raw == "" {
		raw = c.Request().Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return nil
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &seq
}

func (s *Server) followUpSession(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.SteerSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if len(req.Messages) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "at least one message is required")
	}

	// An optional Idempotency-Key makes the request safe to retry: a repeat
	// with the same key for this session is acknowledged without delivering
	// the follow-up twice.
	idempotencyKey := c.Request().Header.Get("Idempotency-Key")

	streaming, duplicate, err := s.sm.FollowUpSession(c.Request().Context(), sessionID, req.Messages, idempotencyKey)
	if err != nil {
		if strings.Contains(err.Error(), "queue full") {
			c.Response().Header().Set("Retry-After", "1")
			return echo.NewHTTPError(http.StatusTooManyRequests, "follow-up queue full")
		}
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("failed to enqueue follow-up: %v", err))
	}

	status := "queued_streaming"
	switch {
	case duplicate:
		status = "duplicate"
	case !streaming:
		status = "queued_idle"
	}
	return c.JSON(http.StatusAccepted, api.FollowUpResponse{Status: status, Duplicate: duplicate})
}

func (s *Server) addMessage(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.AddMessageRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if req.Message == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "message is required")
	}

	if err := s.sm.AddMessage(c.Request().Context(), sessionID, req.Message); err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to add message: %v", err))
	}

	return c.JSON(http.StatusCreated, map[string]string{"status": "added"})
}

func (s *Server) updateMessage(c echo.Context) error {
	sessionID := c.Param("id")
	msgID := c.Param("msg_id")
	var req api.UpdateMessageRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if req.Message == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "message is required")
	}

	if err := s.sm.UpdateMessage(c.Request().Context(), sessionID, msgID, req.Message); err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to update message: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) addSummary(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.AddSummaryRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if req.Summary == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "summary is required")
	}
	if err := validateSummaryAttribution(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	item := session.Item{
		Summary: req.Summary,
		// Older clients send first_kept_entry under the legacy "tokens" name.
		FirstKeptEntry: cmp.Or(req.FirstKeptEntry, req.Tokens),
		Cost:           req.Cost,
		Model:          req.Model,
		Usage:          req.Usage,
	}
	if err := s.sm.AddSummary(c.Request().Context(), sessionID, item); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to add summary: %v", err))
	}

	return c.JSON(http.StatusCreated, map[string]string{"status": "added"})
}

// maxSummaryModelNameLen bounds the model attribution accepted on a summary
// so a hostile client can't persist arbitrarily large names into the store.
const maxSummaryModelNameLen = 256

// validateSummaryAttribution sanity-checks the cost-attribution metadata of
// an AddSummary request and normalizes an all-zero usage to nil so remote
// summaries look identical to locally generated ones (which only record
// usage when an LLM call actually ran).
func validateSummaryAttribution(req *api.AddSummaryRequest) error {
	if len(req.Model) > maxSummaryModelNameLen {
		return fmt.Errorf("model name exceeds %d characters", maxSummaryModelNameLen)
	}
	for _, r := range req.Model {
		if unicode.IsControl(r) {
			return errors.New("model name must not contain control characters")
		}
	}
	if u := req.Usage; u != nil {
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 {
			return errors.New("usage token counts must not be negative")
		}
		if *u == (chat.Usage{}) {
			req.Usage = nil
		}
	}
	return nil
}

func (s *Server) updateSessionTokens(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.UpdateSessionTokensRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.UpdateSessionTokens(c.Request().Context(), sessionID, req.InputTokens, req.OutputTokens, req.Cost); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to update tokens: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) setSessionStarred(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.SetSessionStarredRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.SetSessionStarred(c.Request().Context(), sessionID, req.Starred); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to set starred: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

// getSessionModels lists the models the user can pick from for the
// session's current agent. Returns 404 if the session has no active runtime
// (it must have been started at least once or be attached out-of-band)
// or 422 if the runtime does not support model switching.
func (s *Server) getSessionModels(c echo.Context) error {
	sessionID := c.Param("id")

	agentName, current, choices, err := s.sm.AvailableSessionModels(c.Request().Context(), sessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrModelSwitchingNotSupported):
			return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, ErrSessionNotRunning):
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}

	return c.JSON(http.StatusOK, runtime.SessionModelsResponse{
		Agent:           agentName,
		CurrentModelRef: current,
		Models:          choices,
	})
}

func (s *Server) batchDeleteSessions(c echo.Context) error {
	var req api.BatchDeleteSessionsRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if len(req.SessionIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "session_ids cannot be empty")
	}

	deleted, failed := s.sm.BatchDeleteSessions(c.Request().Context(), req.SessionIDs)

	return c.JSON(http.StatusOK, api.BatchDeleteSessionsResponse{
		DeletedCount: deleted,
		FailedCount:  len(failed),
		FailedIDs:    failed,
	})
}

func (s *Server) batchExportSessions(c echo.Context) error {
	var req api.BatchExportSessionsRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if len(req.SessionIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "session_ids cannot be empty")
	}

	export, err := s.sm.BatchExportSessions(c.Request().Context(), req.SessionIDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to export sessions: %v", err))
	}

	return c.JSON(http.StatusOK, export)
}

func (s *Server) health(c echo.Context) error {
	return c.JSON(http.StatusOK, api.HealthResponse{
		Status: "ok",
	})
}

func (s *Server) ready(c echo.Context) error {
	// Check if session store is accessible (quick connectivity check)
	ctx, cancel := context.WithTimeout(c.Request().Context(), 100*time.Millisecond)
	defer cancel()

	sessions, err := s.sm.GetSessions(ctx)
	var storeConnected bool
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		// We assume store is connected if we can query it or we hit a timeout
		// (timeout is still better than a hard connection failure)
		storeConnected = true
	}

	activeSessions := 0
	if sessions != nil {
		activeSessions = len(sessions)
	}

	var toolsetHealth string
	var latestError string

	// Determine overall readiness
	ready := storeConnected
	if !ready {
		latestError = "store disconnected"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}

	if !ready {
		toolsetHealth = "unavailable"
	} else {
		toolsetHealth = "ok"
	}

	return c.JSON(status, api.ReadyResponse{
		Ready:          ready,
		ActiveSessions: activeSessions,
		StoreConnected: storeConnected,
		ToolsetHealth:  toolsetHealth,
		LatestError:    latestError,
	})
}

func (s *Server) getSessionRecoveryData(c echo.Context) error {
	sessionID := c.Param("id")
	data, err := s.sm.ExportSessionForRecovery(c.Request().Context(), sessionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to export session: %v", err))
	}

	return c.JSON(http.StatusOK, data)
}

func (s *Server) getSessionQueueStatus(c echo.Context) error {
	sessionID := c.Param("id")

	// Get the session runtime to check queue status
	sessionRuntime, ok := s.sm.runtimeSessions.Load(sessionID)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "session not found or not running")
	}

	queueStatus := sessionRuntime.runtime.QueueStatus()

	resp := api.QueueDepthResponse{}
	resp.Steer.Depth = queueStatus.SteerDepth
	resp.Steer.Capacity = queueStatus.SteerCapacity
	resp.Followup.Depth = queueStatus.FollowupDepth
	resp.Followup.Capacity = queueStatus.FollowupCapacity

	return c.JSON(http.StatusOK, resp)
}

// BearerTokenMiddleware validates bearer token authentication
func BearerTokenMiddleware(expectedToken string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Skip authentication for health and readiness endpoints
			if c.Path() == "/health" || c.Path() == "/ready" {
				return next(c)
			}

			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing Authorization header")
			}

			// Extract Bearer token
			const prefix = "Bearer "
			if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid Authorization header format")
			}

			token := auth[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
			}

			return next(c)
		}
	}
}
