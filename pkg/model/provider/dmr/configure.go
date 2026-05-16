package dmr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// configureRequest mirrors model-runner's scheduling.ConfigureRequest.
type configureRequest struct {
	configureBackendConfig

	Model           string  `json:"model"`
	Mode            *string `json:"mode,omitempty"`
	RawRuntimeFlags string  `json:"raw-runtime-flags,omitempty"`
}

// configureBackendConfig mirrors model-runner's inference.BackendConfiguration.
type configureBackendConfig struct {
	ContextSize  *int32                      `json:"context-size,omitempty"`
	RuntimeFlags []string                    `json:"runtime-flags,omitempty"`
	Speculative  *speculativeDecodingRequest `json:"speculative,omitempty"`
	KeepAlive    *string                     `json:"keep_alive,omitempty"`
	VLLM         *vllmConfig                 `json:"vllm,omitempty"`
	LlamaCpp     *llamaCppConfig             `json:"llamacpp,omitempty"`
}

// vllmConfig mirrors model-runner's inference.VLLMConfig for POST /engines/_configure.
type vllmConfig struct {
	HFOverrides          map[string]any `json:"hf-overrides,omitempty"`
	GPUMemoryUtilization *float64       `json:"gpu-memory-utilization,omitempty"`
}

// llamaCppConfig mirrors model-runner's inference.LlamaCppConfig for POST /engines/_configure.
type llamaCppConfig struct {
	ReasoningBudget *int32 `json:"reasoning-budget,omitempty"`
}

func (c *llamaCppConfig) LogValue() slog.Value {
	if c == nil {
		return slog.AnyValue(nil)
	}
	var rb any
	if c.ReasoningBudget != nil {
		rb = *c.ReasoningBudget
	}
	return slog.GroupValue(slog.Any("reasoning-budget", rb))
}

// speculativeDecodingRequest mirrors model-runner's inference.SpeculativeDecodingConfig.
type speculativeDecodingRequest struct {
	DraftModel        string  `json:"draft_model,omitempty"`
	NumTokens         int     `json:"num_tokens,omitempty"`
	MinAcceptanceRate float64 `json:"min_acceptance_rate,omitempty"`
}

type speculativeDecodingOpts struct {
	draftModel     string
	numTokens      int
	acceptanceRate float64
}

func (so *speculativeDecodingOpts) LogValue() slog.Value {
	if so == nil {
		return slog.AnyValue(nil)
	}
	return slog.GroupValue(
		slog.String("draft-model", so.draftModel),
		slog.Int("num-tokens", so.numTokens),
		slog.Float64("acceptance-rate", so.acceptanceRate),
	)
}

// configureModel sends model configuration to Model Runner via POST /engines/_configure.
func configureModel(ctx context.Context, httpClient *http.Client, baseURL, model string, backend configureBackendConfig, mode *string, rawRuntimeFlags string) error {
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	configureURL := buildConfigureURL(baseURL)
	reqData, err := json.Marshal(buildConfigureRequest(model, backend, mode, rawRuntimeFlags))
	if err != nil {
		return fmt.Errorf("failed to marshal configure request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, configureTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, configureURL, bytes.NewReader(reqData))
	if err != nil {
		return fmt.Errorf("failed to create configure request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	slog.DebugContext(ctx, "Sending model configure request",
		"model", model,
		"url", configureURL,
		"context_size", derefInt32(backend.ContextSize),
		"runtime_flags", backend.RuntimeFlags,
		"raw_runtime_flags", rawRuntimeFlags,
		"mode", derefString(mode),
		"speculative_opts", backend.Speculative,
		"llamacpp", backend.LlamaCpp,
		"keep_alive", derefString(backend.KeepAlive),
		"vllm", backend.VLLM)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("configure request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("configure request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	slog.DebugContext(ctx, "Model configure completed", "model", model)
	return nil
}

// buildConfigureURL derives the /engines/_configure endpoint URL from the OpenAI base URL.
// It handles various URL formats:
//   - http://host:port/engines/v1/ → http://host:port/engines/_configure
//   - http://_/exp/vDD4.40/engines/v1 → http://_/exp/vDD4.40/engines/_configure
//   - http://host:port/engines/llama.cpp/v1/ → http://host:port/engines/llama.cpp/_configure
func buildConfigureURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1") + "/_configure"
	}

	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1")
	u.Path = path + "/_configure"
	return u.String()
}

func buildConfigureBackendConfig(contextSize *int64, runtimeFlags []string, specOpts *speculativeDecodingOpts, llamaCpp *llamaCppConfig, vllm *vllmConfig, keepAlive *string) configureBackendConfig {
	cfg := configureBackendConfig{
		RuntimeFlags: runtimeFlags,
		LlamaCpp:     llamaCpp,
		VLLM:         vllm,
		KeepAlive:    keepAlive,
	}
	if contextSize != nil {
		cs := int32(*contextSize) //nolint:gosec // user-configured context size; realistic values fit in int32
		cfg.ContextSize = &cs
	}
	if specOpts != nil {
		cfg.Speculative = &speculativeDecodingRequest{
			DraftModel:        specOpts.draftModel,
			NumTokens:         specOpts.numTokens,
			MinAcceptanceRate: specOpts.acceptanceRate,
		}
	}
	return cfg
}

// buildConfigureRequest constructs the JSON request body for POST /engines/_configure.
// mode and rawRuntimeFlags are top-level ConfigureRequest fields (not part of
// BackendConfiguration); pass nil / "" to omit.
func buildConfigureRequest(model string, backend configureBackendConfig, mode *string, rawRuntimeFlags string) configureRequest {
	return configureRequest{
		Model:                  model,
		Mode:                   mode,
		RawRuntimeFlags:        rawRuntimeFlags,
		configureBackendConfig: backend,
	}
}

// parseFloat64 attempts to parse a value as float64 from various types.
func parseFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case string:
		if s := strings.TrimSpace(t); s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// parseInt attempts to parse a value as int from various types.
func parseInt(v any) (int, bool) {
	if f, ok := parseFloat64(v); ok {
		return int(f), true
	}
	return 0, false
}

// parseInt64Value parses an int64 from YAML/JSON-decoded values (int, float64, string).
func parseInt64Value(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

// parseContextSize extracts context_size from provider_opts.
// Returns nil when unset, letting model-runner use its default.
func parseContextSize(opts map[string]any) *int64 {
	if len(opts) == 0 {
		return nil
	}
	v, ok := opts["context_size"]
	if !ok {
		return nil
	}
	if n, ok := parseInt64Value(v); ok {
		return &n
	}
	return nil
}

// resolveReasoningBudget normalizes a ThinkingBudget to a token count understood by model-runner backends:
//   - nil        → (0, false)  — budget unset, caller should omit the field entirely
//   - disabled   → (0, true)   — budget explicitly disabled, caller should send 0
//   - tokens > 0 → (n, true)   — explicit token count
//   - adaptive / unknown effort → (-1, true) — unlimited
//   - named effort → mapped token count
func resolveReasoningBudget(tb *latest.ThinkingBudget) (budget int64, ok bool) {
	if tb == nil {
		return 0, false
	}
	if tb.IsDisabled() {
		return 0, true
	}
	if tb.Tokens != 0 || tb.Effort == "" {
		return int64(tb.Tokens), true
	}
	if tb.IsAdaptive() {
		return -1, true
	}
	if tok, ok := tb.EffortTokens(); ok {
		return int64(tok), true
	}
	return -1, true // unknown effort → unlimited
}

// buildLlamaCppConfig constructs the llamacpp engine configuration from the model config.
// Currently maps thinking_budget to model-runner's llamacpp.reasoning-budget.
// Returns nil when no relevant config is set.
func buildLlamaCppConfig(cfg *latest.ModelConfig) *llamaCppConfig {
	if cfg == nil {
		return nil
	}
	budget, ok := resolveReasoningBudget(cfg.ThinkingBudget)
	if !ok {
		return nil
	}
	v := int32(budget) //nolint:gosec // resolved reasoning budget fits in int32
	return &llamaCppConfig{ReasoningBudget: &v}
}

// buildVLLMRequestFields constructs per-request extra fields for the vLLM engine.
// Currently maps thinking_budget to vLLM's thinking_token_budget sampling parameter.
// Returns nil when no extra fields are needed.
func buildVLLMRequestFields(cfg *latest.ModelConfig) map[string]any {
	if cfg == nil {
		return nil
	}
	budget, ok := resolveReasoningBudget(cfg.ThinkingBudget)
	if !ok {
		return nil
	}
	return map[string]any{"thinking_token_budget": budget}
}

func derefInt32(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

// derefString safely dereferences a *string for logging.
func derefString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// derefInt64 safely dereferences a *int64 for logging. Returns nil for nil pointers
// so slog renders "<nil>" instead of a memory address.
func derefInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

const (
	engineLlamaCpp = "llama.cpp"
	engineVLLM     = "vllm"
)

// noThinkingMinOutputTokens is the floor we enforce for NoThinking requests
// that also supply a small MaxTokens cap (e.g. session title generation sets
// max_tokens=20). Even with chat_template_kwargs.enable_thinking=false, some
// engines/templates still emit a few reasoning tokens before visible output,
// so a tiny cap can leave the visible text starved. The floor only raises a
// user-supplied cap; if MaxTokens is unset the caller has imposed no cap and
// there is nothing to floor (see client.go for the nil-guarded application
// site). Mirrors the OpenAI provider's 256-token floor (see
// pkg/model/provider/openai/client.go).
const noThinkingMinOutputTokens int64 = 256

// dmrParseResult bundles every piece of model-runner configuration that can be
// derived from a ModelConfig.  Returning a struct (rather than 5+ positional
// values) keeps the public surface ergonomic as we add more fields.
type dmrParseResult struct {
	contextSize     *int64
	runtimeFlags    []string
	rawRuntimeFlags string
	specOpts        *speculativeDecodingOpts
	llamaCpp        *llamaCppConfig
	vllm            *vllmConfig
	keepAlive       *string
	mode            *string
}

// parseDMRProviderOpts extracts DMR-specific provider options from the model
// config: context size, runtime flags, speculative decoding settings,
// backend-specific structured options, and top-level ConfigureRequest fields
// (mode, keep_alive, raw_runtime_flags).
//
// engine is the active model-runner backend (e.g. "llama.cpp", "vllm", "mlx",
// "sglang").
//
// Any validation error on a user-supplied field is returned so the caller can
// fail fast rather than round-tripping the server and reading the 4xx body.
func parseDMRProviderOpts(engine string, cfg *latest.ModelConfig) (dmrParseResult, error) {
	var res dmrParseResult
	if cfg == nil {
		return res, nil
	}

	res.contextSize = parseContextSize(cfg.ProviderOpts)

	if engine == "" || engine == engineLlamaCpp {
		res.llamaCpp = buildLlamaCppConfig(cfg)
	}

	if engine == engineVLLM {
		vllm, err := parseVLLMConfig(cfg.ProviderOpts)
		if err != nil {
			return res, err
		}
		res.vllm = vllm
	}

	ka, err := parseKeepAlive(cfg.ProviderOpts)
	if err != nil {
		return res, err
	}
	res.keepAlive = ka

	mode, err := parseMode(cfg.ProviderOpts)
	if err != nil {
		return res, err
	}
	res.mode = mode

	if raw, err := parseRawRuntimeFlags(cfg.ProviderOpts); err != nil {
		return res, err
	} else {
		res.rawRuntimeFlags = raw
	}

	slog.Debug("DMR provider opts", "provider_opts", cfg.ProviderOpts, "engine", engine)

	if len(cfg.ProviderOpts) == 0 {
		return res, nil
	}

	res.runtimeFlags = parseRuntimeFlags(cfg.ProviderOpts)
	res.specOpts = parseSpeculativeOpts(cfg.ProviderOpts)

	if len(res.runtimeFlags) > 0 && res.rawRuntimeFlags != "" {
		return res, errors.New("provider_opts: cannot set both runtime_flags and raw_runtime_flags; pick one")
	}

	return res, nil
}

// parseVLLMConfig extracts vLLM-specific configuration from provider_opts.
// Currently supports "gpu_memory_utilization" and "hf_overrides" keys.
// Returns nil when none of the keys are present or all values are invalid.
// hf_overrides is validated client-side with the same key rules model-runner
// enforces (see ../model-runner/pkg/inference/hf_overrides.go).
func parseVLLMConfig(opts map[string]any) (*vllmConfig, error) {
	if len(opts) == 0 {
		return nil, nil
	}

	var vllm *vllmConfig

	if gpuMem, ok := opts["gpu_memory_utilization"]; ok {
		if val, ok := parseFloat64(gpuMem); ok {
			if val < 0 || val > 1 {
				return nil, fmt.Errorf("provider_opts.gpu_memory_utilization must be between 0.0 and 1.0, got %v", val)
			}
			if vllm == nil {
				vllm = &vllmConfig{}
			}
			vllm.GPUMemoryUtilization = &val
		}
	}

	if hfOverrides, ok := opts["hf_overrides"]; ok {
		if overrides, ok := hfOverrides.(map[string]any); ok {
			if err := validateHFOverrides(overrides); err != nil {
				return nil, err
			}
			if vllm == nil {
				vllm = &vllmConfig{}
			}
			vllm.HFOverrides = overrides
		}
	}

	return vllm, nil
}

// validHFOverridesKeyRegex mirrors model-runner's regex: keys must be valid Go
// identifier-ish tokens to prevent injection via keys like "--malicious-flag".
var validHFOverridesKeyRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateHFOverrides mirrors inference.HFOverrides.Validate() from model-runner
// so the client can fail fast on bad input instead of waiting for a 400.
func validateHFOverrides(overrides map[string]any) error {
	for key, value := range overrides {
		if !validHFOverridesKeyRegex.MatchString(key) {
			return fmt.Errorf("invalid hf_overrides key %q: must contain only alphanumeric characters and underscores, and start with a letter or underscore", key)
		}
		if err := validateHFOverridesValue(key, value); err != nil {
			return err
		}
	}
	return nil
}

func validateHFOverridesValue(key string, value any) error {
	switch v := value.(type) {
	case string, bool, float64, float32, nil,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return nil
	case []any:
		for i, elem := range v {
			if err := validateHFOverridesValue(fmt.Sprintf("%s[%d]", key, i), elem); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for nestedKey, nestedValue := range v {
			if !validHFOverridesKeyRegex.MatchString(nestedKey) {
				return fmt.Errorf("invalid hf_overrides nested key %q in %q: must contain only alphanumeric characters and underscores, and start with a letter or underscore", nestedKey, key)
			}
			if err := validateHFOverridesValue(key+"."+nestedKey, nestedValue); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("invalid hf_overrides value for key %q: unsupported type %T", key, value)
	}
}

// parseKeepAlive extracts keep_alive from provider_opts and validates it using
// the same rules as model-runner's inference.ParseKeepAlive:
//   - Go duration strings: "5m", "1h", "30s"
//   - "0" to unload immediately
//   - Any negative value ("-1", "-1m") to keep loaded forever
//
// Returns nil when unset, letting model-runner use its default (5 minutes).
func parseKeepAlive(opts map[string]any) (*string, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	v, ok := opts["keep_alive"]
	if !ok {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf(`provider_opts.keep_alive must be a string (e.g. "5m", "1h", "-1"), got %T`, v)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("provider_opts.keep_alive must not be empty")
	}
	if err := validateKeepAlive(s); err != nil {
		return nil, err
	}
	return &s, nil
}

// validateKeepAlive enforces the same rules as model-runner's inference.ParseKeepAlive.
func validateKeepAlive(s string) error {
	if s == "0" || s == "-1" {
		return nil
	}
	if _, err := time.ParseDuration(s); err != nil {
		return fmt.Errorf("invalid keep_alive duration %q: %w", s, err)
	}
	return nil
}

// validModes mirrors the set accepted by model-runner's ParseBackendMode.
var validModes = map[string]struct{}{
	"completion":       {},
	"embedding":        {},
	"reranking":        {},
	"image-generation": {},
}

// parseMode extracts mode from provider_opts. When unset the scheduler auto-
// detects mode from the request path, so nil is the safe default.
func parseMode(opts map[string]any) (*string, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	v, ok := opts["mode"]
	if !ok {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("provider_opts.mode must be a string, got %T", v)
	}
	s = strings.TrimSpace(s)
	if _, ok := validModes[s]; !ok {
		return nil, fmt.Errorf("provider_opts.mode %q is invalid; must be one of: completion, embedding, reranking, image-generation", s)
	}
	return &s, nil
}

// parseRawRuntimeFlags extracts raw_runtime_flags as a single shell-style string.
// Model-runner parses this via shellwords; keep user validation minimal and
// reject empty/whitespace-only values.
func parseRawRuntimeFlags(opts map[string]any) (string, error) {
	if len(opts) == 0 {
		return "", nil
	}
	v, ok := opts["raw_runtime_flags"]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("provider_opts.raw_runtime_flags must be a string, got %T", v)
	}
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	return s, nil
}

// parseRuntimeFlags extracts the "runtime_flags" key from provider opts.
func parseRuntimeFlags(opts map[string]any) []string {
	v, ok := opts["runtime_flags"]
	if !ok {
		return nil
	}

	switch t := v.(type) {
	case []any:
		flags := make([]string, 0, len(t))
		for _, item := range t {
			flags = append(flags, fmt.Sprint(item))
		}
		return flags
	case []string:
		return append([]string(nil), t...)
	case string:
		return strings.Fields(strings.ReplaceAll(t, ",", " "))
	default:
		return nil
	}
}

// parseSpeculativeOpts extracts speculative decoding options from provider opts.
func parseSpeculativeOpts(opts map[string]any) *speculativeDecodingOpts {
	var so speculativeDecodingOpts
	var found bool

	if v, ok := opts["speculative_draft_model"]; ok {
		if s, ok := v.(string); ok && s != "" {
			so.draftModel = s
			found = true
		}
	}
	if v, ok := opts["speculative_num_tokens"]; ok {
		if n, ok := parseInt(v); ok {
			so.numTokens = n
			found = true
		}
	}
	if v, ok := opts["speculative_acceptance_rate"]; ok {
		if f, ok := parseFloat64(v); ok {
			so.acceptanceRate = f
			found = true
		}
	}

	if !found {
		return nil
	}
	return &so
}
