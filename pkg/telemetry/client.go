package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/desktop"
)

// telemetryLogger wraps slog.Logger to automatically prepend "[Telemetry]" to all messages
type telemetryLogger struct {
	logger *slog.Logger
}

// NewTelemetryLogger creates a new telemetry logger that automatically prepends "[Telemetry]" to all messages
func NewTelemetryLogger(logger *slog.Logger) *telemetryLogger {
	return &telemetryLogger{logger: logger}
}

// sanitizeLogValue replaces newline and carriage-return characters in s to
// prevent log-injection when the value is forwarded to a text-format sink.
func sanitizeLogValue(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// sanitizeLogArgs sanitizes every element of the args slice so that no
// user-controlled string value can reach a slog sink without passing
// through a strings.ReplaceAll barrier (the only transformation that
// CodeQL's go/log-injection query recognises as a sanitizer).
//
// Strategy — invert the passthrough logic so the catch-all sanitizes
// rather than forwards:
//   - plain string       → sanitizeLogValue directly
//   - numeric / bool     → pass through as-is (provably not a string;
//     preserves structured-log typing for non-sensitive fields)
//   - everything else    → render via fmt.Sprintf and sanitize; this
//     catches named string types (e.g. EventType), error, fmt.Stringer,
//     and any future string-like type without an explicit case.
func sanitizeLogArgs(args []any) []any {
	if len(args) == 0 {
		return args
	}
	result := make([]any, len(args))
	for i, v := range args {
		switch val := v.(type) {
		case string:
			result[i] = sanitizeLogValue(val)
		case int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64, bool:
			result[i] = val
		default:
			// Named string types, errors, Stringers, and anything else:
			// render then sanitize so every string-like path crosses the
			// strings.ReplaceAll barrier.
			result[i] = sanitizeLogValue(fmt.Sprintf("%v", val))
		}
	}
	return result
}

// Debug logs a debug message with "[Telemetry]" prefix
func (tl *telemetryLogger) Debug(msg string, args ...any) {
	tl.logger.Debug("[Telemetry]", append([]any{"telemetry_msg", sanitizeLogValue(msg)}, sanitizeLogArgs(args)...)...)
}

// Info logs an info message with "[Telemetry]" prefix
func (tl *telemetryLogger) Info(msg string, args ...any) {
	tl.logger.Info("[Telemetry]", append([]any{"telemetry_msg", sanitizeLogValue(msg)}, sanitizeLogArgs(args)...)...)
}

// Warn logs a warning message with "[Telemetry]" prefix
func (tl *telemetryLogger) Warn(msg string, args ...any) {
	tl.logger.Warn("[Telemetry]", append([]any{"telemetry_msg", sanitizeLogValue(msg)}, sanitizeLogArgs(args)...)...)
}

// Error logs an error message with "[Telemetry]" prefix
func (tl *telemetryLogger) Error(msg string, args ...any) {
	tl.logger.Error("[Telemetry]", append([]any{"telemetry_msg", sanitizeLogValue(msg)}, sanitizeLogArgs(args)...)...)
}

// Enabled returns whether the logger is enabled for the given level
func (tl *telemetryLogger) Enabled(ctx context.Context, level slog.Level) bool {
	return tl.logger.Enabled(ctx, level)
}

func newClient(ctx context.Context, logger *slog.Logger, enabled, debugMode bool, version string, customHTTPClient ...*http.Client) *Client {
	telemetryLogger := NewTelemetryLogger(logger)

	if !enabled {
		return &Client{
			logger:    telemetryLogger,
			enabled:   false,
			debugMode: debugMode,
			version:   version,
		}
	}

	header := "x-api-key"

	endpoint := "https://api.docker.com/events/v1/track"
	apiKey := "Gxw1IjiDEP29dWm9DanuE2XhIKKzqDEY4iGlW1P0"

	// Use staging configuration in debug mode
	if debugMode {
		endpoint = "https://api-stage.docker.com/events/v1/track"
		apiKey = "z4sTQ8eDid2nJ53md8ptCaZlVxvIlhvf4AGR7oi5"
	}

	var httpClient *http.Client
	if len(customHTTPClient) > 0 && customHTTPClient[0] != nil {
		httpClient = customHTTPClient[0]
	} else {
		httpClient = &http.Client{Timeout: 30 * time.Second} //rubocop:disable Lint/HTTPClientTransport // telemetry client sends to a controlled endpoint; OTel transport would create trace loops
	}

	client := &Client{
		logger:      telemetryLogger,
		userUUID:    getUserUUID(),
		desktopUUID: desktop.GetUUID(ctx),
		enabled:     enabled,
		debugMode:   debugMode,
		httpClient:  httpClient,
		endpoint:    endpoint,
		apiKey:      apiKey,
		header:      header,
		version:     version,
	}

	telemetryLogger.Debug("Enabled:", enabled)

	return client
}
