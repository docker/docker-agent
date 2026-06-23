package desktop

import (
	"context"
	"time"

	"github.com/docker/docker-agent/pkg/memoize"
)

// versionMemoizer caches the Docker Desktop version lookup with a TTL so:
//   - if docker-agent starts before Desktop is ready, version detection
//     recovers automatically once Desktop comes up;
//   - if Desktop is upgraded mid-session, the new version is picked up
//     within at most one TTL.
var versionMemoizer = memoize.New[string](5 * time.Minute)

// GetVersion returns the running Docker Desktop version (e.g. "4.74.0") or
// an empty string if Docker Desktop is not running or the call fails.
//
// The lookup is bounded by a short internal timeout so a stale or missing
// backend socket cannot stall callers on hot paths (it is queried on every
// outbound built-in tool HTTP request). ctx is used for the underlying
// HTTP call on a cache miss; on a cache hit the cached value is returned
// without consulting ctx.
func GetVersion(ctx context.Context) string {
	v, _ := versionMemoizer.Memoize("desktopVersion", func() (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		var info struct {
			CurrentVersion string `json:"currentVersion"`
		}
		_ = ClientBackend.Get(ctx, "/update", &info)
		return info.CurrentVersion, nil
	})
	return v
}
