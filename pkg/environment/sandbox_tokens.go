package environment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

// sandboxTokens is the JSON schema for the file.
type sandboxTokens struct {
	DockerToken string `json:"docker_token,omitempty"`
}

// SandboxTokensFileName is the name of the JSON file used to forward
// short-lived tokens (e.g. DOCKER_TOKEN) from the host into a Docker
// sandbox. The host writes this file periodically; the sandbox reads it.
const SandboxTokensFileName = "sandbox-tokens.json"

// SandboxTokensFilePath returns the absolute path to the sandbox tokens
// file inside the given directory.
func SandboxTokensFilePath(dir string) string {
	return filepath.Join(dir, SandboxTokensFileName)
}

// SandboxTokenProvider reads DOCKER_TOKEN from a JSON file on disk.
// It is used inside the sandbox where Docker Desktop's backend API
// is unreachable and the OS env contains only a stale one-shot token.
//
// Only DOCKER_TOKEN is served; requests for any other variable return
// ("", false).
type SandboxTokenProvider struct {
	path string
}

// NewSandboxTokenProvider creates a provider that reads tokens from path.
func NewSandboxTokenProvider(path string) *SandboxTokenProvider {
	return &SandboxTokenProvider{
		path: path,
	}
}

// Get implements [Provider]. It returns DOCKER_TOKEN read from the JSON
// file, or ("", false) for any other variable name or on read failure.
func (p *SandboxTokenProvider) Get(_ context.Context, name string) (string, bool) {
	if name != DockerDesktopTokenEnv {
		return "", false
	}

	data, err := os.ReadFile(p.path)
	if err != nil {
		slog.Debug("Failed to read sandbox tokens file", "path", p.path, "error", err)
		return "", false
	}

	var tokens sandboxTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		slog.Debug("Failed to parse sandbox tokens file", "path", p.path, "error", err)
		return "", false
	}

	if tokens.DockerToken == "" {
		return "", false
	}

	return tokens.DockerToken, true
}

// SandboxTokenWriter periodically fetches DOCKER_TOKEN from a provider
// and writes it to the sandbox tokens JSON file so that processes inside
// the sandbox can read a fresh value.
type SandboxTokenWriter struct {
	path     string
	provider Provider
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

// NewSandboxTokenWriter creates a writer that refreshes the token file at
// the given interval. Call [SandboxTokenWriter.Start] to begin writing and
// [SandboxTokenWriter.Stop] to terminate the background goroutine.
func NewSandboxTokenWriter(path string, provider Provider, interval time.Duration) *SandboxTokenWriter {
	return &SandboxTokenWriter{
		path:     path,
		provider: provider,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start writes the token file immediately and then refreshes it on the
// configured interval in a background goroutine.
func (w *SandboxTokenWriter) Start(ctx context.Context) {
	// Write once synchronously so the file exists before the sandbox starts.
	w.writeOnce(ctx)

	go func() {
		defer close(w.done)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				w.writeOnce(ctx)
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop terminates the background goroutine and removes the token file.
func (w *SandboxTokenWriter) Stop() {
	w.once.Do(func() {
		close(w.stop)
		<-w.done

		if err := os.Remove(w.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("Failed to remove sandbox tokens file", "path", w.path, "error", err)
		}
	})
}

// writeOnce fetches the token and writes it to the JSON file atomically
// (write-to-temp + rename) to avoid partial reads.
func (w *SandboxTokenWriter) writeOnce(ctx context.Context) {
	token, ok := w.provider.Get(ctx, DockerDesktopTokenEnv)
	if !ok {
		slog.DebugContext(ctx, "No DOCKER_TOKEN available to write to sandbox tokens file")
		return
	}

	tokens := sandboxTokens{DockerToken: token}
	data, err := json.Marshal(tokens)
	if err != nil {
		slog.DebugContext(ctx, "Failed to marshal sandbox tokens", "error", err)
		return
	}

	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		slog.DebugContext(ctx, "Failed to create sandbox tokens directory", "path", w.path, "error", err)
		return
	}

	if err := atomicfile.Write(w.path, bytes.NewReader(data), 0o600); err != nil {
		slog.DebugContext(ctx, "Failed to write sandbox tokens file", "path", w.path, "error", err)
		return
	}

	slog.DebugContext(ctx, "Wrote sandbox tokens file", "path", w.path)
}
