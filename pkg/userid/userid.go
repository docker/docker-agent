// Package userid exposes the persistent UUID identifying this cagent
// installation. The value is stored in `$configDir/user-uuid`, generated
// lazily on first use, and shared across cagent runs on the same machine.
//
// It is consumed both by telemetry (as the `user_uuid` event property)
// and by the HTTP transport (as the `X-Cagent-Id` header on
// gateway-bound requests) so that the gateway can correlate calls made
// by the same cagent install without having to invent a new identifier.
package userid

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/paths"
)

// fileName is the basename of the file holding the persistent UUID,
// stored under [paths.GetConfigDir].
const fileName = "user-uuid"

var (
	mu     sync.Mutex
	cached string
)

// Get returns the persistent UUID identifying this cagent installation.
//
// On the first call it tries to read the value from
// `$configDir/user-uuid`; if the file does not exist, is empty, or
// cannot be read, a fresh UUID is generated and persisted (best
// effort). The result is cached in memory for the lifetime of the
// process so subsequent calls do not touch the filesystem.
func Get() string {
	mu.Lock()
	defer mu.Unlock()

	if cached != "" {
		return cached
	}

	file := filePath()

	if data, err := os.ReadFile(file); err == nil {
		if existing := strings.TrimSpace(string(data)); existing != "" {
			// Validate that the stored value is actually a valid UUID.
			// If the file was manually edited or corrupted, regenerate
			// rather than propagating invalid data to telemetry and
			// the gateway.
			if _, err := uuid.Parse(existing); err == nil {
				cached = existing
				return cached
			}
			// File contains invalid UUID — fall through and regenerate.
		}
		// File exists but is empty/whitespace — fall through and
		// regenerate so we always return a valid UUID.
	}

	id := uuid.New().String()
	// Best-effort persistence: even if we cannot save the value to
	// disk we still cache it in memory so the same identifier is used
	// for the rest of this process.
	_ = save(file, id)
	cached = id
	return cached
}

// ResetForTests clears the in-memory cache. Tests in any package
// that rely on a deterministic config dir override should call this
// after [paths.SetConfigDir] to force the next [Get] call to re-read
// from disk.
func ResetForTests() {
	mu.Lock()
	defer mu.Unlock()
	cached = ""
}

func filePath() string {
	return filepath.Join(paths.GetConfigDir(), fileName)
}

func save(file, id string) error {
	// Use 0o700 on the directory to match the 0o600 protection on the
	// file itself: the per-install UUID is forwarded as `X-Cagent-Id`
	// on every gateway request, so even directory-level enumeration on
	// a shared host is a mild privacy leak we'd like to avoid.
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(id), 0o600)
}
