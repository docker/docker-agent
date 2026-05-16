package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// TestConfigureTaskBudget_NoBudget verifies that nil / zero budgets don't
// touch the request params.
func TestConfigureTaskBudget_NoBudget(t *testing.T) {
	t.Parallel()

	for name, tb := range map[string]*latest.TaskBudget{
		"nil":  nil,
		"zero": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			params := anthropic.BetaMessageNewParams{}
			configureTaskBudget(&params, tb)

			assert.Empty(t, params.Betas)
			data, err := json.Marshal(params.OutputConfig)
			require.NoError(t, err)
			assert.NotContains(t, string(data), "task_budget")
		})
	}
}

// TestConfigureTaskBudget_AttachesPayloadAndBeta verifies the happy path:
// the beta header is appended and `task_budget` is serialized under
// `output_config`, coexisting with existing fields like `effort`.
func TestConfigureTaskBudget_AttachesPayloadAndBeta(t *testing.T) {
	t.Parallel()

	params := anthropic.BetaMessageNewParams{
		Betas:        []anthropic.AnthropicBeta{"existing"},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortHigh},
	}
	configureTaskBudget(&params, &latest.TaskBudget{Total: 128000})

	assert.Equal(t,
		[]anthropic.AnthropicBeta{"existing", taskBudgetBeta},
		params.Betas)

	var out map[string]any
	data, err := json.Marshal(params.OutputConfig)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &out))

	assert.Equal(t, "high", out["effort"])
	tb, ok := out["task_budget"].(map[string]any)
	require.True(t, ok, "expected task_budget object, got %s", string(data))
	assert.Equal(t, "tokens", tb["type"])
	assert.InDelta(t, 128000, tb["total"], 0.001)
}

// anthropicTestServer returns a minimal stub that captures request metadata
// and replies with an empty SSE stream so the client can terminate cleanly.
func anthropicTestServer(t *testing.T, capture func(*http.Request, []byte)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture(r, body)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// drain fully consumes a stream so the underlying HTTP handler runs to completion.
func drain(stream chat.MessageStream) {
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	stream.Close()
}

// newTestClient builds an anthropic Client wired to a test HTTP server.
func newTestClient(srv *httptest.Server, cfg latest.ModelConfig) *Client {
	return &Client{
		Config: base.Config{ModelConfig: cfg},
		clientFn: func(_ context.Context) (anthropic.Client, error) {
			return anthropic.NewClient(
				option.WithAPIKey("test-key"),
				option.WithBaseURL(srv.URL),
			), nil
		},
	}
}

// TestTaskBudget_RoutesToBetaAPIWithBetaHeader verifies end-to-end that
// configuring `task_budget`:
//  1. routes the request through the Beta Messages API, and
//  2. attaches the `task-budgets-2026-03-13` beta header, and
//  3. serializes `output_config.task_budget` in the request body.
func TestTaskBudget_RoutesToBetaAPIWithBetaHeader(t *testing.T) {
	var (
		gotPath  string
		gotBetas []string
		gotBody  []byte
	)
	srv := anthropicTestServer(t, func(r *http.Request, body []byte) {
		gotPath = r.URL.Path
		// The SDK emits one `anthropic-beta` header per beta via
		// WithHeaderAdd, so use Values (not Get) to see every entry.
		gotBetas = r.Header.Values("anthropic-beta")
		gotBody = body
	})

	client := newTestClient(srv, latest.ModelConfig{
		Provider:   "anthropic",
		Model:      "claude-opus-4-7",
		TaskBudget: &latest.TaskBudget{Total: 128000},
	})

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}},
		nil,
	)
	require.NoError(t, err)
	drain(stream)

	assert.Equal(t, "/v1/messages", gotPath, "expected Beta messages path")
	assert.Contains(t, gotBetas, taskBudgetBeta,
		"anthropic-beta headers must contain %s; got %v", taskBudgetBeta, gotBetas)

	var body map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &body), "raw body: %s", string(gotBody))
	outputCfg, ok := body["output_config"].(map[string]any)
	require.True(t, ok, "expected output_config in body, got %s", string(gotBody))
	tb, ok := outputCfg["task_budget"].(map[string]any)
	require.True(t, ok, "expected task_budget in output_config, got %s", string(gotBody))
	assert.Equal(t, "tokens", tb["type"])
	assert.InDelta(t, 128000, tb["total"], 0.001)
}

// TestNoTaskBudget_UsesStandardPath sanity-checks that without task_budget
// (and no other beta-only features) the request targets /v1/messages and
// does not carry the task-budgets beta header.
func TestNoTaskBudget_UsesStandardPath(t *testing.T) {
	var (
		gotPath  string
		gotBetas []string
	)
	srv := anthropicTestServer(t, func(r *http.Request, _ []byte) {
		gotPath = r.URL.Path
		gotBetas = r.Header.Values("anthropic-beta")
	})

	client := newTestClient(srv, latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-sonnet-4-5-20250929",
	})

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}},
		nil,
	)
	require.NoError(t, err)
	drain(stream)

	assert.Equal(t, "/v1/messages", gotPath)
	assert.NotContains(t, gotBetas, taskBudgetBeta)
}

// TestZeroTaskBudget_DisablesFeature verifies that `task_budget: 0`
// (shorthand for "disabled") does NOT route through the Beta path and does
// NOT emit the task-budgets beta header, matching the documented behavior.
func TestZeroTaskBudget_DisablesFeature(t *testing.T) {
	var gotBetas []string
	var gotBody []byte
	srv := anthropicTestServer(t, func(r *http.Request, body []byte) {
		gotBetas = r.Header.Values("anthropic-beta")
		gotBody = body
	})

	// Simulate what UnmarshalYAML produces for `task_budget: 0`.
	client := newTestClient(srv, latest.ModelConfig{
		Provider:   "anthropic",
		Model:      "claude-sonnet-4-5-20250929",
		TaskBudget: &latest.TaskBudget{Type: "tokens", Total: 0},
	})

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}},
		nil,
	)
	require.NoError(t, err)
	drain(stream)

	assert.NotContains(t, gotBetas, taskBudgetBeta,
		"task_budget: 0 must not emit the task-budgets beta header")
	assert.NotContains(t, string(gotBody), "task_budget",
		"task_budget: 0 must not serialize task_budget in the request body")
}
