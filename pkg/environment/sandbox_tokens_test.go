package environment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxTokenProvider_Get_ReturnsToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	tokens := sandboxTokens{DockerToken: "my-secret-token"}
	data, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	p := NewSandboxTokenProvider(path)
	val, ok := p.Get(t.Context(), DockerDesktopTokenEnv)
	assert.True(t, ok)
	assert.Equal(t, "my-secret-token", val)
}

func TestSandboxTokenProvider_Get_OnlyServesDockerToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	tokens := sandboxTokens{DockerToken: "my-secret-token"}
	data, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	p := NewSandboxTokenProvider(path)

	val, ok := p.Get(t.Context(), "OTHER_VAR")
	assert.False(t, ok)
	assert.Empty(t, val)
}

func TestSandboxTokenProvider_Get_MissingFile(t *testing.T) {
	t.Parallel()

	p := NewSandboxTokenProvider(filepath.Join(t.TempDir(), "nonexistent.json"))

	val, ok := p.Get(t.Context(), DockerDesktopTokenEnv)
	assert.False(t, ok)
	assert.Empty(t, val)
}

func TestSandboxTokenProvider_Get_InvalidJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))

	p := NewSandboxTokenProvider(path)

	val, ok := p.Get(t.Context(), DockerDesktopTokenEnv)
	assert.False(t, ok)
	assert.Empty(t, val)
}

func TestSandboxTokenProvider_Get_EmptyToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	tokens := sandboxTokens{DockerToken: ""}
	data, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	p := NewSandboxTokenProvider(path)

	val, ok := p.Get(t.Context(), DockerDesktopTokenEnv)
	assert.False(t, ok)
	assert.Empty(t, val)
}

func TestSandboxTokenWriter_WritesFileOnStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	provider := NewEnvListProvider([]string{"DOCKER_TOKEN=fresh-token"})
	w := NewSandboxTokenWriter(path, provider, time.Hour) // long interval; we only test the initial write
	w.Start(t.Context())
	defer w.Stop()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var tokens sandboxTokens
	require.NoError(t, json.Unmarshal(data, &tokens))
	assert.Equal(t, "fresh-token", tokens.DockerToken)
}

func TestSandboxTokenWriter_RefreshesToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	// Use a channel-based provider that returns different values.
	ch := make(chan string, 2)
	ch <- "token-v1"
	ch <- "token-v2"

	chanProvider := &chanTokenProvider{ch: ch}

	w := NewSandboxTokenWriter(path, chanProvider, 50*time.Millisecond)
	w.Start(t.Context())
	defer w.Stop()

	// The first write happens synchronously in Start, so token-v1 should be on disk.
	assertTokenFileContains(t, path, "token-v1")

	// Wait for at least one tick to pick up token-v2.
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var tokens sandboxTokens
		if err := json.Unmarshal(data, &tokens); err != nil {
			return false
		}
		return tokens.DockerToken == "token-v2"
	}, 2*time.Second, 25*time.Millisecond)
}

func TestSandboxTokenWriter_StopRemovesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	provider := NewEnvListProvider([]string{"DOCKER_TOKEN=test-token"})
	w := NewSandboxTokenWriter(path, provider, time.Hour)
	w.Start(t.Context())

	// File should exist.
	_, err := os.Stat(path)
	require.NoError(t, err)

	w.Stop()

	// File should be removed.
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestSandboxTokenWriter_StopIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SandboxTokensFileName)

	provider := NewEnvListProvider([]string{"DOCKER_TOKEN=test-token"})
	w := NewSandboxTokenWriter(path, provider, time.Hour)
	w.Start(t.Context())

	w.Stop()
	w.Stop() // Should not panic.
}

func TestSandboxTokensFilePath(t *testing.T) {
	t.Parallel()

	got := SandboxTokensFilePath("/home/user/.cagent")
	assert.Equal(t, "/home/user/.cagent/sandbox-tokens.json", got)
}

// --- helpers ---

func assertTokenFileContains(t *testing.T, path, expectedToken string) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var tokens sandboxTokens
	require.NoError(t, json.Unmarshal(data, &tokens))
	assert.Equal(t, expectedToken, tokens.DockerToken)
}

// chanTokenProvider returns the next value from its channel on each Get call.
type chanTokenProvider struct {
	ch   chan string
	last string
}

func (p *chanTokenProvider) Get(_ context.Context, name string) (string, bool) {
	if name != DockerDesktopTokenEnv {
		return "", false
	}
	select {
	case v := <-p.ch:
		p.last = v
		return v, true
	default:
		// No new value, return last known.
		if p.last != "" {
			return p.last, true
		}
		return "", false
	}
}
