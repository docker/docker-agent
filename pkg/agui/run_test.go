package agui

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

// The production entry point serves the endpoint behind its auth token and
// returns once its context is canceled.
func TestRun_ServesUntilCanceled(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "testdata/basic.yaml", "", filepath.Join(t.TempDir(), "session.db"), &config.RuntimeConfig{}, ln, RunOptions{AuthToken: "secret"})
	}()

	post := func(token string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+ln.Addr().String()+Path, strings.NewReader(`{"threadId":"t"}`))
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusUnauthorized, post(""))
	assert.Equal(t, http.StatusBadRequest, post("secret"))

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
}

func TestRun_UnknownAgent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	err := Run(t.Context(), "testdata/basic.yaml", "missing", filepath.Join(t.TempDir(), "session.db"), &config.RuntimeConfig{}, listen(t), RunOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get agent")
}
