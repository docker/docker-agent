// Package modelerrors provides error classification utilities for LLM model
// providers. It determines whether errors are retryable, identifies context
// window overflow conditions, extracts HTTP status codes from various SDK
// error types, and computes exponential backoff durations.
package modelerrors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// StatusError wraps an HTTP API error with structured metadata for retry decisions.
// Providers wrap SDK errors in this type so the retry loop can use errors.As
// to extract status code and Retry-After without importing provider-specific SDKs.
type StatusError struct {
	// StatusCode is the HTTP status code from the provider's API response.
	StatusCode int
	// RetryAfter is the parsed Retry-After header duration. Zero if absent.
	RetryAfter time.Duration
	// Err is the original error from the provider SDK.
	Err error
}

func (e *StatusError) Error() string {
	underlying := e.Err.Error()
	// Lift structured details out of the SDK error envelope (URL + status +
	// JSON body) when possible, so the user sees what the provider actually
	// said instead of a generic "400 Bad Request".
	if details := parseProviderError(underlying); details != "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, details)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, underlying)
}

func (e *StatusError) Unwrap() error {
	return e.Err
}

// WrapHTTPError wraps err in a *StatusError carrying the HTTP status code and
// parsed Retry-After header from resp. Returns err unchanged if statusCode < 400
// or err is nil. Pass resp=nil when no *http.Response is available.
func WrapHTTPError(statusCode int, resp *http.Response, err error) error {
	if err == nil || statusCode < 400 {
		return err
	}
	var retryAfter time.Duration
	if resp != nil {
		retryAfter = parseRetryAfterHeader(resp.Header.Get("Retry-After"))
	}
	return &StatusError{
		StatusCode: statusCode,
		RetryAfter: retryAfter,
		Err:        err,
	}
}

// Default fallback configuration.
const (
	// DefaultRetries is the default number of retries per model with exponential
	// backoff for retryable errors (5xx, timeouts). 2 retries means 3 total attempts.
	// This handles transient provider issues without immediately failing over.
	DefaultRetries = 2

	// DefaultCooldown is the default duration to stick with a fallback model
	// after a non-retryable error before retrying the primary.
	DefaultCooldown = 1 * time.Minute
)

// OverflowKind classifies the cause of a context overflow, so the runtime
// and UI can react differently to each shape.
//
//   - [OverflowKindTokens]: the accumulated conversation exceeds the model's
//     context window. Token-count rejection. Compaction can usually help.
//   - [OverflowKindWire]: the request body exceeds the provider's wire-level
//     limit (e.g. Anthropic's 32 MB cap, gateway 413s). Compaction usually
//     CANNOT help because the offending single turn still has to be sent.
//   - [OverflowKindMedia]: an image, PDF, or similar attachment in the
//     conversation exceeds the provider's media constraints (size, page
//     count, dimensions).
type OverflowKind string

const (
	OverflowKindTokens OverflowKind = "tokens"
	OverflowKindWire   OverflowKind = "wire"
	OverflowKindMedia  OverflowKind = "media"
)

// ContextOverflowError wraps an underlying error to indicate that the failure
// was caused by the conversation context exceeding some provider-side limit.
// This is used to trigger auto-compaction in the runtime loop instead of
// surfacing raw HTTP errors to the user.
//
// Kind classifies the specific shape of the overflow ([OverflowKindTokens] by
// default for backwards compatibility). Use [NewContextOverflowError] to have
// it set automatically by classification, or build the struct directly to
// force a Kind.
type ContextOverflowError struct {
	Underlying error
	Kind       OverflowKind
}

// NewContextOverflowError creates a ContextOverflowError wrapping the given
// underlying error. The Kind is inferred from the underlying error via
// [classifyOverflow]; if classification yields no result, Kind defaults to
// [OverflowKindTokens] (the historical behaviour). Use this constructor
// rather than building the struct directly so future field additions don't
// break callers.
func NewContextOverflowError(underlying error) *ContextOverflowError {
	kind := classifyOverflow(underlying)
	if kind == "" {
		kind = OverflowKindTokens
	}
	return &ContextOverflowError{Underlying: underlying, Kind: kind}
}

func (e *ContextOverflowError) Error() string {
	if e.Underlying == nil {
		return "context window overflow"
	}
	return "context window overflow: " + e.Underlying.Error()
}

func (e *ContextOverflowError) Unwrap() error {
	return e.Underlying
}

// tokenOverflowPatterns matches token-count rejections from various providers.
// Best-effort substring match (case-insensitive) against the error message.
// Provider error wording is not contractual and drifts over time; this list
// is heuristics derived from observed errors. Adding a provider only requires
// appending a phrase.
var tokenOverflowPatterns = []string{
	"prompt is too long",                // Anthropic, Vertex (with Anthropic body)
	"prompt too long",                   // Ollama ("prompt too long; exceeded ...")
	"maximum context length",            // OpenAI, OpenRouter, DeepSeek, vLLM
	"context length exceeded",           // OpenAI legacy
	"context_length_exceeded",           // OpenAI structured code
	"input is too long",                 // Bedrock
	"input token count",                 // Gemini ("...exceeds the maximum")
	"exceeds the context window",        // OpenAI Responses API
	"reduce the length of the messages", // Groq
	"exceeded model token limit",        // Kimi, Moonshot
	"context window exceeds limit",      // MiniMax
	"model_context_window_exceeded",     // z.ai
	"max_tokens must be greater than",   // Anthropic edge case: thinking-budget cascade
	"maximum number of tokens",
	"content length exceeds",
	"exceeds the model's max token",
	"token limit",
	"reduce your prompt",
}

// wireOverflowPatterns matches wire-level rejections — the whole request body
// is too big to send regardless of context window. These trigger different
// recovery than token overflows (compaction-as-retry won't help when the
// latest turn alone is over the wire cap).
var wireOverflowPatterns = []string{
	"request_too_large",        // Anthropic structured error.type
	"request too large",        // Anthropic prose
	"payload too large",        // HTTP 413 status text
	"request entity too large", // RFC 7231 status text
}

// mediaOverflowPatterns matches media-specific rejections (image too big, PDF
// too many pages, etc.). Distinguished from token/wire because recovery
// strategies differ — stripping media from history can help here.
var mediaOverflowPatterns = []string{
	"image exceeds",           // Anthropic
	"image dimensions exceed", // Anthropic many-image
	"pdf pages",               // "maximum of N PDF pages" — Anthropic
}

// IsContextOverflowError reports whether err indicates the conversation
// exceeded a provider-side limit (token window, wire size, or media size).
// Use [OverflowKindOf] to distinguish the three shapes.
func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*ContextOverflowError](err); ok {
		return true
	}
	return classifyOverflow(err) != ""
}

// OverflowKindOf returns the [OverflowKind] of err, or "" if it isn't an
// overflow error. If err is already wrapped in a [*ContextOverflowError]
// with a non-empty Kind, that Kind is returned; otherwise classification
// runs on the unwrapped error.
func OverflowKindOf(err error) OverflowKind {
	if err == nil {
		return ""
	}
	if coe, ok := errors.AsType[*ContextOverflowError](err); ok {
		if coe.Kind != "" {
			return coe.Kind
		}
		// Legacy wrap with no Kind — try classifying the underlying.
		if coe.Underlying != nil {
			if k := classifyOverflow(coe.Underlying); k != "" {
				return k
			}
		}
		return OverflowKindTokens
	}
	return classifyOverflow(err)
}

// classifyOverflow inspects err for overflow signals and returns the matching
// [OverflowKind], or "" if err is not an overflow error.
//
// The classifier runs two tiers, in order:
//
//	Tier 1 — structured signals (high confidence):
//	  * body.error.type == "request_too_large"     → OverflowKindWire
//	  * body.error.code == "context_length_exceeded" → OverflowKindTokens
//	  * HTTP status 413                            → OverflowKindWire
//
//	Tier 2 — substring patterns (best-effort fallback):
//	  * mediaOverflowPatterns → OverflowKindMedia
//	  * wireOverflowPatterns  → OverflowKindWire
//	  * tokenOverflowPatterns → OverflowKindTokens
//
// Tier 1 wins when both fire. Within Tier 2, media is checked first because
// it is the most specific; wire before tokens because some wire signals
// ("request too large") textually overlap with token-overflow phrasing in a
// way that benefits from the wire match coming first.
func classifyOverflow(err error) OverflowKind {
	if err == nil {
		return ""
	}
	// Already-wrapped errors carry their Kind; respect it.
	if coe, ok := errors.AsType[*ContextOverflowError](err); ok && coe.Kind != "" {
		return coe.Kind
	}

	raw := err.Error()

	// Tier 1: structured body fields.
	if body := firstJSONObject(raw); body != nil {
		var parsed providerErrorBody
		if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil {
			if parsed.Error.Type == "request_too_large" {
				return OverflowKindWire
			}
			if code := scalarString(parsed.Error.Code); code == "context_length_exceeded" {
				return OverflowKindTokens
			}
		}
	}

	// Tier 1: HTTP status code (413 → wire).
	if se, ok := errors.AsType[*StatusError](err); ok && se.StatusCode == http.StatusRequestEntityTooLarge {
		return OverflowKindWire
	}

	// Tier 2: substring fallback. Media first (most specific), then wire,
	// then tokens.
	msg := strings.ToLower(raw)
	for _, p := range mediaOverflowPatterns {
		if strings.Contains(msg, p) {
			return OverflowKindMedia
		}
	}
	for _, p := range wireOverflowPatterns {
		if strings.Contains(msg, p) {
			return OverflowKindWire
		}
	}
	for _, p := range tokenOverflowPatterns {
		if strings.Contains(msg, p) {
			return OverflowKindTokens
		}
	}
	return ""
}

// statusCodeRegex matches HTTP status codes in error messages (e.g., "429", "500", ": 429 ")
var statusCodeRegex = regexp.MustCompile(`\b([45]\d{2})\b`)

// extractHTTPStatusCode attempts to extract an HTTP status code from the error
// using regex parsing of the error message. This is a fallback for providers
// whose errors are not yet wrapped in *StatusError (the preferred path).
//
// The regex matches 4xx/5xx codes at word boundaries
// (e.g., "429 Too Many Requests", "500 Internal Server Error").
// Returns 0 if no status code found.
func extractHTTPStatusCode(err error) int {
	if err == nil {
		return 0
	}

	// Check for *StatusError first (preferred structured path).
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}

	// Fallback: extract from error message using regex.
	// OpenAI SDK error format: `POST "/v1/...": 429 Too Many Requests {...}`
	matches := statusCodeRegex.FindStringSubmatch(err.Error())
	if len(matches) >= 2 {
		if code, err := strconv.Atoi(matches[1]); err == nil {
			return code
		}
	}

	return 0
}

// isRetryableStatusCode determines if an HTTP status code is retryable.
// Retryable means we should retry the SAME model with exponential backoff.
//
// Retryable status codes:
// - 5xx (server errors): 500, 502, 503, 504
// - 529 (Anthropic overloaded)
// - 408 (request timeout)
//
// Non-retryable status codes (skip to next model immediately):
// - 429 (rate limit) - provider is explicitly telling us to back off
// - 4xx client errors (400, 401, 403, 404) - won't get better with retry
func isRetryableStatusCode(statusCode int) bool {
	switch statusCode {
	case 500, 502, 503, 504: // Server errors
		return true
	case 529: // Anthropic overloaded
		return true
	case 408: // Request timeout
		return true
	case 429: // Rate limit - NOT retryable, skip to next model
		return false
	default:
		return false
	}
}

// transientStatusCodePatterns contains error message substrings that indicate
// a transient failure even when the HTTP status code would otherwise classify
// the error as non-retryable (e.g. 4xx). Patterns MUST be lowercase: they are
// compared against a lowercased error message (see [matchesTransientPattern]).
//
// Currently:
//   - Vertex AI / Gemini intermittently returns 400 INVALID_ARGUMENT with the
//     message "Please ensure that the number of function response parts is
//     equal to the number of function call parts of the function call turn."
//     even for well-formed requests; the same payload succeeds on retry.
//     Without this override the run would surface a fatal error to the user
//     and exit non-zero, see https://github.com/docker/docker-agent/issues/2683.
var transientStatusCodePatterns = []string{
	"number of function response parts",
}

// matchesTransientPattern reports whether the error's message matches one of
// [transientStatusCodePatterns]. Used to override an otherwise non-retryable
// HTTP status classification. Patterns are pre-lowercased (enforced by
// declaration convention) and additionally lowercased here for defence in
// depth, so a mixed-case pattern slipping into the list will still match.
func matchesTransientPattern(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, p := range transientStatusCodePatterns {
		if strings.Contains(msg, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// retryablePatterns contains error message substrings that indicate a
// transient/retryable failure. Numeric status codes (500, 502, etc.) are
// handled separately by extractHTTPStatusCode + isRetryableStatusCode.
var retryablePatterns = []string{
	"timeout",               // Generic timeout
	"connection reset",      // Connection reset
	"connection refused",    // Connection refused
	"no such host",          // DNS failure
	"temporary failure",     // Temporary failure
	"service unavailable",   // Service unavailable
	"internal server error", // Server error
	"bad gateway",           // Gateway error
	"gateway timeout",       // Gateway timeout
	"overloaded",            // Server overloaded
	"overloaded_error",      // Server overloaded
	"other side closed",     // Connection closed by peer
	"fetch failed",          // Network fetch failure
	"reset before headers",  // Connection reset before headers received
	"upstream connect",      // Upstream connection error
	"internal_error",        // HTTP/2 INTERNAL_ERROR (stream-level)
}

// streamTruncationPatterns matches errors where the response body was cut off
// mid-stream, before a clean end-of-stream marker. For local backends (e.g.
// Docker Model Runner / llama.cpp) this most often happens when a large
// prompt's prefill takes long enough with no bytes flowing that an idle or
// connection timeout in the model server -- or a proxy in front of it -- drops
// the connection before the first token is streamed (see issue #3298). The
// connection reset / closed-by-peer variants are already covered by
// [retryablePatterns]; these two cover the clean-FIN-mid-event shapes that
// surface from the SSE/JSON decoder instead of the socket layer.
//
// Patterns MUST be lowercase: they are compared against a lowercased error
// message (see [IsStreamTruncationError]).
var streamTruncationPatterns = []string{
	"unexpected eof",               // io.ErrUnexpectedEOF: HTTP body cut mid-frame
	"unexpected end of json input", // json.Unmarshal on a truncated SSE event
}

// IsStreamTruncationError reports whether err indicates the model stream was
// cut off before a clean completion: the connection was dropped or the body
// was truncated mid-stream. See [streamTruncationPatterns] for why this happens
// and why it is treated as retryable.
//
// Context cancellation/deadline is excluded: that is the caller tearing the
// request down (e.g. the idle-timeout cancel or a user Ctrl+C), not an upstream
// drop, and must stay non-retryable.
func IsStreamTruncationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, p := range streamTruncationPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// nonRetryablePatterns contains error message substrings that indicate a
// permanent/non-retryable failure. Numeric status codes (429, 401, etc.) are
// handled separately by extractHTTPStatusCode.
var nonRetryablePatterns = []string{
	"rate limit",        // Rate limit message
	"too many requests", // Rate limit message
	"throttl",           // Throttling (rate limiting)
	"quota",             // Quota exceeded
	"capacity",          // Capacity issues (often rate-limit related)
	"invalid",           // Invalid request
	"unauthorized",      // Auth error
	"authentication",    // Auth error
	"api key",           // API key error
}

// isRetryableModelError determines if an error should trigger a retry of the SAME model.
// It is used as a fallback by ClassifyModelError when no *StatusError is present.
//
// Retryable errors (retry same model with backoff):
// - Network timeouts
// - Temporary network errors
// - HTTP 5xx errors (server errors)
// - HTTP 529 (Anthropic overloaded)
// - HTTP 408 (request timeout)
//
// Non-retryable errors (skip to next model in chain immediately):
// - Context cancellation
// - HTTP 429 (rate limit) - provider is explicitly rate limiting us
// - HTTP 4xx errors (client errors)
// - Authentication errors
// - Invalid request errors
//
// The key distinction is: 429 means "you're calling too fast, slow down" which
// suggests we should try a different model, not keep hammering the same one.
func isRetryableModelError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation is never retryable
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Context overflow errors are never retryable — the context hasn't changed
	// between attempts, so retrying the same oversized payload will always fail.
	// This avoids wasting time on 3 attempts + exponential backoff.
	if IsContextOverflowError(err) {
		slog.Debug("Context overflow error, not retryable", "error", err)
		return false
	}

	// First, try to extract HTTP status code from known SDK error types
	if statusCode := extractHTTPStatusCode(err); statusCode != 0 {
		retryable := isRetryableStatusCode(statusCode) || matchesTransientPattern(err)
		slog.Debug("Classified error by status code",
			"status_code", statusCode,
			"retryable", retryable)
		return retryable
	}

	// Check for network errors
	if netErr, ok := errors.AsType[net.Error](err); ok {
		// Timeout errors are retryable
		if netErr.Timeout() {
			slog.Debug("Network timeout error, retryable", "error", err)
			return true
		}
	}

	// A stream truncated mid-response is transient: the upstream (or a proxy)
	// dropped a connection that was making progress, most often during a long
	// prefill with no bytes flowing yet. Retrying usually succeeds because the
	// backend's prompt cache makes the second prefill fast enough to beat the
	// idle limit. Checked before the pattern loops below so the truncated-JSON
	// variant isn't mis-classified by the "invalid" non-retryable pattern.
	if IsStreamTruncationError(err) {
		slog.Debug("Stream truncated mid-response, retryable", "error", err)
		return true
	}

	errMsg := strings.ToLower(err.Error())

	for _, pattern := range retryablePatterns {
		if strings.Contains(errMsg, pattern) {
			slog.Debug("Matched retryable error pattern", "pattern", pattern)
			return true
		}
	}

	for _, pattern := range nonRetryablePatterns {
		if strings.Contains(errMsg, pattern) {
			slog.Debug("Matched non-retryable error pattern", "pattern", pattern)
			return false
		}
	}

	// Default: don't retry unknown errors to be safe
	slog.Debug("Unknown error type, not retrying", "error", err)
	return false
}

// parseRetryAfterHeader parses a Retry-After header value.
// Supports both seconds (integer) and HTTP-date formats per RFC 7231 §7.1.3.
// Returns 0 if the value is empty, invalid, or results in a non-positive duration.
func parseRetryAfterHeader(value string) time.Duration {
	if value == "" {
		return 0
	}
	// Try integer seconds first (most common for rate limits)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	// Try HTTP-date format
	if t, err := http.ParseTime(value); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// ClassifyModelError classifies an error for the retry/fallback decision.
//
// If the error chain contains a *StatusError (wrapped by provider adapters),
// its StatusCode and RetryAfter fields are used directly — no provider-specific
// imports needed in the caller.
//
// Returns:
//   - retryable=true:    retry the SAME model with backoff (5xx, timeouts)
//   - rateLimited=true:  it's a 429 error; caller decides retry vs fallback based on config
//   - retryAfter:        Retry-After duration from the provider (only set for 429)
//
// When rateLimited=true, retryable is always false — the caller is responsible for
// deciding whether to retry (when no fallback is configured) or skip to the next
// model (when fallbacks are available).
func ClassifyModelError(err error) (retryable, rateLimited bool, retryAfter time.Duration) {
	if err == nil {
		return false, false, 0
	}

	// Context cancellation and deadline are never retryable.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, false, 0
	}

	// Context overflow errors are never retryable — retrying the same oversized
	// payload will always fail.
	if IsContextOverflowError(err) {
		return false, false, 0
	}

	// Primary path: typed StatusError wrapped by provider adapters.
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode == http.StatusTooManyRequests {
			return false, true, statusErr.RetryAfter
		}
		return isRetryableStatusCode(statusErr.StatusCode) || matchesTransientPattern(err), false, 0
	}

	// Fallback: providers that don't yet wrap (e.g. Bedrock), or non-provider
	// errors (network, pattern matching).
	statusCode := extractHTTPStatusCode(err)
	if statusCode == http.StatusTooManyRequests {
		return false, true, 0 // No Retry-After without StatusError
	}
	if statusCode != 0 {
		return isRetryableStatusCode(statusCode) || matchesTransientPattern(err), false, 0
	}
	return isRetryableModelError(err), false, 0
}

// FormatError returns a user-friendly error message for model errors.
// Overflow errors get a kind-specific actionable message; other errors fall
// through to err.Error(). For HTTP errors that text comes from *StatusError,
// which itself extracts structured provider details (see parseProviderError).
//
// The messages are provider-agnostic by design: docker-agent supports many
// LLM providers and the cap that triggered the rejection is a deployment
// detail of the provider, not something the user can act on by name.
func FormatError(err error) string {
	if err == nil {
		return ""
	}

	switch OverflowKindOf(err) {
	case OverflowKindWire:
		return "Your message is too large for the AI provider. " +
			"Try a smaller paste, attach the file separately, or split the content."
	case OverflowKindMedia:
		return "An image or file in this conversation is too large for the AI provider. " +
			"Try a smaller file or remove it from context."
	// Frontend-agnostic by design too: this error reaches every consumer of
	// the runtime event stream, and not all of them expose a /compact
	// command, so name the action ("compact") rather than a command. It also
	// avoids claiming compaction "is not enabled": this path is reached both
	// when compaction is disabled and when its retries are exhausted.
	case OverflowKindTokens:
		return "The conversation has exceeded the model's context window. " +
			"Compact the conversation to reduce its size, or start a new session."
	}

	if IsStreamTruncationError(err) {
		return "The connection to the model was closed unexpectedly before it completed its response. " +
			"With local models (e.g. Docker Model Runner), this can happen when an idle or " +
			"connection timeout in the model server (or a proxy in front of it) drops a silent " +
			"prefill before the first token, or cuts a long response mid-stream. " +
			"Try a shorter prompt, or raise the model server's idle/keep-alive timeout."
	}

	return err.Error()
}

// requestIDRegex matches the `(Request-ID: <id>)` segment that anthropic-sdk-go
// and openai-go append between the status text and the response body.
var requestIDRegex = regexp.MustCompile(`\(Request-ID:\s*([^)\s]+)\)`)

// providerErrorBody is the union of JSON shapes returned by major LLM
// providers in non-2xx response bodies:
//
//	Anthropic   {"type":"error","error":{"type":"...","message":"..."}}
//	OpenAI      {"error":{"message":"...","type":"...","code":"...","param":"..."}}
//	Gemini      {"error":{"code":N,"message":"...","status":"..."}}
//	Proxies     {"message":"Bad Request"}
type providerErrorBody struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    any    `json:"code"`  // string (OpenAI) or number (Gemini)
		Param   any    `json:"param"` // string or null
		Status  string `json:"status"`
	} `json:"error"`
	Message string `json:"message"`
}

// parseProviderError returns a focused details line lifted from an SDK error
// message of the form
//
//	<METHOD> "<URL>": <code> <statusText>[ (Request-ID: <id>)] <jsonBody>
//
// Returns "" when no JSON body is present or it has no recognised fields.
func parseProviderError(s string) string {
	body := firstJSONObject(s)
	if body == nil {
		return ""
	}
	var parsed providerErrorBody
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	details := formatProviderError(&parsed)
	if details == "" {
		return ""
	}
	if m := requestIDRegex.FindStringSubmatch(s); len(m) >= 2 {
		details += " (Request-ID: " + m[1] + ")"
	}
	return details
}

// formatProviderError renders a parsed body as
//
//	<error.type>: <error.message> (code=..., param=..., status=...)
//
// Falls back to the top-level `message` field for minimal/proxy bodies.
// Returns "" when the body has nothing useful.
func formatProviderError(p *providerErrorBody) string {
	if p.Error == nil || p.Error.Message == "" {
		return p.Message
	}
	msg := p.Error.Message
	if p.Error.Type != "" {
		msg = p.Error.Type + ": " + msg
	}

	var meta []string
	if code := scalarString(p.Error.Code); code != "" {
		meta = append(meta, "code="+code)
	}
	if param := scalarString(p.Error.Param); param != "" {
		meta = append(meta, "param="+param)
	}
	if p.Error.Status != "" && !strings.EqualFold(p.Error.Status, p.Error.Type) {
		meta = append(meta, "status="+p.Error.Status)
	}
	if len(meta) > 0 {
		msg += " (" + strings.Join(meta, ", ") + ")"
	}
	return msg
}

// firstJSONObject returns the first complete JSON object found in s, or nil.
// encoding/json handles escaped quotes and braces inside string values.
// Limits parsing to 1MB to prevent memory exhaustion from malicious or
// accidentally huge error responses.
//
// To handle '{' characters in URLs or status text (e.g., "param={value}"),
// we try parsing from each '{' position until we find valid JSON.
func firstJSONObject(s string) []byte {
	const maxJSONSize = 1 << 20 // 1MB

	pos := 0
	for {
		idx := strings.IndexByte(s[pos:], '{')
		if idx < 0 {
			return nil
		}
		start := pos + idx

		reader := io.LimitReader(strings.NewReader(s[start:]), maxJSONSize)
		var raw json.RawMessage
		if err := json.NewDecoder(reader).Decode(&raw); err == nil {
			// Successfully decoded JSON
			return raw
		}
		// Try next '{' position
		pos = start + 1
	}
}

// scalarString renders a JSON scalar as text. JSON numbers decode to float64;
// whole numbers are rendered without a trailing ".0" so a code of 400 prints
// as "400". Returns "" for nil and empty strings.
func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// RetryableHTTPStatus reports whether err contains an HTTP status that warrants
// backoff (429 rate-limit, 408 request-timeout, or a 5xx server error). It
// checks for a *StatusError in the chain first; if none is found it falls back
// to matching \b([45]\d{2})\b in the error message, which can produce false
// positives for port numbers or similar numeric patterns. Callers that need
// strict StatusError-only classification should pre-filter with errors.As.
// No context-error handling is performed.
func RetryableHTTPStatus(err error) bool {
	code := extractHTTPStatusCode(err)
	if code == 0 {
		return false
	}
	return code == http.StatusTooManyRequests || isRetryableStatusCode(code)
}
