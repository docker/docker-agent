package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
)

// batchEmbedRequest mirrors the Gemini Developer API batchEmbedContents body.
type batchEmbedRequest struct {
	Requests []struct {
		Model   string `json:"model"`
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		TaskType             *string `json:"taskType"`
		OutputDimensionality *int    `json:"outputDimensionality"`
	} `json:"requests"`
}

func newEmbedClient(t *testing.T, baseURL string, providerOpts map[string]any, opts ...options.Opt) *Client {
	t.Helper()
	return newEmbedClientForModel(t, "gemini-embedding-001", baseURL, providerOpts, opts...)
}

func newEmbedClientForModel(t *testing.T, model, baseURL string, providerOpts map[string]any, opts ...options.Opt) *Client {
	t.Helper()
	cfg := &latest.ModelConfig{
		Provider:     "google",
		Model:        model,
		BaseURL:      baseURL,
		ProviderOpts: providerOpts,
	}
	env := environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_API_KEY":                  "test-key",
		environment.DockerDesktopTokenEnv: "test-dd-token",
	})
	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)
	return client
}

// An explicit token source avoids ADC discovery, including on WASM.
func newVertexEmbedClient(t *testing.T, model, baseURL string, providerOpts map[string]any) *Client {
	t.Helper()
	cfg := &latest.ModelConfig{
		Provider:     "google",
		Model:        model,
		BaseURL:      baseURL,
		ProviderOpts: providerOpts,
	}
	env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1"})
	client, err := NewClient(t.Context(), cfg, env,
		options.WithTokenSource(func(context.Context) (string, error) { return "test-token", nil }))
	require.NoError(t, err)
	require.Equal(t, apiSurfaceVertexAI, client.apiSurface)
	return client
}

// recordingServer captures every request path and JSON body, replying with
// responses[i] to the i-th request.
func recordingServer(t *testing.T, responses ...string) (*httptest.Server, *[]string, *[]map[string]any) {
	t.Helper()
	var (
		mu     sync.Mutex
		paths  []string
		bodies []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		n := len(paths)
		mu.Unlock()
		if !assert.LessOrEqual(t, n, len(responses), "unexpected extra request") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[n-1])
	}))
	t.Cleanup(srv.Close)
	return srv, &paths, &bodies
}

func embedServer(t *testing.T, status int, body string, captured *batchEmbedRequest, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1beta/models/gemini-embedding-001:batchEmbedContents", r.URL.Path)
		if captured != nil {
			raw, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.NoError(t, json.Unmarshal(raw, captured))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateBatchEmbedding_WireShapeAndOrder(t *testing.T) {
	t.Parallel()

	for _, gateway := range []bool{false, true} {
		t.Run(fmt.Sprintf("gateway=%t", gateway), func(t *testing.T) {
			t.Parallel()

			var captured batchEmbedRequest
			srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1,0.2,0.3]},{"values":[0.4,0.5,0.6]}]}`, &captured, nil)
			var client *Client
			if gateway {
				client = newEmbedClient(t, "", nil, options.WithGateway(srv.URL))
			} else {
				client = newEmbedClient(t, srv.URL, nil)
			}

			result, err := client.CreateBatchEmbedding(t.Context(), []string{"first", "second"})
			require.NoError(t, err)

			require.Len(t, captured.Requests, 2)
			assert.Equal(t, "models/gemini-embedding-001", captured.Requests[0].Model)
			assert.Equal(t, "first", captured.Requests[0].Content.Parts[0].Text)
			assert.Equal(t, "second", captured.Requests[1].Content.Parts[0].Text)
			for _, req := range captured.Requests {
				assert.Nil(t, req.TaskType, "task_type must not be sent by default")
				assert.Nil(t, req.OutputDimensionality)
			}

			require.Len(t, result.Embeddings, 2)
			assert.InDeltaSlice(t, []float64{0.1, 0.2, 0.3}, result.Embeddings[0], 1e-6)
			assert.InDeltaSlice(t, []float64{0.4, 0.5, 0.6}, result.Embeddings[1], 1e-6)
			assert.Zero(t, result.InputTokens, "Gemini Developer API reports no embedding usage")
			assert.Zero(t, result.TotalTokens)
			assert.Zero(t, result.Cost)
		})
	}
}

func TestCreateBatchEmbedding_OutputDimensionality(t *testing.T) {
	t.Parallel()

	var captured batchEmbedRequest
	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1,0.2]}]}`, &captured, nil)
	client := newEmbedClient(t, srv.URL, map[string]any{"output_dimensionality": 768})

	_, err := client.CreateBatchEmbedding(t.Context(), []string{"text"})
	require.NoError(t, err)

	require.Len(t, captured.Requests, 1)
	require.NotNil(t, captured.Requests[0].OutputDimensionality)
	assert.Equal(t, 768, *captured.Requests[0].OutputDimensionality)
}

func TestCreateBatchEmbedding_InvalidOutputDimensionality(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]any{"zero": 0, "negative": -1, "string": "768", "float": 1.5} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			srv := embedServer(t, http.StatusOK, `{}`, nil, &calls)
			client := newEmbedClient(t, srv.URL, map[string]any{"output_dimensionality": value})

			_, err := client.CreateBatchEmbedding(t.Context(), []string{"text"})
			require.ErrorContains(t, err, "output_dimensionality")
			assert.Zero(t, calls.Load(), "invalid config must fail before any request")
		})
	}
}

func TestCreateBatchEmbedding_UsageFromStatistics(t *testing.T) {
	t.Parallel()

	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1],"statistics":{"tokenCount":3}},{"values":[0.2],"statistics":{"tokenCount":4}}],"metadata":{"billableCharacterCount":42}}`, nil, nil)
	client := newEmbedClient(t, srv.URL, nil)

	result, err := client.CreateBatchEmbedding(t.Context(), []string{"a", "b"})
	require.NoError(t, err)
	assert.Equal(t, int64(7), result.InputTokens)
	assert.Equal(t, int64(7), result.TotalTokens)
}

func TestCreateEmbedding_ReturnsFirstVector(t *testing.T) {
	t.Parallel()

	var captured batchEmbedRequest
	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[1,2]}]}`, &captured, nil)
	client := newEmbedClient(t, srv.URL, nil)

	result, err := client.CreateEmbedding(t.Context(), "hello")
	require.NoError(t, err)
	require.Len(t, captured.Requests, 1)
	assert.Equal(t, "hello", captured.Requests[0].Content.Parts[0].Text)
	assert.Equal(t, []float64{1, 2}, result.Embedding)
}

func TestCreateBatchEmbedding_EmptyInputSkipsRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := embedServer(t, http.StatusOK, `{}`, nil, &calls)
	client := newEmbedClient(t, srv.URL, nil)

	result, err := client.CreateBatchEmbedding(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, &base.BatchEmbeddingResult{Embeddings: [][]float64{}}, result)
	assert.Zero(t, calls.Load())
}

func TestCreateBatchEmbedding_CardinalityMismatch(t *testing.T) {
	t.Parallel()

	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1]}]}`, nil, nil)
	client := newEmbedClient(t, srv.URL, nil)

	_, err := client.CreateBatchEmbedding(t.Context(), []string{"a", "b"})
	require.ErrorContains(t, err, "expected 2 embeddings, got 1")
}

func TestCreateBatchEmbedding_EmptyVector(t *testing.T) {
	t.Parallel()

	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1]},{}]}`, nil, nil)
	client := newEmbedClient(t, srv.URL, nil)

	_, err := client.CreateBatchEmbedding(t.Context(), []string{"a", "b"})
	require.ErrorContains(t, err, "empty embedding at index 1")
}

func TestCreateBatchEmbedding_InconsistentDimensions(t *testing.T) {
	t.Parallel()

	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]},{"values":[0.5]}]}`, nil, nil)
	client := newEmbedClient(t, srv.URL, nil)

	_, err := client.CreateBatchEmbedding(t.Context(), []string{"a", "b", "c"})
	require.ErrorContains(t, err, "inconsistent embedding dimensions: index 0 has 2, index 2 has 1")
}

func TestCreateBatchEmbedding_Embedding2DeveloperAPI(t *testing.T) {
	t.Parallel()

	srv, paths, bodies := recordingServer(t, `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`)
	client := newEmbedClientForModel(t, "gemini-embedding-2", srv.URL, map[string]any{"output_dimensionality": 2})

	result, err := client.CreateBatchEmbedding(t.Context(), []string{"first", "second"})
	require.NoError(t, err)

	require.Equal(t, []string{"/v1beta/models/gemini-embedding-2:batchEmbedContents"}, *paths)
	requests, ok := (*bodies)[0]["requests"].([]any)
	require.True(t, ok)
	require.Len(t, requests, 2)
	for i, text := range []string{"first", "second"} {
		req := requests[i].(map[string]any)
		assert.Equal(t, "models/gemini-embedding-2", req["model"])
		assert.Equal(t, text, req["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"])
		assert.InDelta(t, 2, req["outputDimensionality"], 0)
		assert.NotContains(t, req, "taskType")
	}

	require.Len(t, result.Embeddings, 2)
	assert.InDeltaSlice(t, []float64{0.1, 0.2}, result.Embeddings[0], 1e-6)
	assert.InDeltaSlice(t, []float64{0.3, 0.4}, result.Embeddings[1], 1e-6)
	assert.Zero(t, result.InputTokens)
}

func TestCreateBatchEmbedding_VertexPredict(t *testing.T) {
	t.Parallel()

	srv, paths, bodies := recordingServer(t, `{"predictions":[
		{"embeddings":{"values":[0.1,0.2],"statistics":{"token_count":3,"truncated":false}}},
		{"embeddings":{"values":[0.3,0.4],"statistics":{"token_count":4,"truncated":false}}}
	],"metadata":{"billableCharacterCount":11}}`)
	client := newVertexEmbedClient(t, "gemini-embedding-001", srv.URL, map[string]any{"output_dimensionality": 2})

	result, err := client.CreateBatchEmbedding(t.Context(), []string{"first", "second"})
	require.NoError(t, err)

	require.Equal(t, []string{"/v1beta1/publishers/google/models/gemini-embedding-001:predict"}, *paths)
	body := (*bodies)[0]
	assert.Equal(t, []any{map[string]any{"content": "first"}, map[string]any{"content": "second"}}, body["instances"])
	assert.Equal(t, map[string]any{"outputDimensionality": float64(2)}, body["parameters"])

	require.Len(t, result.Embeddings, 2)
	assert.InDeltaSlice(t, []float64{0.1, 0.2}, result.Embeddings[0], 1e-6)
	assert.InDeltaSlice(t, []float64{0.3, 0.4}, result.Embeddings[1], 1e-6)
	assert.Equal(t, int64(7), result.InputTokens)
	assert.Equal(t, int64(7), result.TotalTokens)
}

func TestCreateBatchEmbedding_VertexEmbedding2SequentialEmbedContent(t *testing.T) {
	t.Parallel()

	srv, paths, bodies := recordingServer(t,
		`{"embedding":{"values":[0.1,0.2]},"usageMetadata":{"promptTokenCount":3},"truncated":false}`,
		`{"embedding":{"values":[0.3,0.4]},"usageMetadata":{"promptTokenCount":4},"truncated":false}`,
	)
	client := newVertexEmbedClient(t, "gemini-embedding-2", srv.URL, map[string]any{"output_dimensionality": 2})

	result, err := client.CreateBatchEmbedding(t.Context(), []string{"first", "second"})
	require.NoError(t, err)

	wantPath := "/v1beta1/publishers/google/models/gemini-embedding-2:embedContent"
	require.Equal(t, []string{wantPath, wantPath}, *paths, "one embedContent request per input, in order")
	for i, text := range []string{"first", "second"} {
		body := (*bodies)[i]
		assert.Equal(t, map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}, body["content"])
		assert.Equal(t, map[string]any{"outputDimensionality": float64(2)}, body["embedContentConfig"])
		assert.NotContains(t, body, "instances")
	}

	require.Len(t, result.Embeddings, 2)
	assert.InDeltaSlice(t, []float64{0.1, 0.2}, result.Embeddings[0], 1e-6)
	assert.InDeltaSlice(t, []float64{0.3, 0.4}, result.Embeddings[1], 1e-6)
	assert.Equal(t, int64(7), result.InputTokens)
	assert.Equal(t, int64(7), result.TotalTokens)
}

func TestCreateBatchEmbedding_429SurfacesAsStatusError(t *testing.T) {
	t.Parallel()

	srv := embedServer(t, http.StatusTooManyRequests, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`, nil, nil)
	client := newEmbedClient(t, srv.URL, nil)

	_, err := client.CreateBatchEmbedding(t.Context(), []string{"hello"})
	require.Error(t, err)

	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se, "error must wrap *modelerrors.StatusError so the backoff gate can arm")
	assert.Equal(t, http.StatusTooManyRequests, se.StatusCode)
}

func TestCreateBatchEmbedding_ContextCancelled(t *testing.T) {
	t.Parallel()

	srv := embedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1]}]}`, nil, nil)
	client := newEmbedClient(t, srv.URL, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := client.CreateBatchEmbedding(ctx, []string{"hello"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestVertexEmbedsOneContentPerRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		surface string
		model   string
		want    bool
	}{
		{apiSurfaceVertexAI, "gemini-embedding-2", true},
		{apiSurfaceVertexAI, "gemini-embedding-001", false},
		{apiSurfaceVertexAI, "text-embedding-005", false},
		{apiSurfaceGeminiAPI, "gemini-embedding-2", false},
		{apiSurfaceGateway, "gemini-embedding-2", false},
	}
	for _, tt := range tests {
		c := &Client{apiSurface: tt.surface}
		c.ModelConfig.Model = tt.model
		assert.Equal(t, tt.want, c.vertexEmbedsOneContentPerRequest(), "%s/%s", tt.surface, tt.model)
	}
}

func TestEmbedEach_OneVectorPerResponse(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req batchEmbedRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Len(t, req.Requests, 1, "one content per request")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"embeddings":[{"values":[%d]}]}`, calls.Add(1))
	}))
	t.Cleanup(srv.Close)
	client := newEmbedClient(t, srv.URL, nil)

	genaiClient, err := client.clientFn(t.Context())
	require.NoError(t, err)

	embeddings, err := client.embedEach(t.Context(), genaiClient, []*genai.Content{
		genai.NewContentFromText("a", genai.RoleUser),
		genai.NewContentFromText("bb", genai.RoleUser),
		genai.NewContentFromText("ccc", genai.RoleUser),
	}, &genai.EmbedContentConfig{})
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
	require.Len(t, embeddings, 3)
	assert.Equal(t, []float32{1}, embeddings[0].Values)
	assert.Equal(t, []float32{2}, embeddings[1].Values)
	assert.Equal(t, []float32{3}, embeddings[2].Values)
}

// Two vectors then zero would still total two: the per-response check must
// catch the misalignment instead of silently pairing texts with wrong vectors.
func TestEmbedEach_RejectsMisalignedResponses(t *testing.T) {
	t.Parallel()

	srv, paths, _ := recordingServer(t, `{"embeddings":[{"values":[1]},{"values":[2]}]}`, `{"embeddings":[]}`)
	client := newEmbedClient(t, srv.URL, nil)

	genaiClient, err := client.clientFn(t.Context())
	require.NoError(t, err)

	_, err = client.embedEach(t.Context(), genaiClient, []*genai.Content{
		genai.NewContentFromText("a", genai.RoleUser),
		genai.NewContentFromText("b", genai.RoleUser),
	}, &genai.EmbedContentConfig{})
	require.ErrorContains(t, err, "expected 1 embedding for input 0, got 2")
	assert.Len(t, *paths, 1, "must fail at the offending response")
}
