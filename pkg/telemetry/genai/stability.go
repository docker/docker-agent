package genai

import (
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// EnvSemconvStability is the OTel-defined environment variable that lets
// callers opt into experimental versions of the GenAI semantic
// conventions
// (https://github.com/open-telemetry/semantic-conventions/blob/main/docs/gen-ai/README.md).
//
// It is a comma-separated list of opt-in tokens. The only token defined
// for GenAI today is `gen_ai_latest_experimental` — when present, the
// instrumentation emits only the spec-defined `gen_ai.*` attributes and
// drops the legacy attribute names (e.g. `tool.name`, `agent`,
// `session.id`).
//
// Default behaviour (env var unset) is dual-emit: spans carry both the
// legacy keys and the `gen_ai.*` keys so existing dashboards keep
// working alongside spec-aware tooling. This matches the spec's
// recommendation that instrumentations not change the version of
// conventions they emit by default and instead require the opt-in for
// the new version.
const EnvSemconvStability = "OTEL_SEMCONV_STABILITY_OPT_IN"

// stabilityToken is the spec-defined opt-in for the latest experimental
// GenAI conventions.
const stabilityToken = "gen_ai_latest_experimental"

// Stability identifies which version of attribute names a span should
// emit.
type Stability int

const (
	// StabilityDualEmit is the default: emit both legacy attribute
	// names (`tool.name`, `agent`, `session.id`, ...) and the
	// `gen_ai.*` keys, so existing dashboards continue working while
	// spec-aware tooling sees the new values.
	StabilityDualEmit Stability = iota
	// StabilityGenAILatest is selected by
	// `OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental`. Only
	// the `gen_ai.*` attributes are emitted; the legacy keys are
	// dropped.
	StabilityGenAILatest
)

var (
	stabilityMu     sync.Mutex
	stabilityOnce   sync.Once
	cachedStability Stability
)

// CurrentStability returns the active stability mode. The result is
// computed once per process from the env var.
func CurrentStability() Stability {
	stabilityMu.Lock()
	once := &stabilityOnce
	stabilityMu.Unlock()

	once.Do(func() {
		raw := os.Getenv(EnvSemconvStability)
		for tok := range strings.SplitSeq(raw, ",") {
			// Spec: tokens are case-insensitive.
			if strings.EqualFold(strings.TrimSpace(tok), stabilityToken) {
				stabilityMu.Lock()
				cachedStability = StabilityGenAILatest
				stabilityMu.Unlock()
				return
			}
		}
		stabilityMu.Lock()
		defer stabilityMu.Unlock()
		cachedStability = StabilityDualEmit
	})

	stabilityMu.Lock()
	defer stabilityMu.Unlock()
	return cachedStability
}

// EmitLegacyAttributes reports whether legacy (pre-semconv) attribute
// keys should be emitted. True when stability is StabilityDualEmit;
// false when the user has opted into `gen_ai_latest_experimental`.
func EmitLegacyAttributes() bool {
	return CurrentStability() == StabilityDualEmit
}

// LegacyToolAttributes returns the historic tool dispatcher attribute
// set (`tool.name`, `agent`, `session.id`, `tool.call_id`,
// `tool.type`) — but only when legacy emission is enabled. Returns nil
// otherwise so call sites can append unconditionally.
func LegacyToolAttributes(toolName, toolType, agentName, sessionID, callID string) []attribute.KeyValue {
	if !EmitLegacyAttributes() {
		return nil
	}
	attrs := []attribute.KeyValue{
		attribute.String("tool.name", toolName),
		attribute.String("agent", agentName),
		attribute.String("session.id", sessionID),
	}
	if toolType != "" {
		attrs = append(attrs, attribute.String("tool.type", toolType))
	}
	if callID != "" {
		attrs = append(attrs, attribute.String("tool.call_id", callID))
	}
	return attrs
}
