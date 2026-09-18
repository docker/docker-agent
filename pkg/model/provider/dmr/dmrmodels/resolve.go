// Package dmrmodels handles Docker Model Runner endpoint discovery and model
// listing. It is deliberately free of the OpenAI SDK (and any other model
// client dependency) so that pkg/runtime — which only needs DMR model
// discovery — does not link the full DMR provider client into embedders'
// binaries. The full client lives in the parent dmr package.
package dmrmodels

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
)

const (
	// dmrInferencePrefix mirrors github.com/docker/model-runner/pkg/inference.InferencePrefix.
	dmrInferencePrefix = "/engines"
	// dmrExperimentalEndpointsPrefix mirrors github.com/docker/model-runner/pkg/inference.ExperimentalEndpointsPrefix.
	dmrExperimentalEndpointsPrefix = "/exp/vDD4.40"

	// dmrDefaultPort is the default port for Docker Model Runner.
	dmrDefaultPort = "12434"

	// connectivityTimeout is the timeout for testing DMR endpoint connectivity.
	// This is kept short to quickly detect unreachable endpoints and try fallbacks.
	connectivityTimeout = 2 * time.Second
)

// ErrNotInstalled is returned when Docker Model Runner is not installed.
var ErrNotInstalled = errors.New("docker model runner is not available\nplease install it and try again (https://docs.docker.com/ai/model-runner/get-started/)")

// IsNotInstalledError reports whether a `docker model` invocation failed
// because the Docker installation predates Model Runner (the CLI rejects the
// --json flag). Matching on content rather than the exact message keeps the
// detection stable across docker CLI usage-text changes.
func IsNotInstalledError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown flag: --json") //rubocop:disable Lint/ErrorStringMatching // Docker CLI does not expose a typed error for unsupported flags
}

// defaultURL builds the standard DMR inference URL for a given host and port.
// Example: defaultURL("127.0.0.1", "12434") → "http://127.0.0.1:12434/engines/v1/"
func defaultURL(host, port string) string {
	return "http://" + net.JoinHostPort(host, port) + dmrInferencePrefix + "/v1/"
}

// defaultContainerURL is the default DMR URL when running inside a container
// with no explicit endpoint. It targets Docker Desktop's model-runner service.
func defaultContainerURL() string {
	return "http://model-runner.docker.internal" + dmrInferencePrefix + "/v1/"
}

// DefaultHostURL is the default DMR URL when running on the host with no
// explicit endpoint. It targets the standard local model-runner port.
func DefaultHostURL() string {
	return defaultURL("127.0.0.1", dmrDefaultPort)
}

// defaultForEnvironment returns the appropriate default URL based on whether
// the process is running inside a container or on the host.
func defaultForEnvironment() string {
	if inContainer() {
		return defaultContainerURL()
	}
	return DefaultHostURL()
}

func inContainer() bool {
	finfo, err := os.Stat("/.dockerenv")
	return err == nil && finfo.Mode().IsRegular()
}

// testDMRConnectivity performs a quick health check against a DMR endpoint.
// It returns true if the endpoint is reachable and responds within the timeout.
func testDMRConnectivity(ctx context.Context, httpClient *http.Client, baseURL string) bool {
	healthURL := strings.TrimSuffix(baseURL, "/") + "/models"

	ctx, cancel := context.WithTimeout(ctx, connectivityTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, http.NoBody)
	if err != nil {
		slog.DebugContext(ctx, "DMR connectivity check: failed to create request", "url", healthURL, "error", err)
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.DebugContext(ctx, "DMR connectivity check: request failed", "url", healthURL, "error", err)
		return false
	}
	defer resp.Body.Close()

	// Any response (even 4xx/5xx) means the server is reachable
	slog.DebugContext(ctx, "DMR connectivity check: success", "url", healthURL, "status", resp.StatusCode)
	return true
}

// getDMRFallbackURLs returns a list of fallback URLs to try for DMR connectivity.
// The order is chosen to maximize compatibility across platforms:
// 1. model-runner.docker.internal - Docker Desktop's integrated model-runner
// 2. host.docker.internal - Docker Desktop's host access (works on macOS/Windows/Linux Desktop)
// 3. 172.17.0.1 - Default Docker bridge gateway (Linux Docker CE)
// 4. 127.0.0.1 - Localhost (when running directly on host)
func getDMRFallbackURLs(containerized bool) []string {
	if containerized {
		return []string{
			defaultContainerURL(),
			defaultURL("host.docker.internal", dmrDefaultPort),
			defaultURL("172.17.0.1", dmrDefaultPort),
		}
	}
	return []string{DefaultHostURL()}
}

// ResolveBaseURL determines the correct base URL to talk to Docker Model
// Runner, mirroring the behavior of the `docker model` CLI as closely as
// possible.
//
// High‑level rules:
//   - If the user explicitly configured a BaseURL or MODEL_RUNNER_HOST, use that (no fallbacks).
//   - For Desktop endpoints (model-runner.docker.internal) on the host, route
//     through the Docker Engine experimental endpoints prefix over the Unix socket.
//   - For standalone / offload endpoints like http://172.17.0.1:12435/engines/v1/,
//     use localhost:<port>/engines/v1/ on the host, and the gateway IP:port inside containers.
//   - Keep a small compatibility workaround for the legacy http://:0/engines/v1/ endpoint.
//   - Test connectivity and try fallback URLs if the primary endpoint is unreachable.
//
// It also returns an *http.Client when a custom transport (e.g., Docker Unix socket) is needed.
func ResolveBaseURL(ctx context.Context, cfg *latest.ModelConfig, endpoint string) (string, *http.Client) {
	// Explicit configuration — return immediately without fallback testing.
	if cfg != nil && cfg.BaseURL != "" {
		slog.DebugContext(ctx, "DMR using explicitly configured BaseURL", "url", cfg.BaseURL)
		return cfg.BaseURL, nil
	}
	if host := os.Getenv("MODEL_RUNNER_HOST"); host != "" {
		baseURL := strings.TrimRight(host, "/") + dmrInferencePrefix + "/v1/"
		slog.DebugContext(ctx, "DMR using MODEL_RUNNER_HOST", "url", baseURL)
		return baseURL, nil
	}

	// Resolve primary URL based on endpoint
	baseURL, httpClient := resolvePrimaryDMRURL(endpoint)

	// Test connectivity and try fallbacks if needed
	testClient := cmp.Or(httpClient, &http.Client{}) //rubocop:disable Lint/HTTPClientTransport // DMR connectivity probe; default transport is appropriate
	containerized := inContainer()

	if !testDMRConnectivity(ctx, testClient, baseURL) {
		slog.DebugContext(ctx, "DMR primary endpoint unreachable, trying fallbacks", "primary_url", baseURL, "in_container", containerized)

		for _, fallbackURL := range getDMRFallbackURLs(containerized) {
			if fallbackURL == baseURL {
				continue
			}
			slog.DebugContext(ctx, "DMR trying fallback endpoint", "url", fallbackURL)
			if testDMRConnectivity(ctx, &http.Client{}, fallbackURL) { //rubocop:disable Lint/HTTPClientTransport // DMR connectivity probe; default transport is appropriate
				slog.InfoContext(ctx, "DMR using fallback endpoint", "fallback_url", fallbackURL, "original_url", baseURL)
				return fallbackURL, nil
			}
		}
		// All endpoints unreachable — continue with primary URL.
		// The client will fail on first actual use, providing a better error.
		slog.ErrorContext(ctx, "DMR all endpoints currently unreachable, will fail on first use", "primary_url", baseURL, "in_container", containerized)
	} else {
		slog.DebugContext(ctx, "DMR primary endpoint reachable", "url", baseURL)
	}

	return baseURL, httpClient
}

// resolvePrimaryDMRURL resolves the primary DMR URL based on the endpoint string.
// This handles the various endpoint formats and platform-specific routing without
// connectivity testing or fallbacks.
func resolvePrimaryDMRURL(endpoint string) (string, *http.Client) {
	ep := strings.TrimSpace(endpoint)

	// Legacy bug workaround: old DMR versions <= 0.1.44 could report http://:0/engines/v1/.
	if ep == "http://:0/engines/v1/" {
		return DefaultHostURL(), nil
	}

	if ep == "" {
		return defaultForEnvironment(), nil
	}

	u, err := url.Parse(ep)
	if err != nil {
		slog.Debug("failed to parse DMR endpoint, falling back to defaults", "endpoint", ep, "error", err)
		return defaultForEnvironment(), nil
	}

	host := u.Hostname()
	port := u.Port()

	// Desktop endpoint on the host — route through Docker Engine's Unix socket.
	if host == "model-runner.docker.internal" && !inContainer() {
		expPrefix := strings.TrimPrefix(dmrExperimentalEndpointsPrefix, "/")
		baseURL := fmt.Sprintf("http://_/%s%s/v1", expPrefix, dmrInferencePrefix)

		httpClient := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", "/var/run/docker.sock")
				},
			},
		}

		return baseURL, httpClient
	}

	port = cmp.Or(port, dmrDefaultPort)

	// Inside a container — use the endpoint as-is.
	if inContainer() {
		baseURL := ep
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		return baseURL, nil
	}

	// Host case — always talk to localhost:<port>, even if the status
	// endpoint uses a gateway IP like 172.17.0.1.
	return defaultURL("127.0.0.1", port), nil
}

// DockerModelEndpointAndEngine shells out to `docker model status --json`
// and returns the resolved endpoint URL and the active inference engine name.
func DockerModelEndpointAndEngine(ctx context.Context) (endpoint, engine string, err error) {
	cmd := exec.CommandContext(ctx, "docker", "model", "status", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", errors.New(strings.TrimSpace(stderr.String()))
	}

	var st struct {
		Running  bool              `json:"running"`
		Backends map[string]string `json:"backends"`
		Endpoint string            `json:"endpoint"`
		Engine   string            `json:"engine"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		return "", "", err
	}

	endpoint = strings.TrimSpace(st.Endpoint)

	engine = strings.TrimSpace(st.Engine)
	if engine == "" && st.Backends != nil {
		if _, ok := st.Backends["llama.cpp"]; ok {
			engine = "llama.cpp"
		} else {
			for k := range st.Backends {
				engine = k
				break
			}
		}
	}
	engine = cmp.Or(engine, "llama.cpp")

	return endpoint, engine, nil
}
