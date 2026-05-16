package options

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

type ModelOptions struct {
	gateway          string
	structuredOutput *latest.StructuredOutput
	generatingTitle  bool
	noThinking       bool
	maxTokens        int64
	providers        map[string]latest.ProviderConfig
	modelsDevStore   *modelsdev.Store
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

type Opt func(*ModelOptions)

func WithGateway(gateway string) Opt {
	return func(cfg *ModelOptions) {
		cfg.gateway = gateway
	}
}

func WithStructuredOutput(structuredOutput *latest.StructuredOutput) Opt {
	return func(cfg *ModelOptions) {
		cfg.structuredOutput = structuredOutput
	}
}

func WithGeneratingTitle() Opt {
	return func(cfg *ModelOptions) {
		cfg.generatingTitle = true
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
	return out
}
