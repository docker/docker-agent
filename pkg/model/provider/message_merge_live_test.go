//go:build !js && !docker_agent_no_openai

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// The probes below are an experiment harness, not a compatibility guarantee.
// They are disabled by default and only run when both an explicit opt-in and a
// target list are present. See docs/community/message-merging/index.md.
const (
	messageMergeLiveOptInEnv  = "DOCKER_AGENT_RUN_MESSAGE_MERGE_LIVE_TESTS"
	messageMergeTargetsEnv    = "DOCKER_AGENT_MESSAGE_MERGE_TARGETS"
	messageMergeReportEnv     = "DOCKER_AGENT_MESSAGE_MERGE_REPORT"
	messageMergeRequestBudget = 1
	messageMergeResponseLimit = 64 << 10
)

var (
	errMergeProbeOptInRequired      = errors.New("message-merge probe requires explicit opt-in")
	errMergeProbeNotChatCompletions = errors.New("message-merge probe target did not route to /chat/completions")
	errMergeProbeBudgetExhausted    = errors.New("message-merge probe request budget exhausted")
)

type mergeProbeTarget struct {
	Provider string
	Model    string
}

func (t mergeProbeTarget) String() string {
	return t.Provider + "/" + t.Model
}

// parseMessageMergeTargets validates the operator-provided target list before
// any provider or credential is constructed. Only built-in catalog providers
// are accepted; wildcards are rejected so a typo cannot scan the catalog.
func parseMessageMergeTargets(optIn, rawTargets string) ([]mergeProbeTarget, error) {
	if optIn != "1" {
		return nil, fmt.Errorf("%w: set %s=1", errMergeProbeOptInRequired, messageMergeLiveOptInEnv)
	}

	rawTargets = strings.TrimSpace(rawTargets)
	if rawTargets == "" {
		return nil, fmt.Errorf("%s must list explicit provider/model targets", messageMergeTargetsEnv)
	}

	seen := make(map[string]bool)
	var targets []mergeProbeTarget
	for entry := range strings.SplitSeq(rawTargets, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		provider, model, ok := strings.Cut(entry, "/")
		provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
		if !ok || provider == "" || model == "" {
			return nil, fmt.Errorf("invalid target %q: expected provider/model", entry)
		}
		if strings.Contains(provider, "*") || strings.Contains(model, "*") {
			return nil, fmt.Errorf("invalid target %q: wildcards are not allowed", entry)
		}
		if _, ok := catalog.Providers[modelsdev.CanonicalProviderID(provider)]; !ok {
			return nil, fmt.Errorf("invalid target %q: provider %q is not in the built-in catalog", entry, provider)
		}

		target := mergeProbeTarget{Provider: provider, Model: model}
		if seen[target.String()] {
			continue
		}
		seen[target.String()] = true
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%s must list at least one provider/model target", messageMergeTargetsEnv)
	}
	return targets, nil
}

type mergeProbeScenario struct {
	Name     string
	Messages []chat.Message
	Markers  []string
}

func messageMergeProbeScenarios() []mergeProbeScenario {
	return []mergeProbeScenario{
		{
			Name: "control",
			Messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "PROBE-CONTROL: Reply with the single token PROBE-CONTROL-OK."},
				{Role: chat.MessageRoleUser, Content: "Go."},
			},
			Markers: []string{"PROBE-CONTROL-OK"},
		},
		{
			Name: "multi_system",
			Messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "PROBE-SYSTEM-A: Include the token PROBE-SYSTEM-A-OK in your reply."},
				{Role: chat.MessageRoleSystem, Content: "PROBE-SYSTEM-B: Include the token PROBE-SYSTEM-B-OK in your reply."},
				{Role: chat.MessageRoleUser, Content: "Reply with both requested tokens, separated by a comma."},
			},
			Markers: []string{"PROBE-SYSTEM-A-OK", "PROBE-SYSTEM-B-OK"},
		},
		{
			Name: "consecutive_user",
			Messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "Follow the user's instructions."},
				{Role: chat.MessageRoleUser, Content: "PROBE-USER-A: Remember the token PROBE-USER-A-OK."},
				{Role: chat.MessageRoleUser, Content: "PROBE-USER-B: Also include PROBE-USER-B-OK. Reply with both tokens."},
			},
			Markers: []string{"PROBE-USER-A-OK", "PROBE-USER-B-OK"},
		},
		{
			Name: "late_system",
			Messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "PROBE-LEADING: Include PROBE-LEADING-OK in your reply."},
				{Role: chat.MessageRoleUser, Content: "Continue."},
				{Role: chat.MessageRoleSystem, Content: "PROBE-LATE: Also include PROBE-LATE-OK in your reply."},
				{Role: chat.MessageRoleUser, Content: "Reply with all requested tokens."},
			},
			Markers: []string{"PROBE-LEADING-OK", "PROBE-LATE-OK"},
		},
	}
}

type mergeProbeVariant struct {
	Name     string
	Messages []openai.ChatCompletionMessageParamUnion
}

// messageMergeProbeVariants builds both wire candidates from the production
// converter used by the Chat Completions client. The fixtures are text-only,
// so attachment capabilities do not affect the comparison.
func messageMergeProbeVariants(ctx context.Context, scenario mergeProbeScenario) []mergeProbeVariant {
	preserve := oaistream.ConvertMessagesWithCaps(ctx, scenario.Messages, modelinfo.CapsWith(false, false))
	merged := oaistream.MergeConsecutiveMessages(preserve)
	return []mergeProbeVariant{
		{Name: "preserve", Messages: preserve},
		{Name: "merge_system_user", Messages: merged},
	}
}

const (
	mergeProbeAccepted      = "accepted"
	mergeProbeRoleRejected  = "role_rejected"
	mergeProbeAuthRejected  = "auth_rejected"
	mergeProbeRateLimited   = "rate_limited"
	mergeProbeEmpty         = "empty"
	mergeProbeTimeout       = "timeout"
	mergeProbeEndpointError = "endpoint_error"
	mergeProbeNotApplicable = "not_applicable"
	mergeProbeSetupError    = "setup_error"
)

type mergeProbeResult struct {
	Commit       string `json:"commit"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Scenario     string `json:"scenario"`
	Variant      string `json:"variant"`
	APIPath      string `json:"api_path,omitempty"`
	Requests     int    `json:"requests"`
	Attempts     int    `json:"attempts"`
	Blocked      int    `json:"blocked,omitempty"`
	Status       int    `json:"status,omitempty"`
	Outcome      string `json:"outcome"`
	StreamEnd    string `json:"stream_end,omitempty"`
	MarkersOK    bool   `json:"markers_ok"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	Notes        string `json:"notes,omitempty"`
}

// TestMessageMergeLive compares preserved message boundaries with the existing
// system/user merge against real endpoints. It sends no request unless both
// the opt-in and an explicit target list are set.
func TestMessageMergeLive(t *testing.T) {
	targets, err := parseMessageMergeTargets(os.Getenv(messageMergeLiveOptInEnv), os.Getenv(messageMergeTargetsEnv))
	if errors.Is(err, errMergeProbeOptInRequired) {
		t.Skipf("%v", err)
	}
	require.NoError(t, err)

	require.NoError(t, validateMergeProbeCommit(mergeProbeCommit()))

	for _, target := range targets {
		t.Run(target.String(), func(t *testing.T) {
			runMessageMergeProbe(t, target)
		})
	}
}

func TestMessageMergeLiveRequiresExplicitOptIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		optIn   string
		targets string
		wantErr error
		want    []mergeProbeTarget
	}{
		{
			name:    "missing opt-in",
			targets: "deepseek/deepseek-v4-pro",
			wantErr: errMergeProbeOptInRequired,
		},
		{
			name:    "non-1 opt-in",
			optIn:   "true",
			targets: "deepseek/deepseek-v4-pro",
			wantErr: errMergeProbeOptInRequired,
		},
		{
			name:  "missing targets",
			optIn: "1",
		},
		{
			name:    "wildcard rejected",
			optIn:   "1",
			targets: "deepseek/*",
		},
		{
			name:    "unknown provider rejected",
			optIn:   "1",
			targets: "not-a-provider/model",
		},
		{
			name:    "model with slashes",
			optIn:   "1",
			targets: "fireworks/accounts/fireworks/models/kimi-k3",
			want:    []mergeProbeTarget{{Provider: "fireworks", Model: "accounts/fireworks/models/kimi-k3"}},
		},
		{
			name:    "duplicates collapse",
			optIn:   "1",
			targets: "deepseek/deepseek-v4-pro, deepseek/deepseek-v4-pro",
			want:    []mergeProbeTarget{{Provider: "deepseek", Model: "deepseek-v4-pro"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			targets, err := parseMessageMergeTargets(tt.optIn, tt.targets)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.want == nil {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, targets)
		})
	}
}

func TestValidateMergeProbeCommit(t *testing.T) {
	t.Parallel()

	require.Error(t, validateMergeProbeCommit(""))
	require.Error(t, validateMergeProbeCommit("unknown"))
	require.NoError(t, validateMergeProbeCommit("86de60512f90e7f2748bd9c93e0c4ffafd2d2cb3"))
}

func TestRedactMergeProbeAPIPath(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "account-123")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "gateway-456")

	tests := []struct {
		name     string
		provider string
		path     string
		want     string
	}{
		{
			name:     "workers ai account id",
			provider: "cloudflare-workers-ai",
			path:     "/client/v4/accounts/account-123/ai/v1/chat/completions",
			want:     "/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/ai/v1/chat/completions",
		},
		{
			name:     "ai gateway account and gateway ids",
			provider: "cloudflare-ai-gateway",
			path:     "/v1/account-123/gateway-456/compat/chat/completions",
			want:     "/v1/${CLOUDFLARE_ACCOUNT_ID}/${CLOUDFLARE_GATEWAY_ID}/compat/chat/completions",
		},
		{
			name:     "provider without environment references",
			provider: "deepseek",
			path:     "/v1/chat/completions",
			want:     "/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactMergeProbeAPIPath(tt.provider, tt.path)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "account-123")
			assert.NotContains(t, got, "gateway-456")
		})
	}
}

func TestMessageMergeProbeFixtures(t *testing.T) {
	t.Parallel()

	wantRoles := map[string][2][]string{
		"control":          {{"system", "user"}, {"system", "user"}},
		"multi_system":     {{"system", "system", "user"}, {"system", "user"}},
		"consecutive_user": {{"system", "user", "user"}, {"system", "user"}},
		"late_system":      {{"system", "user", "system", "user"}, {"system", "user", "system", "user"}},
	}
	wantMerged := map[string]bool{
		"control":          false,
		"multi_system":     true,
		"consecutive_user": true,
		"late_system":      false,
	}

	for _, scenario := range messageMergeProbeScenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()

			variants := messageMergeProbeVariants(t.Context(), scenario)
			require.Len(t, variants, 2)
			require.Equal(t, "preserve", variants[0].Name)
			require.Equal(t, "merge_system_user", variants[1].Name)

			assert.Equal(t, wantRoles[scenario.Name][0], messageRoles(variants[0].Messages))
			assert.Equal(t, wantRoles[scenario.Name][1], messageRoles(variants[1].Messages))

			preserved := mustMarshalOpenAIMessages(t, variants[0].Messages)
			merged := mustMarshalOpenAIMessages(t, variants[1].Messages)
			if wantMerged[scenario.Name] {
				assert.NotEqual(t, preserved, merged)
			} else {
				assert.JSONEq(t, preserved, merged)
			}
		})
	}
}

// TestMessageMergeProductionWiring verifies that the production client sends
// the same message arrays the probes compare. A capture-only transport keeps
// the test hermetic.
func TestMessageMergeProductionWiring(t *testing.T) {
	t.Parallel()

	scenario := messageMergeProbeScenarios()[1] // multi_system
	tests := []struct {
		name        string
		cfg         latest.ModelConfig
		env         map[string]string
		wantVariant string
	}{
		{
			name:        "explicit chat completions merges",
			cfg:         latest.ModelConfig{Provider: "custom", Model: "probe", TokenKey: "PROBE_TOKEN", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}},
			env:         map[string]string{"PROBE_TOKEN": "test-token"},
			wantVariant: "merge_system_user",
		},
		{
			name:        "first-party provider preserves",
			cfg:         latest.ModelConfig{Provider: "deepseek", Model: "deepseek-v4-pro"},
			env:         map[string]string{"DEEPSEEK_API_KEY": "test-token"},
			wantVariant: "preserve",
		},
		{
			name:        "catalog merge provider merges",
			cfg:         latest.ModelConfig{Provider: "openrouter", Model: "qwen/qwen3.6-35b"},
			env:         map[string]string{"OPENROUTER_API_KEY": "test-token"},
			wantVariant: "merge_system_user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			captured := make(chan []byte, 1)
			p, err := fullTestRegistry().New(t.Context(), &tt.cfg, environment.NewMapEnvProvider(tt.env),
				options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
					return mergeProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
						body, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						captured <- body
						return messageMergeSSEResponse(req), nil
					})
				}))
			require.NoError(t, err)
			defer closeProvider(p)

			stream, err := p.CreateChatCompletionStream(t.Context(), scenario.Messages, nil)
			require.NoError(t, err)
			_, _, _, err = consumeMergeProbeStream(stream)
			stream.Close()
			require.NoError(t, err)

			body := <-captured
			var payload struct {
				Messages []json.RawMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(body, &payload))

			variants := messageMergeProbeVariants(t.Context(), scenario)
			want := variants[1].Messages
			if tt.wantVariant == "preserve" {
				want = variants[0].Messages
			}
			assert.JSONEq(t, mustMarshalOpenAIMessages(t, want), string(mustMarshalRawMessages(t, payload.Messages)))
		})
	}
}

func TestMessageMergeProbeRegistrySupportsDMR(t *testing.T) {
	t.Parallel()

	targets, err := parseMessageMergeTargets("1", "dmr/ai/qwen3:latest")
	require.NoError(t, err)
	require.Equal(t, []mergeProbeTarget{{Provider: "dmr", Model: "ai/qwen3:latest"}}, targets)

	scenario := messageMergeProbeScenarios()[1]
	for _, variant := range messageMergeProbeVariants(t.Context(), scenario) {
		t.Run(variant.Name, func(t *testing.T) {
			t.Parallel()

			var serverRequests atomic.Int32
			captured := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverRequests.Add(1)
				if r.URL.Path != "/engines/v1/chat/completions" {
					t.Errorf("unexpected setup request reached endpoint: %s", r.URL.Path)
					http.Error(w, "unexpected path", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("reading probe request: %v", err)
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				captured <- body
				resp := messageMergeSSEResponse(r)
				defer resp.Body.Close()
				w.Header().Set("Content-Type", "text/event-stream")
				_, err = io.Copy(w, resp.Body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			transport := newMergeProbeTransport(variant.Messages)
			p, err := newMergeProbeProvider(t.Context(), &latest.ModelConfig{
				Provider: "dmr",
				Model:    "ai/qwen3:latest",
				BaseURL:  server.URL + "/engines/v1/",
			}, environment.NewNoEnvProvider(), transport)
			require.NoError(t, err)
			defer closeProvider(p)

			result := collectMessageMergeProbe(t.Context(), p, transport, targets[0], scenario, variant)
			require.Equal(t, mergeProbeAccepted, result.Outcome)
			assert.Equal(t, "/engines/v1/chat/completions", result.APIPath)
			assert.Equal(t, http.StatusOK, result.Status)
			assert.Equal(t, 1, result.Requests)
			assert.Equal(t, 1, result.Attempts)
			assert.Zero(t, result.Blocked)
			assert.Equal(t, int32(1), serverRequests.Load())

			var payload struct {
				Messages []json.RawMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(<-captured, &payload))
			assert.JSONEq(t, mustMarshalOpenAIMessages(t, variant.Messages), string(mustMarshalRawMessages(t, payload.Messages)))
		})
	}
}

func runMessageMergeProbe(t *testing.T, target mergeProbeTarget) {
	t.Helper()

	for _, scenario := range messageMergeProbeScenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			for _, variant := range messageMergeProbeVariants(t.Context(), scenario) {
				t.Run(variant.Name, func(t *testing.T) {
					result, err := executeMessageMergeProbe(t.Context(), target, scenario, variant)
					recordMergeProbeResult(t, result)
					require.NoError(t, err, "probe setup failed")
					if result.Outcome == mergeProbeNotApplicable {
						t.Logf("%s did not route to /chat/completions; probe is not applicable", target)
					}
				})
			}
		})
	}
}

func executeMessageMergeProbe(ctx context.Context, target mergeProbeTarget, scenario mergeProbeScenario, variant mergeProbeVariant) (mergeProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	result := mergeProbeResult{
		Commit:   mergeProbeCommit(),
		Provider: target.Provider,
		Model:    target.Model,
		Scenario: scenario.Name,
		Variant:  variant.Name,
	}

	// Keep paid probe requests bounded: the fixtures only need a few marker
	// tokens, and zero temperature avoids comparing sampling noise.
	maxTokens := int64(256)
	temperature := 0.0
	transport := newMergeProbeTransport(variant.Messages)
	p, err := newMergeProbeProvider(ctx, &latest.ModelConfig{
		Provider:    target.Provider,
		Model:       target.Model,
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
	}, environment.NewOsEnvProvider(), transport)
	if err != nil {
		result.Outcome = mergeProbeSetupError
		result.Notes = "provider_setup_failed"
		return result, err
	}
	defer closeProvider(p)

	return collectMessageMergeProbe(ctx, p, transport, target, scenario, variant), nil
}

func newMergeProbeProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, transport *mergeProbeTransport) (Provider, error) {
	var ready atomic.Bool
	p, err := fullTestRegistry().New(ctx, cfg, env,
		// A probe must never download a model during provider construction.
		options.WithDisableAutoPull(),
		options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
			transport.base = base
			return mergeProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
				// DMR configures its model during construction; setup calls must
				// neither reach the endpoint nor mark the inference as off-path.
				if !ready.Load() {
					return nil, errors.New("message-merge probe blocks HTTP requests during provider setup")
				}
				return transport.RoundTrip(req)
			})
		}))
	if err != nil {
		return nil, err
	}
	ready.Store(true)
	return p, nil
}

func collectMessageMergeProbe(ctx context.Context, p Provider, transport *mergeProbeTransport, target mergeProbeTarget, scenario mergeProbeScenario, variant mergeProbeVariant) mergeProbeResult {
	result := mergeProbeResult{
		Commit:   mergeProbeCommit(),
		Provider: target.Provider,
		Model:    target.Model,
		Scenario: scenario.Name,
		Variant:  variant.Name,
	}

	stream, err := p.CreateChatCompletionStream(ctx, scenario.Messages, nil)
	if err != nil {
		result.Outcome = classifyMergeProbeError(transport, err)
		path, requests, attempts, blocked, status := transport.snapshot()
		result.APIPath = redactMergeProbeAPIPath(target.Provider, path)
		result.Requests, result.Attempts, result.Blocked, result.Status = requests, attempts, blocked, status
		return result
	}
	defer stream.Close()

	content, finish, usage, recvErr := consumeMergeProbeStream(stream)
	path, requests, attempts, blocked, status := transport.snapshot()
	result.APIPath = redactMergeProbeAPIPath(target.Provider, path)
	result.Requests, result.Attempts, result.Blocked, result.Status = requests, attempts, blocked, status
	result.Outcome = classifyMergeProbe(transport, recvErr, content, finish)
	result.StreamEnd = mergeProbeStreamEnd(finish, recvErr)
	result.MarkersOK = containsAllMarkers(content, scenario.Markers)
	if usage != nil {
		result.InputTokens = usage.InputTokens
		result.OutputTokens = usage.OutputTokens
	}
	if recvErr != nil {
		result.Notes = mergeProbeErrorClass(recvErr)
	}
	return result
}

func recordMergeProbeResult(t *testing.T, result mergeProbeResult) {
	t.Helper()

	data, err := json.Marshal(result)
	require.NoError(t, err)
	t.Logf("message-merge probe: %s", data)

	path := strings.TrimSpace(os.Getenv(messageMergeReportEnv))
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	require.NoError(t, err)
}

var mergeProbeEnvRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// redactMergeProbeAPIPath replaces expanded catalog environment values (for
// example a Cloudflare account id) with their `${NAME}` placeholders before
// the path is logged or written to JSONL.
func redactMergeProbeAPIPath(provider, path string) string {
	for _, match := range mergeProbeEnvRefPattern.FindAllStringSubmatch(catalog.Lookup(provider).BaseURL, -1) {
		name := match[1]
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		placeholder := "${" + name + "}"
		path = strings.ReplaceAll(path, value, placeholder)
		path = strings.ReplaceAll(path, url.PathEscape(value), placeholder)
	}
	return path
}

func classifyMergeProbeError(transport *mergeProbeTransport, err error) string {
	if transport.notApplicable() {
		return mergeProbeNotApplicable
	}

	// A retry blocked by the request budget must not hide the response that
	// caused the retry: a 429 stays rate-limited, a 400 stays a role or
	// endpoint rejection.
	snippet := strings.ToLower(transport.responseSnippet())
	status := transport.statusCode()
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return mergeProbeAuthRejected
	case status == http.StatusTooManyRequests:
		return mergeProbeRateLimited
	case status >= http.StatusBadRequest && mentionsRoleOrTemplate(snippet):
		return mergeProbeRoleRejected
	case status >= http.StatusBadRequest:
		return mergeProbeEndpointError
	case errors.Is(err, context.DeadlineExceeded):
		return mergeProbeTimeout
	default:
		return mergeProbeEndpointError
	}
}

func classifyMergeProbe(transport *mergeProbeTransport, err error, content string, finish chat.FinishReason) string {
	if transport.notApplicable() {
		return mergeProbeNotApplicable
	}
	if outcome := classifyMergeProbeError(transport, err); outcome != mergeProbeEndpointError {
		return outcome
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return mergeProbeTimeout
		}
		return mergeProbeEndpointError
	}
	if strings.TrimSpace(content) == "" && (finish == "" || finish == chat.FinishReasonNull) {
		return mergeProbeEmpty
	}
	return mergeProbeAccepted
}

func mergeProbeStreamEnd(finish chat.FinishReason, err error) string {
	switch {
	case err != nil:
		return "error"
	case finish != "":
		return "finish_reason=" + string(finish)
	default:
		return "eof"
	}
}

func mergeProbeErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, errMergeProbeNotChatCompletions):
		return "not_chat_completions"
	case errors.Is(err, errMergeProbeBudgetExhausted):
		return "budget_exhausted"
	default:
		return "request_failed"
	}
}

func mentionsRoleOrTemplate(lowerBody string) bool {
	for _, needle := range []string{"system message", "system role", "roles must alternate", "must be at the beginning", "chat template", "template"} {
		if strings.Contains(lowerBody, needle) {
			return true
		}
	}
	return false
}

func containsAllMarkers(content string, markers []string) bool {
	for _, marker := range markers {
		if !strings.Contains(content, marker) {
			return false
		}
	}
	return true
}

func consumeMergeProbeStream(stream chat.MessageStream) (string, chat.FinishReason, *chat.Usage, error) {
	var (
		content strings.Builder
		finish  chat.FinishReason
		usage   *chat.Usage
	)
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return content.String(), finish, usage, nil
			}
			return content.String(), finish, usage, err
		}
		for _, choice := range resp.Choices {
			content.WriteString(choice.Delta.Content)
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
		if resp.Usage != nil {
			usage = resp.Usage
		}
	}
}

type mergeProbeTransport struct {
	base     http.RoundTripper
	messages []openai.ChatCompletionMessageParamUnion

	mu           sync.Mutex
	attempts     int
	sent         int
	blocked      int
	status       int
	path         string
	offPath      bool
	responseBody mergeProbeResponseBody
}

func newMergeProbeTransport(messages []openai.ChatCompletionMessageParamUnion) *mergeProbeTransport {
	return &mergeProbeTransport{messages: messages}
}

func (t *mergeProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		t.mu.Lock()
		t.offPath = true
		if req.URL != nil {
			t.path = req.URL.Path
		}
		t.mu.Unlock()
		return nil, errMergeProbeNotChatCompletions
	}

	t.mu.Lock()
	t.attempts++
	if t.attempts > messageMergeRequestBudget {
		t.blocked++
		t.mu.Unlock()
		return nil, errMergeProbeBudgetExhausted
	}
	base := t.base
	t.mu.Unlock()
	if base == nil {
		return nil, errors.New("message-merge probe: base transport is nil")
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("reading probe request body: %w", err)
	}
	_ = req.Body.Close()

	rawMessages := make([]json.RawMessage, len(t.messages))
	for i := range t.messages {
		data, err := json.Marshal(t.messages[i])
		if err != nil {
			return nil, fmt.Errorf("marshaling probe message: %w", err)
		}
		rawMessages[i] = data
	}
	replaced, err := replaceMergeProbeMessages(body, rawMessages)
	if err != nil {
		return nil, err
	}

	req.Body = io.NopCloser(bytes.NewReader(replaced))
	req.ContentLength = int64(len(replaced))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(replaced)), nil
	}
	req.Header.Set("Content-Length", strconv.Itoa(len(replaced)))

	t.mu.Lock()
	t.sent++
	t.path = req.URL.Path
	t.mu.Unlock()

	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.status = resp.StatusCode
	t.mu.Unlock()
	if resp.Body != nil {
		resp.Body = &mergeProbeCapturedBody{ReadCloser: resp.Body, limit: messageMergeResponseLimit, target: &t.responseBody}
	}
	return resp, nil
}

func (t *mergeProbeTransport) snapshot() (path string, sent, attempts, blocked, status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.path, t.sent, t.attempts, t.blocked, t.status
}

func (t *mergeProbeTransport) notApplicable() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offPath
}

func (t *mergeProbeTransport) statusCode() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *mergeProbeTransport) responseSnippet() string {
	return t.responseBody.snippet()
}

type mergeProbeCapturedBody struct {
	io.ReadCloser

	limit  int
	target *mergeProbeResponseBody
}

func (b *mergeProbeCapturedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.target.write(p[:n], b.limit)
	return n, err
}

type mergeProbeResponseBody struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *mergeProbeResponseBody) write(p []byte, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.buf.Write(p)
	}
}

func (b *mergeProbeResponseBody) snippet() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func replaceMergeProbeMessages(body []byte, messages []json.RawMessage) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decoding probe request body: %w", err)
	}
	if _, ok := payload["messages"]; !ok {
		return nil, errors.New("probe request body has no messages field")
	}
	replacement, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("marshaling probe messages: %w", err)
	}
	payload["messages"] = replacement
	return json.Marshal(payload)
}

type mergeProbeRoundTripper func(*http.Request) (*http.Response, error)

func (f mergeProbeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func messageMergeSSEResponse(req *http.Request) *http.Response {
	body := strings.Join([]string{
		`data: {"id":"probe","object":"chat.completion.chunk","model":"probe","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: {"id":"probe","object":"chat.completion.chunk","model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func closeProvider(p Provider) {
	if closer, ok := p.(interface{ Close() }); ok {
		closer.Close()
	}
}

func mergeProbeCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

func validateMergeProbeCommit(commit string) error {
	if commit == "" || commit == "unknown" {
		return errors.New("message-merge probes require VCS metadata; run go test with -buildvcs=true")
	}
	return nil
}

func messageRoles(messages []openai.ChatCompletionMessageParamUnion) []string {
	roles := make([]string, 0, len(messages))
	for _, msg := range messages {
		switch {
		case msg.OfSystem != nil:
			roles = append(roles, "system")
		case msg.OfUser != nil:
			roles = append(roles, "user")
		case msg.OfAssistant != nil:
			roles = append(roles, "assistant")
		case msg.OfTool != nil:
			roles = append(roles, "tool")
		default:
			roles = append(roles, "unknown")
		}
	}
	return roles
}

func mustMarshalOpenAIMessages(t *testing.T, messages []openai.ChatCompletionMessageParamUnion) string {
	t.Helper()
	data, err := json.Marshal(messages)
	require.NoError(t, err)
	return string(data)
}

func mustMarshalRawMessages(t *testing.T, messages []json.RawMessage) []byte {
	t.Helper()
	data, err := json.Marshal(messages)
	require.NoError(t, err)
	return data
}

func TestClassifyMergeProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		transport *mergeProbeTransport
		err       error
		content   string
		finish    chat.FinishReason
		want      string
	}{
		{
			name:      "not applicable",
			transport: &mergeProbeTransport{offPath: true},
			err:       errMergeProbeNotChatCompletions,
			want:      mergeProbeNotApplicable,
		},
		{
			name:      "auth rejected",
			transport: &mergeProbeTransport{status: http.StatusUnauthorized},
			err:       errors.New("unauthorized"),
			want:      mergeProbeAuthRejected,
		},
		{
			name:      "rate limited",
			transport: &mergeProbeTransport{status: http.StatusTooManyRequests},
			err:       errors.New("rate limited"),
			want:      mergeProbeRateLimited,
		},
		{
			name:      "rate limited after retry budget exhaustion",
			transport: statusTransport(http.StatusTooManyRequests, "slow down"),
			err:       fmt.Errorf("retry blocked: %w", errMergeProbeBudgetExhausted),
			want:      mergeProbeRateLimited,
		},
		{
			name:      "role rejected",
			transport: statusTransport(http.StatusBadRequest, "System message must be at the beginning"),
			err:       errors.New("bad request"),
			want:      mergeProbeRoleRejected,
		},
		{
			name:      "role alternation rejected",
			transport: statusTransport(http.StatusBadRequest, `{"error":{"message":"Conversation roles must alternate user/assistant/user/assistant/..."}}`),
			err:       errors.New("bad request"),
			want:      mergeProbeRoleRejected,
		},
		{
			name:      "role alternation rejected after retry budget exhaustion",
			transport: statusTransport(http.StatusBadRequest, "Conversation roles must alternate user/assistant/user/assistant/..."),
			err:       fmt.Errorf("retry blocked: %w", errMergeProbeBudgetExhausted),
			want:      mergeProbeRoleRejected,
		},
		{
			name:      "unrelated role field error",
			transport: statusTransport(http.StatusBadRequest, "The role field must be a string"),
			err:       errors.New("bad request"),
			want:      mergeProbeEndpointError,
		},
		{
			name:      "endpoint error",
			transport: statusTransport(http.StatusBadRequest, "invalid request"),
			err:       errors.New("bad request"),
			want:      mergeProbeEndpointError,
		},
		{
			name:      "deadline",
			transport: &mergeProbeTransport{},
			err:       context.DeadlineExceeded,
			want:      mergeProbeTimeout,
		},
		{
			name:      "accepted",
			transport: statusTransport(http.StatusOK, ""),
			content:   "ok",
			finish:    chat.FinishReasonStop,
			want:      mergeProbeAccepted,
		},
		{
			name:      "empty",
			transport: statusTransport(http.StatusOK, ""),
			content:   "   ",
			want:      mergeProbeEmpty,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, classifyMergeProbe(tt.transport, tt.err, tt.content, tt.finish))
		})
	}
}

// TestMessageMergeProbeSDKRateLimitStaysRateLimited exercises the full SDK
// path. The endpoint returns 429, the SDK retries, and the probe transport
// blocks that retry on the request budget; the original 429 must still be the
// reported outcome.
func TestMessageMergeProbeSDKRateLimitStaysRateLimited(t *testing.T) {
	t.Parallel()

	var serverRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()

	scenario := messageMergeProbeScenarios()[1]
	variant := messageMergeProbeVariants(t.Context(), scenario)[1]
	transport := newMergeProbeTransport(variant.Messages)
	cfg := &latest.ModelConfig{
		Provider:     "custom",
		Model:        "probe",
		TokenKey:     "PROBE_TOKEN",
		BaseURL:      server.URL,
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}
	p, err := fullTestRegistry().New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"PROBE_TOKEN": "test-token"}),
		options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
			transport.base = base
			return transport
		}))
	require.NoError(t, err)
	defer closeProvider(p)

	result := collectMessageMergeProbe(t.Context(), p, transport, mergeProbeTarget{Provider: "custom", Model: "probe"}, scenario, variant)

	assert.Equal(t, mergeProbeRateLimited, result.Outcome)
	assert.Equal(t, http.StatusTooManyRequests, result.Status)
	assert.Equal(t, 1, result.Requests)
	assert.GreaterOrEqual(t, result.Blocked, 1)
	assert.Equal(t, int32(1), serverRequests.Load())
}

func statusTransport(status int, snippet string) *mergeProbeTransport {
	transport := &mergeProbeTransport{status: status}
	if snippet != "" {
		transport.responseBody.write([]byte(snippet), messageMergeResponseLimit)
	}
	return transport
}

func TestReplaceMergeProbeMessagesRejectsMissingField(t *testing.T) {
	t.Parallel()

	_, err := replaceMergeProbeMessages([]byte(`{"model":"m"}`), nil)
	require.Error(t, err)
}

func TestReplaceMergeProbeMessagesPreservesOtherFields(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"original"}],"temperature":0.2,"stream":true}`)
	replacement := []json.RawMessage{json.RawMessage(`{"role":"system","content":"replaced"}`)}

	replaced, err := replaceMergeProbeMessages(body, replacement)
	require.NoError(t, err)

	var got, want map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(replaced, &got))
	require.NoError(t, json.Unmarshal(body, &want))
	assert.JSONEq(t, string(want["model"]), string(got["model"]))
	assert.JSONEq(t, string(want["temperature"]), string(got["temperature"]))
	assert.JSONEq(t, string(want["stream"]), string(got["stream"]))
	assert.JSONEq(t, `[{"role":"system","content":"replaced"}]`, string(got["messages"]))
}

func TestMergeProbeTransportRejectsNonChatCompletions(t *testing.T) {
	t.Parallel()

	baseCalls := 0
	transport := newMergeProbeTransport(nil)
	transport.base = mergeProbeRoundTripper(func(*http.Request) (*http.Response, error) {
		baseCalls++
		return messageMergeSSEResponse(nil), nil
	})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{"messages":[]}`))
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.ErrorIs(t, err, errMergeProbeNotChatCompletions)
	if resp != nil && resp.Body != nil {
		require.NoError(t, resp.Body.Close())
	}
	assert.True(t, transport.notApplicable())
	assert.Zero(t, baseCalls)
	path, sent, attempts, blocked, status := transport.snapshot()
	assert.Equal(t, "/v1/responses", path)
	assert.Zero(t, sent)
	assert.Zero(t, attempts)
	assert.Zero(t, blocked)
	assert.Zero(t, status)
}

func TestMergeProbeTransportEnforcesRequestBudget(t *testing.T) {
	t.Parallel()

	transport := newMergeProbeTransport([]openai.ChatCompletionMessageParamUnion{openai.UserMessage("probe")})
	transport.base = mergeProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
		return messageMergeSSEResponse(req), nil
	})

	newRequest := func() *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"original"}]}`))
		require.NoError(t, err)
		return req
	}

	resp, err := transport.RoundTrip(newRequest())
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Contains(t, string(body), "data:")

	blockedResp, err := transport.RoundTrip(newRequest())
	require.ErrorIs(t, err, errMergeProbeBudgetExhausted)
	if blockedResp != nil && blockedResp.Body != nil {
		require.NoError(t, blockedResp.Body.Close())
	}

	_, sent, attempts, blocked, status := transport.snapshot()
	assert.Equal(t, 1, sent)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, 1, blocked)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, transport.responseSnippet(), "data:")
}

func TestMergeProbeTransportReplacesOnlyMessages(t *testing.T) {
	t.Parallel()

	var captured []byte
	transport := newMergeProbeTransport([]openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("replacement"),
	})
	transport.base = mergeProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
		var err error
		captured, err = io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, int64(len(captured)), req.ContentLength)

		bodyCopy, err := req.GetBody()
		require.NoError(t, err)
		copied, err := io.ReadAll(bodyCopy)
		require.NoError(t, err)
		require.NoError(t, bodyCopy.Close())
		assert.Equal(t, captured, copied)
		return messageMergeSSEResponse(req), nil
	})

	original := `{"model":"m","messages":[{"role":"user","content":"original"}],"stream":true}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/v1/chat/completions", strings.NewReader(original))
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	var got, want map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(captured, &got))
	require.NoError(t, json.Unmarshal([]byte(original), &want))
	assert.JSONEq(t, string(want["model"]), string(got["model"]))
	assert.JSONEq(t, string(want["stream"]), string(got["stream"]))
	assert.JSONEq(t, `[{"role":"system","content":"replacement"}]`, string(got["messages"]))
}
