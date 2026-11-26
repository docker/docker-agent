package environment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
)

// SecretStore persists a secret so that one of the default secret sources
// ([DefaultSources]) can resolve it on later runs. Implementations are the
// write-side counterparts of the read-side providers: a value stored under a
// name must be returned by the matching provider's Get for that same name.
type SecretStore interface {
	// Name identifies the matching read-side source (e.g. "config-env-file").
	Name() string
	// Description is a short human-readable label for interactive pickers.
	Description() string
	// Store persists value under name, replacing any previous value.
	Store(ctx context.Context, name, value string) error
}

// SecretStores returns the secret stores usable on this machine, most
// preferred first. Currently the only store is the docker agent env file,
// which is always available as a plain-file store.
func SecretStores() []SecretStore {
	return []SecretStore{NewConfigEnvFileStore()}
}

// EnvFileStore stores secrets as KEY=value lines in an env file. The file at
// [ConfigEnvFilePath] is read by the default source chain, so values stored
// there resolve on every later run without extra flags.
type EnvFileStore struct {
	path string
}

// NewConfigEnvFileStore creates an EnvFileStore writing to the docker agent
// config env file (see [ConfigEnvFilePath]).
func NewConfigEnvFileStore() *EnvFileStore {
	return &EnvFileStore{path: ConfigEnvFilePath()}
}

func (s *EnvFileStore) Name() string { return "config-env-file" }

func (s *EnvFileStore) Description() string {
	return fmt.Sprintf("docker agent env file (%s, plain text)", s.path)
}

// Store writes NAME=value to the env file, replacing the line for an existing
// NAME and preserving every other line (comments included). The file is
// created private (0600) in a private directory and written atomically.
func (s *EnvFileStore) Store(_ context.Context, name, value string) error {
	if name == "" || strings.ContainsAny(name, "=\n") {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	if strings.Contains(value, "\n") {
		return fmt.Errorf("value for %s cannot contain newlines", name)
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating config directory for %s: %w", s.path, err)
	}

	existing, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", s.path, err)
	}

	content := upsertEnvLine(string(existing), name, value)
	if err := atomicfile.Write(s.path, strings.NewReader(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", s.path, err)
	}
	return nil
}

// upsertEnvLine replaces the NAME=... line in content or appends one,
// returning the new file content. Existing lines are preserved verbatim so
// user comments and unrelated variables survive updates.
func upsertEnvLine(content, name, value string) string {
	newLine := name + "=" + value

	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}

	replaced := false
	for i, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == name {
			lines[i] = newLine
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, newLine)
	}

	return strings.Join(lines, "\n") + "\n"
}

// ConfigEnvFilePath returns the path of the env file that the default secret
// sources read: <config dir>/.env.
func ConfigEnvFilePath() string {
	return filepath.Join(paths.GetConfigDir(), ".env")
}
