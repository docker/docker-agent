// Package chatserver implements an OpenAI-compatible HTTP server that exposes
// docker-agent agents through the /v1/chat/completions and /v1/models
// endpoints.
//
// The goal is to let any tool that already speaks OpenAI's chat protocol
// (e.g. Open WebUI, custom shell scripts using the openai SDK) drive a
// docker-agent agent without needing to know about docker-agent's own
// protocol.
//
// On types: we deliberately don't reuse the request/response structs from
// github.com/openai/openai-go/v3. The SDK is built around its internal
// `apijson` encoder; with stdlib `encoding/json` those types serialize
// every field and produce noisy responses. `apijson` lives under
// `internal/`, so we can't borrow it. `openai.Model` is the one type that
// round-trips cleanly with stdlib json, so we reuse it for /v1/models.
package chatserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/echolog"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
)

// Options configures the chat completions server. Future improvements
// (auth, conversations, etc.) extend this struct rather than the Run
// signature so callers stay stable.
type Options struct {
	// AgentName pins the single agent to expose. Empty exposes every
	// agent in the team and uses the team's default as the fallback.
	AgentName string
	// RunConfig is the runtime configuration used to load the team.
	RunConfig *config.RuntimeConfig
	// CORSOrigin is the allowed value for the Access-Control-Allow-Origin
	// header. When empty, the CORS middleware is not registered at all
	// (the server never emits any Access-Control-* response header).
	//
	// Multiple values can be provided separated by commas. Each entry is
	// either a literal origin (matched exactly), the wildcard "*", or a
	// pattern starting with "~" interpreted as a Go regular expression
	// against the request's Origin header. Examples:
	//
	//	"https://app.example.com"
	//	"https://app.example.com,https://staging.example.com"
	//	"~^https://[a-z0-9-]+\\.example\\.com$"
	CORSOrigin string
	// APIKey, if non-empty, is the static bearer token clients must
	// present in the `Authorization` header (`Authorization: Bearer X`).
	// Empty disables authentication; once set, every request to /v1/* is
	// rejected with 401 unless it carries the matching token.
	// /v1/models is also protected so an unauthenticated client can't
	// fingerprint the server.
	APIKey string
	// MaxRequestBytes caps the size of an incoming request body. Zero
	// means use the package default (1 MiB).
	MaxRequestBytes int64
	// RequestTimeout caps how long a single chat completion is allowed to
	// run. Zero means use the package default (5 minutes). The cap covers
	// model calls, tool calls, and SSE streaming combined.
	RequestTimeout time.Duration
	// ConversationsMaxSessions, when > 0, enables the X-Conversation-Id
	// header: clients can pass a stable id to reuse the same session
	// across requests instead of re-sending the full message history
	// every turn. This is the size of the in-memory LRU cache.
	ConversationsMaxSessions int
	// ConversationTTL is how long a cached conversation may be idle
	// before it's evicted. Zero means use the package default
	// (30 minutes).
	ConversationTTL time.Duration
	// MaxIdleRuntimes bounds the number of idle runtimes pooled per
	// agent. Building a runtime resolves tools and sets up channels;
	// keeping a small pool of warm runtimes avoids paying that cost on
	// every request. Zero disables pooling (a fresh runtime is built
	// for every request, the original behaviour).
	MaxIdleRuntimes int
}

const (
	defaultMaxRequestBytes int64         = 1 << 20 // 1 MiB
	defaultRequestTimeout  time.Duration = 5 * time.Minute
	defaultConversationTTL time.Duration = 30 * time.Minute
)

// Run starts an OpenAI-compatible HTTP server on the given listener and
// blocks until ctx is cancelled or the server fails. The team is loaded
// once from agentFilename and shared across requests; every chat completion
// request gets a fresh session.
func Run(ctx context.Context, agentFilename string, opts Options, ln net.Listener) error {
	slog.DebugContext(ctx, "Starting chat completions server", "agent", agentFilename, "addr", ln.Addr())

	t, err := loadTeam(ctx, agentFilename, opts.RunConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := t.StopToolSets(ctx); err != nil {
			slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
		}
	}()

	policy, err := newAgentPolicy(t, opts.AgentName)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: newRouter(&server{
			team:              t,
			policy:            policy,
			conversations:     newConversationStore(opts.ConversationsMaxSessions, conversationTTL(opts)),
			conversationLocks: newConversationLockSet(),
			runtimes:          newRuntimePool(t, opts.MaxIdleRuntimes),
		}, opts),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return serve(ctx, httpServer, ln)
}

func conversationTTL(opts Options) time.Duration {
	if opts.ConversationTTL > 0 {
		return opts.ConversationTTL
	}
	return defaultConversationTTL
}

// loadTeam resolves and loads the team referenced by agentFilename.
func loadTeam(ctx context.Context, agentFilename string, runConfig *config.RuntimeConfig) (*team.Team, error) {
	src, err := config.Resolve(agentFilename, nil)
	if err != nil {
		return nil, err
	}
	t, err := teamloader.Load(ctx, src, runConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load agents: %w", err)
	}
	return t, nil
}

// serve runs httpServer on ln until ctx is cancelled, then triggers a
// graceful shutdown.
func serve(ctx context.Context, httpServer *http.Server, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// server is concurrent-safe: every request creates its own session and
// runtime, so the only shared state is the team (whose toolsets are
// independently safe to call) and the optional conversation cache.
type server struct {
	team              *team.Team
	policy            agentPolicy
	conversations     *conversationStore
	conversationLocks *conversationLockSet
	runtimes          *runtimePool
}

func newRouter(s *server, opts Options) http.Handler {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	maxBytes := opts.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxRequestBytes
	}
	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	e.Use(echolog.RedactedRequestLogger())
	e.Use(middleware.BodyLimit(strconv.FormatInt(maxBytes, 10)))
	e.Use(requestTimeoutMiddleware(timeout))

	// Register /openapi.json *before* the bearer-auth middleware so the
	// schema is reachable for introspection without credentials. CORS
	// configuration is then layered for /v1/* routes.
	e.GET("/openapi.json", s.handleOpenAPI)

	if opts.APIKey != "" {
		e.Use(bearerAuthMiddleware(opts.APIKey))
	}
	if opts.CORSOrigin != "" {
		cfg, err := corsMiddlewareConfig(opts.CORSOrigin)
		if err != nil {
			// Bad config is reported via the request log. The middleware
			// is simply not registered, which is the safest default.
			slog.Error("Invalid --cors-origin, CORS disabled", "error", err)
		} else {
			e.Use(middleware.CORSWithConfig(cfg))
		}
	}

	e.GET("/v1/models", s.handleModels)
	e.POST("/v1/chat/completions", s.handleChatCompletions)
	return e
}

// requestTimeoutMiddleware caps each request's lifetime. Streaming
// handlers honour the timeout via c.Request().Context().
func requestTimeoutMiddleware(d time.Duration) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ctx, cancel := context.WithTimeout(c.Request().Context(), d)
			defer cancel()
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	}
}

// corsMiddlewareConfig parses a comma-separated --cors-origin value into an
// echo middleware.CORSConfig. Each entry is one of:
//
//   - the literal "*" wildcard;
//   - a regex when prefixed with "~" (compiled and matched against the
//     request's Origin header);
//   - a literal origin matched verbatim.
//
// Returns an error when no entry parses successfully, in which case the
// caller leaves the middleware unregistered.
func corsMiddlewareConfig(spec string) (middleware.CORSConfig, error) {
	var literals []string
	var patterns []*regexp.Regexp
	for raw := range strings.SplitSeq(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(entry, "~"); ok {
			re, err := regexp.Compile(rest)
			if err != nil {
				return middleware.CORSConfig{}, fmt.Errorf("invalid CORS regex %q: %w", rest, err)
			}
			patterns = append(patterns, re)
			continue
		}
		if err := validateCORSOrigin(entry); err != nil {
			return middleware.CORSConfig{}, err
		}
		literals = append(literals, entry)
	}
	if len(literals) == 0 && len(patterns) == 0 {
		return middleware.CORSConfig{}, errors.New("no usable CORS origins")
	}

	cfg := middleware.CORSConfig{
		AllowOrigins: literals,
		AllowMethods: []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowHeaders: []string{"Authorization", "Content-Type", "Accept"},
		MaxAge:       86400,
	}
	if len(patterns) > 0 {
		cfg.AllowOriginFunc = func(origin string) (bool, error) {
			for _, re := range patterns {
				if re.MatchString(origin) {
					return true, nil
				}
			}
			return false, nil
		}
	}
	return cfg, nil
}

// validateCORSOrigin sanity-checks a literal origin entry. The aim is to
// reject obvious typos early ("http//foo.com", "https://foo.com/bar")
// rather than to be a full URL parser — the echo middleware will still
// do its own matching at request time.
func validateCORSOrigin(o string) error {
	if o == "*" {
		return nil
	}
	u, err := url.Parse(o)
	if err != nil {
		return fmt.Errorf("invalid CORS origin %q: %w", o, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid CORS origin %q: scheme must be http or https", o)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid CORS origin %q: missing host", o)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid CORS origin %q: must not include path, query, or fragment", o)
	}
	return nil
}

// bearerAuthMiddleware enforces the static `Authorization: Bearer <token>`
// header. CORS preflight requests (OPTIONS) are exempted so that browsers
// can negotiate before sending the auth header.
//
// The expected token is captured by closure rather than read per-request,
// and the comparison uses subtle.ConstantTimeCompare so timing observation
// can't reveal valid prefixes.
func bearerAuthMiddleware(expected string) echo.MiddlewareFunc {
	exp := []byte(expected)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodOptions {
				return next(c)
			}
			// Schema introspection is always reachable so tooling can
			// discover the API without credentials.
			if c.Path() == "/openapi.json" {
				return next(c)
			}
			got, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), exp) != 1 {
				return writeError(c, http.StatusUnauthorized, "missing or invalid bearer token")
			}
			return next(c)
		}
	}
}

func (s *server) handleModels(c echo.Context) error {
	data := make([]openai.Model, 0, len(s.policy.exposed))
	for _, name := range s.policy.exposed {
		data = append(data, openai.Model{ID: name, OwnedBy: "docker-agent"})
	}
	return c.JSON(http.StatusOK, ModelsResponse{Object: "list", Data: data})
}

func (s *server) handleChatCompletions(c echo.Context) error {
	var req ChatCompletionRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return writeError(c, http.StatusBadRequest, err.Error())
	}
	if len(req.Messages) == 0 {
		return writeError(c, http.StatusBadRequest, "at least one message is required")
	}
	if err := validateSamplingParams(&req); err != nil {
		return writeError(c, http.StatusBadRequest, err.Error())
	}

	conversationID := c.Request().Header.Get("X-Conversation-Id")
	if !s.conversationLocks.tryAcquire(conversationID) {
		return writeError(c, http.StatusConflict, "another request is already in flight for this conversation id")
	}
	defer s.conversationLocks.release(conversationID)

	sess := s.resolveSession(conversationID, req.Messages)
	if sess == nil {
		return writeError(c, http.StatusBadRequest, "no user message provided")
	}

	agentName := s.policy.pick(req.Model)
	rt, err := s.runtimes.Get(agentName)
	if err != nil {
		return writeError(c, http.StatusInternalServerError, fmt.Sprintf("failed to acquire runtime: %v", err))
	}
	defer s.runtimes.Put(agentName, rt)

	// Echo back the requested model verbatim when set, so clients matching
	// on the model field stay happy. Otherwise expose the actual agent.
	model := agentName
	if req.Model != "" {
		model = req.Model
	}

	if req.Stream {
		err := s.streamChatCompletion(c, rt, sess, model, req.StreamOptions.IncludeUsage)
		s.maybeStoreConversation(conversationID, sess)
		return err
	}
	err = s.chatCompletion(c, rt, sess, model)
	s.maybeStoreConversation(conversationID, sess)
	return err
}

// resolveSession decides whether to start fresh or continue an existing
// conversation. When X-Conversation-Id is set and we have an existing
// session for it, we append only the latest user message from the
// request (the prior history is already in the session). Otherwise we
// build a brand-new session from the full request history.
func (s *server) resolveSession(id string, msgs []ChatCompletionMessage) *session.Session {
	if id != "" {
		if existing := s.conversations.Get(id); existing != nil {
			appendLatestUser(existing, msgs)
			return existing
		}
	}
	return buildSession(msgs)
}

// maybeStoreConversation inserts the session into the cache after a
// run. We always insert to handle the case where the conversation was
// evicted while the request was in flight.
func (s *server) maybeStoreConversation(id string, sess *session.Session) {
	if id == "" || s.conversations == nil {
		return
	}
	// Always Put, even for existing conversations, to handle eviction
	// during request processing. Put refreshes the lastUsed timestamp
	// and ensures the updated session is stored.
	s.conversations.Put(id, sess)
}

// chatCompletion runs the agent to completion and replies with one
// non-streaming OpenAI ChatCompletion object.
func (s *server) chatCompletion(c echo.Context, rt runtime.Runtime, sess *session.Session, model string) error {
	var toolCalls []ToolCallReference
	emit := agentEmit{
		onToolCall: func(tc ToolCallReference) {
			toolCalls = append(toolCalls, tc)
		},
	}
	if err := runAgentLoop(c.Request().Context(), rt, sess, emit); err != nil {
		return writeError(c, http.StatusInternalServerError, fmt.Sprintf("agent execution failed: %v", err))
	}

	return c.JSON(http.StatusOK, ChatCompletionResponse{
		ID:      newChatID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{{
			Index: 0,
			Message: ChatCompletionMessage{
				Role:      "assistant",
				Content:   sess.GetLastAssistantMessageContent(),
				ToolCalls: toolCalls,
			},
			FinishReason: "stop",
		}},
		Usage: sessionUsage(sess),
	})
}

// streamChatCompletion runs the agent and streams its response back to the
// client as Server-Sent Events in OpenAI's chat.completion.chunk format.
//
// The error return is reserved for future use (e.g. surfacing a write
// failure to the request logger). Today every error is converted into an
// in-band SSE error event, so the function always returns nil.
func (s *server) streamChatCompletion(c echo.Context, rt runtime.Runtime, sess *session.Session, model string, includeUsage bool) error { //nolint:unparam // see comment
	stream := newSSEStream(c.Response(), newChatID(), model)

	// Initial "role: assistant" delta so clients can start rendering.
	stream.send(ChatCompletionStreamDelta{Role: "assistant"}, "")

	emit := agentEmit{
		onContent: func(content string) {
			if content != "" {
				stream.send(ChatCompletionStreamDelta{Content: content}, "")
			}
		},
		onToolCall: func(tc ToolCallReference) {
			// Surface tool calls to the client using OpenAI's exact wire
			// shape: a single delta carrying the full tool_call entry.
			// (OpenAI streams arguments token-by-token; we have them all
			// at once, so one chunk per call is enough.) Tools still run
			// server-side — this is purely for client visibility.
			stream.send(ChatCompletionStreamDelta{ToolCalls: []ToolCallReference{tc}}, "")
		},
	}
	runErr := runAgentLoop(c.Request().Context(), rt, sess, emit)
	if runErr != nil {
		// Emit a structured error envelope (OpenAI streams use a regular
		// `data:` line carrying an `error` object, then close the stream
		// with finish_reason "error" instead of "stop"). Clients matching
		// on the OpenAI protocol can therefore distinguish a model error
		// from a normal completion.
		stream.sendError(runErr)
		stream.send(ChatCompletionStreamDelta{}, "error")
		if includeUsage {
			stream.sendUsage(sessionUsage(sess))
		}
	} else {
		stream.send(ChatCompletionStreamDelta{}, "stop")
		if includeUsage {
			stream.sendUsage(sessionUsage(sess))
		}
	}
	stream.done()
	return nil
}

// sseStream writes OpenAI-style chat.completion.chunk events to a response.
// It centralises SSE bookkeeping (headers, JSON encoding, flushing,
// terminator) so the handler can focus on what to emit.
type sseStream struct {
	w       http.ResponseWriter
	id      string
	model   string
	created int64
}

func newSSEStream(w http.ResponseWriter, id, model string) *sseStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	return &sseStream{w: w, id: id, model: model, created: time.Now().Unix()}
}

func (s *sseStream) sendUsage(usage *ChatCompletionUsage) {
	if usage == nil {
		return
	}
	chunk := ChatCompletionStreamResponse{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChatCompletionStreamChoice{},
		Usage:   usage,
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *sseStream) send(delta ChatCompletionStreamDelta, finishReason string) {
	chunk := ChatCompletionStreamResponse{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChatCompletionStreamChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// done writes the OpenAI sentinel terminator that ends the stream.
func (s *sseStream) done() {
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// sendError emits an OpenAI-style error envelope as a separate SSE event
// alongside the chunked deltas. Real OpenAI streams use this shape when a
// run fails mid-flight, e.g. a content filter trips: the message arrives
// in its own `data:` line carrying an `error` object before the stream
// terminates.
func (s *sseStream) sendError(err error) {
	envelope := ErrorResponse{Error: ErrorDetail{
		Message: err.Error(),
		Type:    "internal_error",
	}}
	data, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// newChatID returns a fresh OpenAI-style chat completion id.
func newChatID() string { return "chatcmpl-" + uuid.NewString() }

// writeError writes an OpenAI-style error envelope.
func writeError(c echo.Context, status int, message string) error {
	return c.JSON(status, ErrorResponse{Error: ErrorDetail{
		Message: message,
		Type:    errTypeFor(status),
	}})
}

func errTypeFor(status int) string {
	if status >= 500 {
		return "internal_error"
	}
	return "invalid_request_error"
}

// validateSamplingParams range-checks the OpenAI sampling fields. Even when
// we don't yet plumb them all the way through to the model, validating up
// front lets clients learn about typos / out-of-range values immediately
// instead of getting an opaque provider error several seconds later.
func validateSamplingParams(req *ChatCompletionRequest) error {
	if req.Temperature != nil {
		t := *req.Temperature
		if math.IsNaN(t) || t < 0 || t > 2 {
			return fmt.Errorf("temperature must be in [0, 2], got %g", t)
		}
	}
	if req.TopP != nil {
		p := *req.TopP
		if math.IsNaN(p) || p <= 0 || p > 1 {
			return fmt.Errorf("top_p must be in (0, 1], got %g", p)
		}
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens must be > 0, got %d", *req.MaxTokens)
	}
	if slices.Contains(req.Stop, "") {
		return errors.New("stop sequences must not be empty strings")
	}
	return nil
}
