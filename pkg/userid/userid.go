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
	"uuid"

	"github.com/docker/docker-agent/pkg/paths"
)

// fileName is the basename of the file holding the persistent UUID,
// stored under the resolver's directory.
const fileName = "user-uuid"

// Resolver owns the directory it reads from and the in-memory cache of
// the resolved UUID. Each instance is fully independent, so tests can
// create one per [testing.T.TempDir] and run in parallel without
// sharing any global state.
type Resolver struct {
	dirFn func() string

	mu     sync.Mutex
	cached string
}

// New returns a [Resolver] that reads and writes the UUID file in dir.
func New(dir string) *Resolver {
	return &Resolver{dirFn: func() string { return dir }}
}

// Get returns the persistent UUID identifying this cagent installation.
//
// On the first call it tries to read the value from
// `<dir>/user-uuid`; if the file does not exist, is empty, contains an
// invalid UUID, or cannot be read, a fresh UUID is generated and
// persisted (best effort). The result is cached in memory for the
// lifetime of the resolver so subsequent calls do not touch the
// filesystem.
func (r *Resolver) Get() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != "" {
		return r.cached
	}

	file := filepath.Join(r.dirFn(), fileName)

	if data, err := os.ReadFile(file); err == nil {
		if existing := strings.TrimSpace(string(data)); existing != "" {
			// Validate that the stored value is actually a valid UUID.
			// If the file was manually edited or corrupted, regenerate
			// rather than propagating invalid data to telemetry and
			// the gateway.
			if validStoredUUID(existing) {
				r.cached = existing
				return r.cached
			}
			// File contains invalid UUID — fall through and regenerate.
		}
		// File exists but is empty/whitespace — fall through and
		// regenerate so we always return a valid UUID.
	}

	id := uuid.NewV4().String()
	// Best-effort persistence: even if we cannot save the value to
	// disk we still cache it in memory so the same identifier is used
	// for the rest of this process.
	_ = save(file, id)
	r.cached = id
	return r.cached
}

func validStoredUUID(s string) bool {
	// Preserve the formats accepted by the former Google UUID parser.
	switch {
	case len(s) == 38:
		s = s[1:37]
	case len(s) == 45 && strings.EqualFold(s[:9], "urn:uuid:"):
		s = s[9:]
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// defaultResolver backs the package-level [Get]. It resolves its
// directory lazily through [paths.GetConfigDir] so it always honours
// the config-dir override in effect at call time.
var defaultResolver = &Resolver{dirFn: paths.GetConfigDir}

// Get returns the persistent UUID using the default resolver, which
// reads from [paths.GetConfigDir].
func Get() string {
	return defaultResolver.Get()
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
