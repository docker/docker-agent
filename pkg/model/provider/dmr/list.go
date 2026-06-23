package dmr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// listModelsTimeout bounds the /models request so a slow or wedged Docker
// Model Runner endpoint can't stall model discovery (the model picker and
// auto-selection both call ListModels synchronously).
const listModelsTimeout = 5 * time.Second

// ListModels returns the IDs of the models available to Docker Model Runner
// (i.e. pulled locally), as reported by its OpenAI-compatible /models
// endpoint. IDs keep their full DMR form, e.g. "ai/qwen3:latest". The result
// is sorted for deterministic ordering.
//
// It returns ErrNotInstalled when Docker Model Runner is not installed, and a
// wrapped error when the endpoint is unreachable or returns an unparseable
// body. A nil error with an empty slice means DMR is reachable but has no
// models pulled.
func ListModels(ctx context.Context) ([]string, error) {
	var endpoint string
	if os.Getenv("MODEL_RUNNER_HOST") == "" {
		ep, _, err := getDockerModelEndpointAndEngine(ctx)
		if err != nil {
			// Mirror NewClient: the unknown "--json" flag is the signal that
			// the Docker installation predates Model Runner, i.e. DMR is not
			// installed at all.
			if strings.Contains(err.Error(), "unknown flag: --json") {
				return nil, ErrNotInstalled
			}
			// Otherwise the docker CLI plugin may simply be unavailable while
			// the engine still serves DMR on a default endpoint, so fall
			// through and let resolveDMRBaseURL probe the defaults.
			slog.DebugContext(ctx, "docker model status query failed while listing models", "error", err)
		}
		endpoint = ep
	}

	baseURL, _, httpClient := resolveDMRBaseURL(ctx, &latest.ModelConfig{}, endpoint)
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return listModelsAt(ctx, httpClient, baseURL)
}

// listModelsAt fetches and parses the OpenAI-compatible /models response from
// the given DMR base URL. It is split out from ListModels so the HTTP handling
// can be unit-tested with an httptest server.
func listModelsAt(ctx context.Context, httpClient *http.Client, baseURL string) ([]string, error) {
	modelsURL := strings.TrimSuffix(baseURL, "/") + "/models"

	ctx, cancel := context.WithTimeout(ctx, listModelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating DMR models request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying DMR models endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DMR models endpoint returned status %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding DMR models response: %w", err)
	}

	models := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			models = append(models, id)
		}
	}
	slices.Sort(models)
	models = slices.Compact(models)

	slog.DebugContext(ctx, "Listed DMR models", "count", len(models), "base_url", baseURL)
	return models, nil
}
