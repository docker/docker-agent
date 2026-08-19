package options

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

type ModelOptions struct {
	gateway          string
	structuredOutput *latest.StructuredOutput
	generatingTitle  bool
	compacting       bool
	noThinking       bool
	maxTokens        int64
	providers        map[string]latest.ProviderConfig
	modelsDevStore   *modelsdev.Store
	transportWrapper func(http.RoundTripper) http.RoundTripper
	openAIVendor     bool
}

func (c *ModelOptions) Gateway() string {
	return c.gateway
}

func (c *ModelOptions) StructuredOutput() *latest.StructuredOutput {
	return c.structuredOutput
}

func (c *ModelOptions) GeneratingTitle() bool {
	return c.generatingTitle
}

func (c *ModelOptions) Compacting() bool {
	return c.compacting
}

func (c *ModelOptions) MaxTokens() int64 {
	return c.maxTokens
}

func (c *ModelOptions) NoThinking() bool {
	return c.noThinking
}

func (c *ModelOptions) Providers() map[string]latest.ProviderConfig {
	return c.providers
}

func (c *ModelOptions) ModelsDevStore() *modelsdev.Store {
	return c.modelsDevStore
}

// OpenAIVendor reports whether the model this ModelOptions was built for is a
// genuine OpenAI vendor endpoint (as opposed to a third-party provider that
// merely speaks the OpenAI-compatible wire protocol, e.g. xai/mistral). It is
// set exclusively by [WithOpenAIVendor], which pkg/model/provider's factory
// calls after applyProviderDefaults has resolved custom providers and
// built-in aliases — never by user-controllable config (YAML/provider_opts
// cannot reach this field). The OpenAI client trusts it for gating
// OpenAI-only wire behavior (e.g. gpt-5.6's real "none" reasoning effort)
// that must not leak onto an OpenAI-compatible alias for a different vendor.
func (c *ModelOptions) OpenAIVendor() bool {
	return c.openAIVendor
}

// TransportWrapper returns the HTTP transport wrapper function registered via
// WithHTTPTransportWrapper, or nil if none was set.
func (c *ModelOptions) TransportWrapper() func(http.RoundTripper) http.RoundTripper {
	return c.transportWrapper
}

// WrapTransport applies the registered transport wrapper (if any) to client's
// transport in place. A wrapper that returns nil is treated as a no-op and the
// original transport is kept (with a warning). No-op when client is nil or no
// wrapper is registered.
func (c *ModelOptions) WrapTransport(ctx context.Context, client *http.Client) {
	w := c.transportWrapper
	if w == nil || client == nil {
		return
	}
	if wrapped := w(client.Transport); wrapped != nil {
		client.Transport = wrapped
	} else {
		slog.WarnContext(ctx, "HTTP transport wrapper returned nil; using original transport")
	}
}

type Opt func(*ModelOptions)

// Apply builds a ModelOptions from a list of Opts, skipping nil entries.
// It centralises the accumulation loop that every provider constructor
// otherwise repeats.
func Apply(opts ...Opt) ModelOptions {
	var m ModelOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&m)
		}
	}
	return m
}

func WithGateway(gateway string) Opt {
	return func(cfg *ModelOptions) {
		cfg.gateway = gateway
	}
}

// WithStructuredOutput sets the native structured-output configuration
// forwarded to providers. Tool-mode structured output
// (structured_output.mode: tool) is enforced by the runtime through an
// internal tool, so it is deliberately dropped here: no provider must
// constrain the response natively in that mode.
func WithStructuredOutput(structuredOutput *latest.StructuredOutput) Opt {
	return func(cfg *ModelOptions) {
		if structuredOutput.ToolMode() {
			cfg.structuredOutput = nil
			return
		}
		cfg.structuredOutput = structuredOutput
	}
}

func WithGeneratingTitle() Opt {
	return func(cfg *ModelOptions) {
		cfg.generatingTitle = true
	}
}

func WithCompacting() Opt {
	return func(cfg *ModelOptions) {
		cfg.compacting = true
	}
}

func WithMaxTokens(maxTokens int64) Opt {
	return func(cfg *ModelOptions) {
		cfg.maxTokens = maxTokens
	}
}

func WithNoThinking() Opt {
	return func(cfg *ModelOptions) {
		cfg.noThinking = true
	}
}

func WithProviders(providers map[string]latest.ProviderConfig) Opt {
	return func(cfg *ModelOptions) {
		cfg.providers = providers
	}
}

func WithModelsDevStore(store *modelsdev.Store) Opt {
	return func(cfg *ModelOptions) {
		cfg.modelsDevStore = store
	}
}

// WithOpenAIVendor records the trusted, internally-resolved "genuine OpenAI
// vendor" bit (see [ModelOptions.OpenAIVendor]). It is meant to be called
// exactly once, by pkg/model/provider's Registry after applyProviderDefaults
// has resolved custom providers and built-in aliases — not from user config,
// since no YAML/provider_opts key can invoke a Go option constructor.
func WithOpenAIVendor(v bool) Opt {
	return func(cfg *ModelOptions) {
		cfg.openAIVendor = v
	}
}

// WithHTTPTransportWrapper registers a function that wraps the HTTP transport
// used by provider clients (Anthropic, OpenAI, and Gemini with the Gemini API
// backend). The function receives the transport that docker-agent built
// (including OTel instrumentation, SSE decompression fix, and Desktop proxy
// support) and must return a new RoundTripper that delegates to it. The wrapper
// is applied in both direct mode and gateway/proxy mode.
//
// Call-frequency note: in direct mode the wrapper is invoked once at client
// construction time; in gateway mode it is invoked on every LLM request
// (because gateway clients are rebuilt on each call to refresh short-lived
// auth tokens). Wrappers with per-call side effects (metrics, token rotation)
// will therefore be called more frequently in gateway mode.
//
// Limitations:
//   - OpenAI clients configured with transport=websocket bypass the HTTP
//     transport layer entirely; the wrapper is not applied in that mode.
//   - Gemini clients using the Vertex AI backend (project/location config or
//     GOOGLE_GENAI_USE_VERTEXAI) rely on the genai SDK's default HTTP client;
//     the wrapper is not applied and a warning is logged.
//
// The wrapper function must return a non-nil RoundTripper; returning nil is a
// no-op (a warning is logged and the original transport is kept).
//
// Example — inject a bearer token on every outbound LLM request:
//
//	options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
//	    return &bearerTransport{token: myToken, base: base}
//	})
func WithHTTPTransportWrapper(fn func(base http.RoundTripper) http.RoundTripper) Opt {
	return func(cfg *ModelOptions) {
		cfg.transportWrapper = fn
	}
}

// FromModelOptions converts a concrete ModelOptions value into a slice of
// Opt configuration functions. Later Opts override earlier ones when applied.
func FromModelOptions(m ModelOptions) []Opt {
	var out []Opt
	if g := m.Gateway(); g != "" {
		out = append(out, WithGateway(g))
	}
	if m.structuredOutput != nil {
		out = append(out, WithStructuredOutput(m.structuredOutput))
	}
	if m.generatingTitle {
		out = append(out, WithGeneratingTitle())
	}
	if m.compacting {
		out = append(out, WithCompacting())
	}
	if m.noThinking {
		out = append(out, WithNoThinking())
	}
	if m.maxTokens != 0 {
		out = append(out, WithMaxTokens(m.maxTokens))
	}
	if len(m.providers) > 0 {
		out = append(out, WithProviders(m.providers))
	}
	if m.modelsDevStore != nil {
		out = append(out, WithModelsDevStore(m.modelsDevStore))
	}
	if m.transportWrapper != nil {
		out = append(out, WithHTTPTransportWrapper(m.transportWrapper))
	}
	if m.openAIVendor {
		out = append(out, WithOpenAIVendor(true))
	}
	return out
}
