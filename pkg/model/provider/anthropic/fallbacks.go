package anthropic

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// serverSideFallbackBeta enables the `fallbacks` request parameter. The July
// date accepts both "default" and explicit lists.
const serverSideFallbackBeta = anthropic.AnthropicBetaServerSideFallback2026_07_01

// maxFallbacks is the longest list the API accepts.
const maxFallbacks = 3

// fallbackSpec is the parsed `fallbacks` provider_opt: the server's default
// routing, or an explicit list of models tried in order.
type fallbackSpec struct {
	Default bool
	Models  []string
}

func (s fallbackSpec) enabled() bool { return s.Default || len(s.Models) > 0 }

func (s fallbackSpec) param() anthropic.BetaFallbacksParamUnion {
	if s.Default {
		return anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	list := make([]anthropic.BetaFallbackParam, len(s.Models))
	for i, m := range s.Models {
		list[i] = anthropic.BetaFallbackParam{Model: m}
	}
	return anthropic.BetaFallbacksParamUnion{OfBetaFallbackArray: list}
}

func fallbacksEnabled(opts map[string]any) bool {
	spec, _ := parseFallbacks(opts)
	return spec.enabled()
}

// claudeAPIHost is the only host that serves server-side fallback.
const claudeAPIHost = "api.anthropic.com"

// validateFallbackHost rejects fallbacks unless requests go straight to the
// Claude API: other platforms and proxies don't support the parameter.
func validateFallbackHost(cfg *latest.ModelConfig, gateway string) error {
	if spec, _ := parseFallbacks(cfg.ProviderOpts); !spec.enabled() {
		return nil
	}
	if gateway != "" {
		return errors.New("anthropic: fallbacks requires the Claude API and is not available through a gateway")
	}
	// Without base_url, the SDK falls back to ANTHROPIC_BASE_URL.
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err != nil || !strings.EqualFold(u.Hostname(), claudeAPIHost) {
			return fmt.Errorf("anthropic: fallbacks requires the Claude API and is not available through base URL %q", baseURL)
		}
	}
	return nil
}

// parseFallbacks reads the `fallbacks` provider_opt: "default", or a list of
// at most three distinct model IDs. An absent or empty value disables
// fallbacks.
func parseFallbacks(opts map[string]any) (fallbackSpec, error) {
	var entries []any
	switch v := opts["fallbacks"].(type) {
	case nil:
		return fallbackSpec{}, nil
	case string:
		if v != "default" {
			return fallbackSpec{}, fmt.Errorf(`anthropic: fallbacks must be "default" or a list of model IDs, got %q`, v)
		}
		return fallbackSpec{Default: true}, nil
	case []string:
		for _, m := range v {
			entries = append(entries, m)
		}
	case []any:
		entries = v
	default:
		return fallbackSpec{}, fmt.Errorf(`anthropic: fallbacks must be "default" or a list of model IDs, got %T`, v)
	}
	if len(entries) > maxFallbacks {
		return fallbackSpec{}, fmt.Errorf("anthropic: fallbacks accepts at most %d models, got %d", maxFallbacks, len(entries))
	}
	var models []string
	for _, e := range entries {
		m, ok := e.(string)
		if !ok || m == "" {
			return fallbackSpec{}, fmt.Errorf("anthropic: fallbacks entries must be model IDs, got %v", e)
		}
		if slices.Contains(models, m) {
			return fallbackSpec{}, fmt.Errorf("anthropic: fallbacks lists %q twice", m)
		}
		models = append(models, m)
	}
	return fallbackSpec{Models: models}, nil
}

// fallbackTargets returns the explicit fallback models; none for "default",
// whose targets the server picks. Construction has already validated the
// option, so a malformed value reads as none.
func fallbackTargets(opts map[string]any) []string {
	spec, _ := parseFallbacks(opts)
	return spec.Models
}

// validateFallbackSpec checks the rules the API enforces with a 400, so a bad
// config fails at client construction.
func validateFallbackSpec(cfg *latest.ModelConfig) error {
	spec, err := parseFallbacks(cfg.ProviderOpts)
	if err != nil {
		return err
	}
	if slices.Contains(spec.Models, cfg.Model) {
		return fmt.Errorf("anthropic: fallback %q is the same as the model", cfg.Model)
	}
	// The request must be valid for every listed model, or the API rejects it
	// up front. Claude 5.x and newer Opus models refuse sampling settings and
	// token thinking budgets that older primaries send. Check what the request
	// builder actually sends, after it resolves the thinking budget.
	sent := sentRequestShape(cfg)
	for _, m := range spec.Models {
		if sent.sampling && rejectsSampling(m) {
			return fmt.Errorf("anthropic: the request sends temperature or top_p (thinking is off), which fallback %q rejects; remove them or that fallback", m)
		}
		if sent.topK && rejectsSampling(m) {
			return fmt.Errorf("anthropic: the request sends top_k, which fallback %q rejects; remove it or that fallback", m)
		}
		if sent.tokenThinking && modelinfo.RejectsTokenThinking(m) {
			return fmt.Errorf("anthropic: %s thinks with a token budget, which fallback %q rejects; turn thinking off or remove that fallback", cfg.Model, m)
		}
	}
	if m, ok := modelinfo.LookupClaude5(cfg.Model); ok && m.NoServerSideFallback {
		if len(spec.Models) > 0 {
			return fmt.Errorf("anthropic: %s has no server-side fallback; remove the fallbacks list", cfg.Model)
		}
		if spec.Default {
			slog.Warn("Anthropic: fallbacks: default has no effect on this model", "model", cfg.Model)
		}
	}
	return nil
}

// requestShape is what a request built from a config sends, for the fields a
// fallback model may refuse.
type requestShape struct {
	sampling, topK, tokenThinking bool
}

// sentRequestShape mirrors the request builder: temperature and top_p only go
// out with thinking off, top_k whenever set, and the thinking budget after
// resolution, which turns an effort level into tokens on models without
// adaptive thinking.
func sentRequestShape(cfg *latest.ModelConfig) requestShape {
	c := &Client{Config: base.Config{ModelConfig: *cfg}}
	budget := c.resolveThinkingBudget()
	thinking := budget != nil && !budget.IsDisabled()
	_, adaptive := anthropicThinkingEffort(budget)
	_, topK := cfg.ProviderOpts["top_k"]
	primaryRejects := rejectsSampling(cfg.Model)
	return requestShape{
		sampling:      !thinking && !primaryRejects && (cfg.Temperature != nil || cfg.TopP != nil),
		topK:          topK && !primaryRejects,
		tokenThinking: thinking && !adaptive && budget.Tokens > 0,
	}
}
