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
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/echolog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/upstream"
)

type Server struct {
	e         *echo.Echo
	sm        *SessionManager
	authToken string
}

func New(ctx context.Context, sessionStore session.Store, runConfig *config.RuntimeConfig, refreshInterval time.Duration, agentSources config.Sources, authToken string) (*Server, error) {
	return NewWithManager(NewSessionManager(ctx, agentSources, sessionStore, refreshInterval, runConfig), authToken), nil
}

// NewWithManager builds a Server around an already-constructed SessionManager.
// Useful when the runtime is owned by another component (e.g. the TUI) and
// only needs to be exposed over HTTP.
func NewWithManager(sm *SessionManager, authToken string) *Server {
	e := echo.New()
	e.Use(echolog.RedactedRequestLogger())
	e.Use(echo.WrapMiddleware(upstream.Handler))

	// Add bearer token middleware if token is configured
	if authToken != "" {
		e.Use(BearerTokenMiddleware(authToken))
	}

	s := &Server{e: e, sm: sm, authToken: authToken}
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
	group.POST("/sessions/:id/resume", s.resumeSession)
	group.POST("/sessions/:id/tools/toggle", s.toggleSessionYolo)
	group.PATCH("/sessions/:id/permissions", s.updateSessionPermissions)
	group.PATCH("/sessions/:id/title", s.updateSessionTitle)
	group.PATCH("/sessions/:id/tokens", s.updateSessionTokens)
	group.PATCH("/sessions/:id/starred", s.setSessionStarred)
	group.POST("/sessions", s.createSession)
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

	group.GET("/ping", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	group.GET("/ready", s.sessionsReady)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := http.Server{
		Handler:           s.e,
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

		c, err := config.Load(c.Request().Context(), agentSource)
		if err != nil {
			slog.Error("Failed to load config from API source", "key", k, "error", err)
			continue
		}

		desc := c.Agents.First().Description

		switch {
		case len(c.Agents) > 1:
			agents = append(agents, api.Agent{
				Name:        k,
				Multi:       true,
				Description: desc,
			})
		case len(c.Agents) == 1:
			agents = append(agents, api.Agent{
				Name:        k,
				Multi:       false,
				Description: desc,
			})
		default:
			slog.Warn("No agents found in config from API source", "key", k)
			continue
		}
	}

	slices.SortFunc(agents, func(a, b api.Agent) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return c.JSON(http.StatusOK, agents)
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
		responses[i] = api.SessionsResponse{
			ID:           sess.ID,
			Title:        sess.Title,
			CreatedAt:    sess.CreatedAt.Format(time.RFC3339),
			NumMessages:  len(sess.GetAllMessages()),
			InputTokens:  sess.InputTokens,
			OutputTokens: sess.OutputTokens,
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
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to create session: %v", err))
	}

	return c.JSON(http.StatusOK, sess)
}

func (s *Server) getSession(c echo.Context) error {
	sess, err := s.sm.GetSession(c.Request().Context(), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("session not found: %v", err))
	}

	return c.JSON(http.StatusOK, api.SessionResponse{
		ID:            sess.ID,
		Title:         sess.Title,
		CreatedAt:     sess.CreatedAt,
		Messages:      sess.GetAllMessages(),
		ToolsApproved: sess.ToolsApproved,
		InputTokens:   sess.InputTokens,
		OutputTokens:  sess.OutputTokens,
		WorkingDir:    sess.WorkingDir,
		Permissions:   sess.Permissions,
	})
}

func (s *Server) getSessionStatus(c echo.Context) error {
	status, err := s.sm.GetSessionStatus(c.Request().Context(), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("session not found: %v", err))
	}
	return c.JSON(http.StatusOK, status)
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

	var messages []api.Message
	if err := json.NewDecoder(c.Request().Body).Decode(&messages); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	streamChan, err := s.sm.RunSession(c.Request().Context(), sessionID, agentFilename, currentAgent, messages)
	if err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
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

	if err := s.sm.ResumeElicitation(c.Request().Context(), sessionID, req.Action, req.Content); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to resume elicitation: %v", err))
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

// sessionEvents streams events for a session as Server-Sent Events. The
// stream lasts until the client disconnects or the session ends.
func (s *Server) sessionEvents(c echo.Context) error {
	if _, ok := s.sm.GetEventSource(c.Param("id")); !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no event source for session")
	}

	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)
	c.Response().Flush()

	s.sm.StreamEvents(c.Request().Context(), c.Param("id"), func(event any) {
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		fmt.Fprintf(c.Response(), "data: %s\n\n", data)
		c.Response().Flush()
	})
	return nil
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

	streaming, err := s.sm.FollowUpSession(c.Request().Context(), sessionID, req.Messages)
	if err != nil {
		if strings.Contains(err.Error(), "queue full") {
			c.Response().Header().Set("Retry-After", "1")
			return echo.NewHTTPError(http.StatusTooManyRequests, "follow-up queue full")
		}
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("failed to enqueue follow-up: %v", err))
	}

	status := "queued_streaming"
	if !streaming {
		status = "queued_idle"
	}
	return c.JSON(http.StatusAccepted, map[string]string{"status": status})
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

	if err := s.sm.AddSummary(c.Request().Context(), sessionID, req.Summary, req.Tokens); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to add summary: %v", err))
	}

	return c.JSON(http.StatusCreated, map[string]string{"status": "added"})
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
