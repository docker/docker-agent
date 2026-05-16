package environment

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
)

type RunSecretsProvider struct {
	root string
}

func NewRunSecretsProvider() *RunSecretsProvider {
	return &RunSecretsProvider{
		root: "/run/secrets",
	}
}

// Get reads the named secret file from the provider's root directory.
//
// Lookups are anchored to root via os.OpenRoot: name components that
// escape via ".." segments, absolute paths, or symbolic links pointing
// outside the root are rejected by the kernel and surface as a missed
// lookup. An attacker who can plant a symlink under root therefore
// cannot coerce the agent into reading files elsewhere on the host.
func (p *RunSecretsProvider) Get(_ context.Context, name string) (string, bool) {
	root, err := os.OpenRoot(p.root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("Failed to open secrets root", "root", p.root, "error", err)
		}
		return "", false
	}
	defer root.Close()

	buf, err := root.ReadFile(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("Failed to read secret", "name", name, "error", err)
		}
		return "", false
	}
	return string(buf), true
}
