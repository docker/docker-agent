package runtime

import (
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
)

// withTestCatalog returns cfg with an in-memory models.dev catalog so a team
// loader fixture never depends on the developer's downloaded catalog.
//
// TestMain isolates the cache root (testutil.RunWithTempHome), which makes the
// first models.dev lookup of every fresh test process a cache miss. Without an
// override, teamloader.Load resolves model aliases and context windows against
// the live models.dev API, which has a 30-second HTTP timeout.
func withTestCatalog(cfg *config.RuntimeConfig) *config.RuntimeConfig {
	if cfg.ModelsDevStoreOverride == nil {
		cfg.ModelsDevStoreOverride = modelsdev.NewDatabaseStore(embedded.Snapshot())
	}
	return cfg
}
