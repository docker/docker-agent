package dmrmodels

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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

// Model is an entry from the OpenAI-compatible models endpoint.
type Model struct {
	ID       string    `json:"id"`
	Metadata *Metadata `json:"dmr,omitempty"`
}

// Metadata describes the packaged model, not necessarily its running configuration.
type Metadata struct {
	ContextWindow int32  `json:"context_window,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
	Parameters    string `json:"parameters,omitempty"`
	Quantization  string `json:"quantization,omitempty"`
	Size          string `json:"size,omitempty"`
}

// ListModels returns sorted, unique IDs of locally available models.
func ListModels(ctx context.Context) ([]string, error) {
	models, err := ListModelsWithMetadata(ctx)
	if err != nil {
		return nil, err
	}
	return modelIDs(models), nil
}

func modelIDs(models []Model) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

// ListModelsWithMetadata discovers the runner and lists its locally available models.
func ListModelsWithMetadata(ctx context.Context) ([]Model, error) {
	var endpoint string
	if os.Getenv("MODEL_RUNNER_HOST") == "" {
		ep, _, err := DockerModelEndpointAndEngine(ctx)
		if err != nil {
			// Mirror NewClient: the unknown "--json" flag is the signal that
			// the Docker installation predates Model Runner, i.e. DMR is not
			// installed at all.
			if IsNotInstalledError(err) {
				return nil, ErrNotInstalled
			}
			// Otherwise the docker CLI plugin may simply be unavailable while
			// the engine still serves DMR on a default endpoint, so fall
			// through and let ResolveBaseURL probe the defaults.
			slog.DebugContext(ctx, "docker model status query failed while listing models", "error", err)
		}
		endpoint = ep
	}

	baseURL, httpClient := ResolveBaseURL(ctx, &latest.ModelConfig{}, endpoint)
	if httpClient == nil {
		httpClient = &http.Client{} //rubocop:disable Lint/HTTPClientTransport // DMR local service; default transport is appropriate
	}

	return ListModelsWithMetadataAt(ctx, httpClient, baseURL)
}

// ListModelsAt lists model IDs at an already resolved endpoint.
func ListModelsAt(ctx context.Context, httpClient *http.Client, baseURL string) ([]string, error) {
	models, err := ListModelsWithMetadataAt(ctx, httpClient, baseURL)
	if err != nil {
		return nil, err
	}
	return modelIDs(models), nil
}

// ListModelsWithMetadataAt retains optional DMR metadata, including on older runners
// that return only IDs. The result is sorted and deduplicated by ID.
func ListModelsWithMetadataAt(ctx context.Context, httpClient *http.Client, baseURL string) ([]Model, error) {
	var body struct {
		Data []Model `json:"data"`
	}
	if err := getJSON(ctx, httpClient, strings.TrimRight(baseURL, "/")+"/models", &body); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(body.Data))
	for _, model := range body.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID != "" {
			models = append(models, model)
		}
	}
	slices.SortStableFunc(models, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	models = slices.CompactFunc(models, func(a, b Model) bool { return a.ID == b.ID })
	slog.DebugContext(ctx, "Listed DMR models", "count", len(models), "base_url", baseURL)
	return models, nil
}

// GetModel retrieves metadata using the runner's own model-reference resolution.
func GetModel(ctx context.Context, httpClient *http.Client, baseURL, model string) (*Model, error) {
	var result Model
	if err := getJSON(ctx, httpClient, strings.TrimRight(baseURL, "/")+"/models/"+url.PathEscape(model), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func getJSON(ctx context.Context, httpClient *http.Client, endpoint string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, listModelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("creating DMR models request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("querying DMR models endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DMR models endpoint returned status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decoding DMR models response: %w", err)
	}
	return nil
}
