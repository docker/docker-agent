package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

type environmentFunc func(context.Context, string) (string, bool)

func (f environmentFunc) Get(ctx context.Context, name string) (string, bool) {
	return f(ctx, name)
}

func testConfig(kind string) latest.EvaluatorConfig {
	cfg := latest.EvaluatorConfig{
		Provider:     "typesafe",
		Model:        "jev-test",
		Type:         kind,
		Instructions: "Assess the state.",
	}
	switch kind {
	case "choice":
		cfg.Choices = map[string]string{"safe": "Safe to proceed", "unsafe": "Do not proceed"}
	case "score":
		cfg.Levels = []string{"Low", "Medium", "High"}
	}
	return cfg
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	env := environmentFunc(func(context.Context, string) (string, bool) {
		t.Error("credentials must not be resolved when the client is constructed")
		return "", false
	})
	client, err := New(t.Context(), testConfig("boolean"), env)
	require.NoError(t, err)
	p := client.(*typesafe)
	assert.Equal(t, "https://api.typesafe.ai/v1/systemone", p.endpoint)
	assert.Equal(t, 10*time.Second, p.timeout)
	assert.Equal(t, "TYPESAFE_API_KEY", p.tokenKey)
	assert.ErrorIs(t, p.client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
}

func TestNewCustomSettings(t *testing.T) {
	t.Parallel()

	cfg := testConfig("score")
	cfg.BaseURL = "https://example.invalid/proxy/"
	cfg.TokenKey = "CUSTOM_TOKEN"
	cfg.Timeout.Duration = 2 * time.Second
	client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	p := client.(*typesafe)
	assert.Equal(t, "https://example.invalid/proxy/v1/systemone", p.endpoint)
	assert.Equal(t, 2*time.Second, p.timeout)
	assert.Equal(t, "CUSTOM_TOKEN", p.tokenKey)
	assert.Equal(t, []string{"0", "1", "2"}, p.probabilityKeys)
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*latest.EvaluatorConfig){
		"unknown provider":     func(c *latest.EvaluatorConfig) { c.Provider = "other" },
		"unresolved provider":  func(c *latest.EvaluatorConfig) { c.Provider = "custom-typesafe" },
		"missing model":        func(c *latest.EvaluatorConfig) { c.Model = "" },
		"whitespace model":     func(c *latest.EvaluatorConfig) { c.Model = " " },
		"invalid type":         func(c *latest.EvaluatorConfig) { c.Type = "noul" },
		"missing instructions": func(c *latest.EvaluatorConfig) { c.Instructions = "" },
		"negative timeout":     func(c *latest.EvaluatorConfig) { c.Timeout.Duration = -time.Second },
		"missing choices":      func(c *latest.EvaluatorConfig) { c.Type = "choice" },
		"missing levels":       func(c *latest.EvaluatorConfig) { c.Type = "score" },
		"negative cost":        func(c *latest.EvaluatorConfig) { c.Cost = &latest.CostConfig{Input: -1} },
		"NaN cost":             func(c *latest.EvaluatorConfig) { c.Cost = &latest.CostConfig{Input: math.NaN()} },
		"infinite cost":        func(c *latest.EvaluatorConfig) { c.Cost = &latest.CostConfig{Output: math.Inf(1)} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig("boolean")
			mutate(&cfg)
			client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
			require.Error(t, err)
			assert.Nil(t, client)
		})
	}
}

func TestNewRejectsInvalidBaseURL(t *testing.T) {
	t.Parallel()

	for _, baseURL := range []string{
		"relative/path", "ftp://example.invalid", "https://", "https://user:private-token@example.invalid",
		"https://example.invalid?token=private-token", "https://example.invalid#private-token", "http://%",
	} {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig("boolean")
			cfg.BaseURL = baseURL
			client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-token")
			assert.Nil(t, client)
		})
	}
}

func TestNewRejectsMissingEnvironment(t *testing.T) {
	t.Parallel()

	client, err := New(t.Context(), testConfig("boolean"), nil)
	require.Error(t, err)
	assert.Nil(t, client)
}

func TestEvaluateMissingKey(t *testing.T) {
	t.Parallel()

	for _, token := range []string{"", "   "} {
		t.Run("empty-token="+token, func(t *testing.T) {
			t.Parallel()
			client, err := New(t.Context(), testConfig("boolean"), environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": token}))
			require.NoError(t, err)
			ctx := evaluator.WithUsageObserver(t.Context(), func(evaluator.UsageRecord) {
				t.Error("request without a response reported usage")
			})
			result, err := client.Evaluate(ctx, "state")
			require.ErrorContains(t, err, "API key is missing")
			assert.Nil(t, result)
		})
	}
	client, err := New(t.Context(), testConfig("boolean"), environment.NewNoEnvProvider())
	require.NoError(t, err)
	result, err := client.Evaluate(t.Context(), "state")
	require.ErrorContains(t, err, "API key is missing")
	assert.Nil(t, result)
}

func TestEvaluateCanceledBeforeLookup(t *testing.T) {
	t.Parallel()

	client, err := New(t.Context(), testConfig("boolean"), environmentFunc(func(context.Context, string) (string, bool) {
		t.Error("canceled evaluation looked up credentials")
		return "", false
	}))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := client.Evaluate(ctx, "state")
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)
}

func TestEvaluateTimeoutIncludesKeyLookup(t *testing.T) {
	t.Parallel()

	cfg := testConfig("boolean")
	cfg.Timeout.Duration = time.Millisecond
	client, err := New(t.Context(), cfg, environmentFunc(func(ctx context.Context, _ string) (string, bool) {
		<-ctx.Done()
		return "", false
	}))
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		result, err := client.Evaluate(t.Context(), "state")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, result)
	})
}

type invalidState struct{}

func (invalidState) MarshalJSON() ([]byte, error) {
	return nil, errors.New("private-state")
}

func TestEvaluateRejectsInvalidState(t *testing.T) {
	t.Parallel()

	for name, state := range map[string]any{
		"nil": nil, "nil map": map[string]string(nil), "nil slice": []any(nil),
		"number": 1, "boolean": true, "channel": make(chan int), "marshal error": invalidState{},
		"NaN": math.NaN(), "infinity": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, err := New(t.Context(), testConfig("boolean"), environmentFunc(func(context.Context, string) (string, bool) {
				t.Error("invalid state looked up credentials")
				return "", false
			}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), state)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-state")
			assert.Nil(t, result)
		})
	}
}
