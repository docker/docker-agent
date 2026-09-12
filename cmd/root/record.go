package root

import (
	"context"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/recording"
)

// recordingProxyDockerTokenPlaceholder stands in for a real Docker Desktop
// auth token so provider clients can build while routed through the local
// fake/recording proxy. It is never sent to a real Docker AI Gateway: the
// proxy either replaces it with the configured provider's own API key
// (routeThroughRecordingProxy only sets it when there is no real upstream
// Docker gateway behind the proxy) or, for a genuine upstream Docker
// gateway, is never used because that case keeps the real token instead.
const recordingProxyDockerTokenPlaceholder = "cagent-local-recording-proxy"

// setupFakeProxy starts a fake proxy if fakeResponses is non-empty.
// It configures the runtime config's ModelsGateway to point to the proxy.
func setupFakeProxy(ctx context.Context, fakeResponses string, streamDelayMs int, runConfig *config.RuntimeConfig) (cleanup func() error, err error) {
	proxyURL, cleanupFn, err := recording.SetupFakeProxy(ctx, fakeResponses, streamDelayMs)
	if err != nil {
		return nil, err
	}

	if proxyURL != "" {
		routeThroughRecordingProxy(runConfig, proxyURL)
	}

	return cleanupFn, nil
}

// setupRecordingProxy starts a recording proxy if recordPath is non-empty.
// It configures the runtime config's ModelsGateway to point to the proxy.
// Any models gateway already configured becomes the proxy's upstream, so
// recording keeps routing (and auth) through the user's gateway.
func setupRecordingProxy(ctx context.Context, recordPath string, runConfig *config.RuntimeConfig) (cassettePath string, cleanup func() error, err error) {
	cassettePath, proxyURL, cleanupFn, err := recording.SetupRecordingProxy(ctx, recordPath, runConfig.ModelsGateway)
	if err != nil {
		return "", nil, err
	}

	if proxyURL != "" {
		routeThroughRecordingProxy(runConfig, proxyURL)
	}

	return cassettePath, cleanupFn, nil
}

// routeThroughRecordingProxy points the runtime config's models gateway at
// the local fake/recording proxy so every provider call is captured (or
// replayed) by it.
//
// The proxy always binds to localhost, which environment.IsTrustedDockerURL
// treats as a trusted Docker AI Gateway address — that heuristic exists for
// a genuine local Docker gateway, but it can't tell that one apart from our
// own capture proxy from the URL alone. Left unchecked, provider clients
// would refuse to build without a real Docker Desktop sign-in even when the
// configured provider (e.g. OpenRouter) has nothing to do with the Docker AI
// Gateway.
//
// When the gateway actually configured before the proxy took over isn't
// itself a trusted Docker gateway, there is no real Docker token to
// preserve, so a placeholder is supplied purely to satisfy client
// construction: the proxy discards it and re-authenticates with the
// provider's own API key before any request leaves the process. When a real
// trusted Docker gateway was configured, the placeholder is skipped so the
// genuine Docker Desktop token keeps flowing through, exactly as before.
func routeThroughRecordingProxy(runConfig *config.RuntimeConfig, proxyURL string) {
	if !environment.IsTrustedDockerURL(runConfig.ModelsGateway) {
		if runConfig.EnvOverrides == nil {
			runConfig.EnvOverrides = make(map[string]string, 1)
		}
		runConfig.EnvOverrides[environment.DockerDesktopTokenEnv] = recordingProxyDockerTokenPlaceholder
	}
	runConfig.ModelsGateway = proxyURL
}
