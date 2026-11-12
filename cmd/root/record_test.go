package root

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func TestSetupRecordingProxy_EmptyPath(t *testing.T) {
	t.Parallel()

	var runConfig config.RuntimeConfig

	cassettePath, cleanup, err := setupRecordingProxy(t.Context(), "", &runConfig)

	require.NoError(t, err)
	assert.Empty(t, cassettePath)
	assert.NotNil(t, cleanup)
	assert.Empty(t, runConfig.ModelsGateway, "ModelsGateway should not be set")

	require.NoError(t, cleanup())
}

func TestSetupRecordingProxy_AutoGeneratesFilename(t *testing.T) {
	t.Chdir(t.TempDir())

	var runConfig config.RuntimeConfig

	cassettePath, cleanup, err := setupRecordingProxy(t.Context(), "true", &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	assert.True(t, strings.HasPrefix(cassettePath, "cagent-recording-"), "should have auto-generated prefix")
	assert.True(t, strings.HasSuffix(cassettePath, ".yaml"), "should have .yaml suffix")
	assert.NotEmpty(t, runConfig.ModelsGateway, "ModelsGateway should be set")
}

func TestSetupRecordingProxy_CreatesProxy(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-recording")

	var runConfig config.RuntimeConfig

	resultPath, cleanup, err := setupRecordingProxy(t.Context(), cassettePath, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	assert.Equal(t, cassettePath+".yaml", resultPath)
	assert.True(t, strings.HasPrefix(runConfig.ModelsGateway, "http://"), "ModelsGateway should be HTTP URL")
}

func TestSetupRecordingEvaluatorOptions(t *testing.T) {
	t.Parallel()
	for _, gateway := range []string{"", "https://gateway.example.com"} {
		t.Run(gateway, func(t *testing.T) {
			t.Parallel()
			rc := &config.RuntimeConfig{Config: config.Config{ModelsGateway: gateway}}
			_, cleanup, err := setupRecordingProxy(t.Context(), filepath.Join(t.TempDir(), "recording"), rc)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, cleanup()) })
			if gateway == "" {
				opts := options.Apply(rc.EvaluatorOptions...)
				assert.Empty(t, opts.Gateway())
				assert.NotNil(t, opts.TransportWrapper())
			} else {
				assert.Empty(t, rc.EvaluatorOptions, "gateway authentication stays on the evaluator client")
			}
		})
	}
}

func TestFakeEvaluatorBypassRemainsOffline(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			direct := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("fake evaluator made a live request")
			}))
			t.Cleanup(direct.Close)
			path := filepath.Join(t.TempDir(), "offline")
			require.NoError(t, os.WriteFile(path+".yaml", []byte(`version: 2
interactions: []
`), 0o600))
			rc := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "chat-key"})}
			cleanup, err := setupFakeProxy(t.Context(), path, 0, rc)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, cleanup()) })
			manifest := fmt.Sprintf(`evaluators:
  risk:
    provider: %s
    model: model
    endpoint: %s/development/predict
    bypass_models_gateway: true
    type: boolean
    instructions: Assess risk.
agents:
  root:
    model: openai/gpt-5-mini
    hooks:
      tool_guard:
        - hooks:
            - type: evaluator
              evaluator: risk
              evaluator_policy:
                decisions: {"true": ask}
                min_probability: 0.9
                fallback: ask
`, backend, direct.URL)
			loaded, err := teamloader.LoadWithConfig(t.Context(), config.NewBytesSource("offline.yaml", []byte(manifest)), rc,
				teamloader.WithProviderRegistry(providers.NewDefaultRegistry()))
			require.NoError(t, err, "fake evaluator must not require upstream credentials")
			client, ok := loaded.Team.Evaluator("risk")
			require.True(t, ok)
			_, err = client.Evaluate(t.Context(), "unmatched private evidence")
			require.Error(t, err, "missing interaction must fail closed")
		})
	}
}

func TestRecordingDirectEvaluatorFailsCredentialPreflight(t *testing.T) {
	t.Parallel()
	rc := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "chat-key"})}
	_, cleanup, err := setupRecordingProxy(t.Context(), filepath.Join(t.TempDir(), "recording"), rc)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cleanup()) })
	manifest := `evaluators:
  risk:
    provider: typesafe
    model: jev-latest
    type: boolean
    instructions: Assess risk.
agents:
  root:
    model: openai/gpt-5-mini
    hooks:
      tool_guard:
        - hooks:
            - type: evaluator
              evaluator: risk
              evaluator_policy:
                decisions: {"true": ask}
                min_probability: 0.9
                fallback: ask
`
	_, err = teamloader.LoadWithConfig(t.Context(), config.NewBytesSource("recording.yaml", []byte(manifest)), rc,
		teamloader.WithProviderRegistry(providers.NewDefaultRegistry()))
	require.ErrorContains(t, err, "TYPESAFE_API_KEY")
}
