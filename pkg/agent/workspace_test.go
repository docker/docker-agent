package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestForWorkingDirRequiresFactory(t *testing.T) {
	t.Parallel()
	rebuilt, err := New("root", "").ForWorkingDir(t.Context(), t.TempDir())
	require.ErrorContains(t, err, "no working directory factory")
	assert.Nil(t, rebuilt)
}

func TestForWorkingDirSnapshotsRuntimeOverrides(t *testing.T) {
	t.Parallel()
	model := &mockProvider{id: modelsdev.NewID("test", "base")}
	override := &mockProvider{id: modelsdev.NewID("test", "override")}
	harness := &latest.HarnessConfig{Type: "claude-code", Effort: "low"}
	var dirs []string
	original := New("root", "", WithModel(model), WithHarness(harness),
		WithWorkingDirFactory(func(ctx context.Context, dir string) (*Agent, error) {
			require.Same(t, t.Context(), ctx)
			dirs = append(dirs, dir)
			return New("factory-name", "", WithModel(model), WithHarness(harness)), nil
		}))
	original.SetModelOverride(override)
	original.SetHarnessEffort("high")
	rebuilt, err := original.ForWorkingDir(t.Context(), "workspace")
	require.NoError(t, err)
	assert.NotSame(t, original, rebuilt)
	assert.Equal(t, original.Name(), rebuilt.Name())
	assert.Same(t, override, rebuilt.Model(t.Context()))
	assert.Equal(t, "high", rebuilt.Harness().Effort)
	rebuilt.SetModelOverride()
	rebuilt.SetHarnessEffort("max")
	assert.Same(t, model, rebuilt.Model(t.Context()))
	assert.Same(t, override, original.Model(t.Context()))
	assert.Equal(t, "high", original.Harness().Effort)
	nested, err := rebuilt.ForWorkingDir(t.Context(), "nested")
	require.NoError(t, err)
	assert.Same(t, model, nested.Model(t.Context()))
	assert.Equal(t, "max", nested.Harness().Effort)
	assert.Equal(t, []string{"workspace", "nested"}, dirs)
}

func TestForWorkingDirFactoryFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("tool creation failed")
	original := New("root", "", WithWorkingDirFactory(func(context.Context, string) (*Agent, error) {
		return nil, failure
	}))
	rebuilt, err := original.ForWorkingDir(t.Context(), "workspace")
	require.ErrorIs(t, err, failure)
	assert.Nil(t, rebuilt)

	for _, result := range []*Agent{nil, original} {
		WithWorkingDirFactory(func(context.Context, string) (*Agent, error) { return result, nil })(original)
		rebuilt, err := original.ForWorkingDir(t.Context(), "workspace")
		require.ErrorContains(t, err, "must return a new agent")
		assert.Nil(t, rebuilt)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rebuilt, err = original.ForWorkingDir(ctx, "workspace")
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, rebuilt)
}
