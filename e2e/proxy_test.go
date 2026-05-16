package e2e_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/fake"
)

func startRecordingAIProxy(t *testing.T) (*httptest.Server, *config.RuntimeConfig) {
	t.Helper()

	cassettePath := filepath.Join("testdata", "cassettes", t.Name())

	// Create a matcher that fails the test on error
	matcher := fake.DefaultMatcher(func(err error) {
		require.NoError(t, err)
	})

	proxyURL, cleanup, err := fake.StartProxyWithOptions(
		cassettePath,
		recorder.ModeRecordOnce,
		matcher,
		fake.APIKeyHeaderUpdater,
		nil,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, cleanup())
	})

	return &httptest.Server{URL: proxyURL}, &config.RuntimeConfig{
		Config: config.Config{
			ModelsGateway: proxyURL,
		},
		EnvProviderForTests: &testEnvProvider{
			environment.DockerDesktopTokenEnv: "DUMMY",
		},
	}
}

type testEnvProvider map[string]string

func (p *testEnvProvider) Get(_ context.Context, name string) (string, bool) {
	val, found := (*p)[name]
	return val, found
}
