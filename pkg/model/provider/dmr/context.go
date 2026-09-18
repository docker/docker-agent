package dmr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
)

// ContextWindow resolves the runner configuration before the packaged model's
// window. It never reconfigures the model or changes the user's provider options.
func (c *Client) ContextWindow(ctx context.Context) (int64, error) {
	// A gateway need not expose runner management APIs. Raw flags can override
	// structured limits, so don't guess the allocation in either case.
	if c.ModelOptions.Gateway() != "" || hasContextFlags(parseRuntimeFlags(c.ModelConfig.ProviderOpts)) {
		return 0, nil
	}
	if raw, _ := parseRawRuntimeFlags(c.ModelConfig.ProviderOpts); raw != "" || c.ModelConfig.ProviderOpts["hf_overrides"] != nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	endpoint, backend, err := modelConfigURL(c.BaseURL)
	if err != nil {
		return 0, err
	}
	query := endpoint.Query()
	query.Set("model", c.ModelConfig.Model)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("creating DMR config request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("querying DMR config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Without runtime configuration, the packaged maximum may overstate
		// the allocation. Leave discovery unknown even on older runners.
		return 0, fmt.Errorf("DMR config endpoint returned status %d", resp.StatusCode)
	}
	var configs []struct {
		Backend string
		Mode    string
		Config  configureBackendConfig
	}
	if err := json.NewDecoder(resp.Body).Decode(&configs); err != nil {
		return 0, fmt.Errorf("decoding DMR config: %w", err)
	}
	var limit int64
	needsMetadata := len(configs) == 0
	matched := false
	for _, entry := range configs {
		if entry.Mode != "completion" || (backend != "" && entry.Backend != backend) {
			continue
		}
		matched = true
		if hasContextFlags(entry.Config.RuntimeFlags) || (entry.Config.VLLM != nil && len(entry.Config.VLLM.HFOverrides) > 0) {
			return 0, nil
		}
		if entry.Config.ContextSize == nil || *entry.Config.ContextSize == 0 {
			needsMetadata = true
		}
		if entry.Config.ContextSize != nil {
			n := int64(*entry.Config.ContextSize)
			if n < 0 {
				return 0, nil
			}
			if n > 0 && (limit == 0 || n < limit) {
				limit = n
			}
		}
	}
	if matched && !needsMetadata && limit > 0 {
		return limit, nil
	}
	model, err := dmrmodels.GetModel(ctx, c.httpClient, c.BaseURL, c.ModelConfig.Model)
	if err != nil {
		return 0, err
	}
	if model.Metadata != nil && model.Metadata.ContextWindow > 0 {
		window := int64(model.Metadata.ContextWindow)
		if limit > 0 {
			window = min(window, limit)
		}
		return window, nil
	}
	return 0, nil
}

// GET _configure is engine-wide even when inference uses a backend-scoped URL.
func modelConfigURL(baseURL string) (*url.URL, string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, "", fmt.Errorf("parsing DMR base URL: %w", err)
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	var backend string
	if prefix, suffix, ok := strings.Cut(path, "/engines/"); ok {
		backend = suffix
		path = prefix + "/engines"
	}
	u.Path = path + "/_configure"
	return u, backend, nil
}

// These flags can override the context inferred from DMR's structured config.
func hasContextFlags(flags []string) bool {
	for _, flag := range flags {
		name, _, _ := strings.Cut(flag, "=")
		switch name {
		case "-c", "--ctx-size", "--max-model-len", "--context-length", "--max-tokens", "-np", "--parallel", "--hf-overrides":
			return true
		}
	}
	return false
}
