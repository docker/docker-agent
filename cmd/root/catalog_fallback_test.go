package root

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
)

func TestCLICatalogFallbackWithColdCache(t *testing.T) {
	paths.SetCacheDir(t.TempDir())
	t.Cleanup(func() { paths.SetCacheDir("") })
	var rc config.RuntimeConfig
	addRuntimeConfigFlags(&cobra.Command{}, &rc)
	require.NotNil(t, rc.ModelsDevFallback)
	store, err := rc.ModelsDevStore()
	require.NoError(t, err)
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancel()
	db, err := store.GetDatabase(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, db.Providers, "normal CLI builds retain their offline catalog")
}
