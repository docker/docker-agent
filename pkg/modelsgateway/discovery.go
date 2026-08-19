// Package modelsgateway discovers the models served by an OpenAI-compatible
// models gateway by querying its /v1/models endpoint.
package modelsgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// listTimeout bounds the discovery request so an unresponsive gateway
// doesn't block callers (e.g. opening the model picker) for long.
const listTimeout = 5 * time.Second

// listResponse is the OpenAI-style body returned by GET /v1/models.
type listResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels queries <gatewayURL>/v1/models and returns the IDs of the
// models the gateway serves. Query parameters present in gatewayURL are
// forwarded. When the gateway targets a trusted Docker domain, the Docker
// Desktop token is required and sent as a Bearer token, mirroring how
// provider clients authenticate gateway traffic.
//
// An error is returned when the gateway is unreachable, responds with a
// non-200 status (e.g. it doesn't implement /v1/models), or returns a body
// that isn't a valid OpenAI-style model list. Callers are expected to fall
// back to their non-discovery behavior in that case.
func ListModels(ctx context.Context, gatewayURL string, env environment.Provider) ([]string, error) {
	return listModelsWith(ctx, gatewayURL, env, nil)
}

// listModelsWith is ListModels with an injectable HTTP client. A nil client
// uses the standard instrumented client; tests inject one to reach a
// non-trusted hostname without touching the network.
func listModelsWith(ctx context.Context, gatewayURL string, env environment.Provider, client *http.Client) ([]string, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway URL: %w", err)
	}

	endpoint := fmt.Sprintf("%s://%s%s/v1/models", u.Scheme, u.Host, strings.TrimSuffix(u.Path, "/"))
	if u.RawQuery != "" {
		endpoint += "?" + u.RawQuery
	}

	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}

	token, err := base.GatewayAuthToken(ctx, env, gatewayURL)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if client == nil {
		client = httpclient.NewHTTPClient(ctx, base.GatewayAuthRetry(env, gatewayURL)...)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying gateway models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway /v1/models returned status %s", resp.Status)
	}

	var list listResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding gateway models response: %w", err)
	}

	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
