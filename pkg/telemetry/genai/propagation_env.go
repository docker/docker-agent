package genai

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// envCarrier adapts an env-var key/value map to OTel's TextMapCarrier so
// the configured propagator can write traceparent / tracestate / baggage
// into a subprocess's environment. Keys are uppercased on Set to match
// the convention subprocess-propagation tools (otel-cli, OTel SDKs)
// expect.
type envCarrier map[string]string

func (c envCarrier) Get(key string) string { return c[strings.ToUpper(key)] }
func (c envCarrier) Set(key, value string) { c[strings.ToUpper(key)] = value }
func (c envCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

var _ propagation.TextMapCarrier = envCarrier{}

// InjectTraceContextEnv returns `KEY=VALUE` env-var strings carrying the
// W3C trace context for the current span. Use to extend `exec.Cmd.Env`
// for direct subprocess spawns (hook scripts, LSP servers) so OTel-aware
// children chain onto the active span.
//
// Returns nil when no propagator is configured or when the active context
// has no span context to inject.
func InjectTraceContextEnv(ctx context.Context) []string {
	carrier := envCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	out := make([]string, 0, len(carrier))
	for k, v := range carrier {
		out = append(out, k+"="+v)
	}
	return out
}
