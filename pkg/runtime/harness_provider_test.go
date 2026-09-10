package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

func TestNewHarnessProvider_RequiresRegistration(t *testing.T) {
	// Not parallel: swaps the package-level factory.
	original := harnessFactory.Load()
	harnessFactory.Store(nil)
	t.Cleanup(func() { harnessFactory.Store(original) })

	_, err := (&LocalRuntime{}).newHarnessProvider(&latest.HarnessConfig{Type: "codex"})
	require.ErrorIs(t, err, ErrHarnessNotRegistered)

	var called *latest.HarnessConfig
	RegisterHarness(func(cfg *latest.HarnessConfig) (harness.Provider, error) {
		called = cfg
		return nil, nil
	})
	_, err = (&LocalRuntime{}).newHarnessProvider(&latest.HarnessConfig{Type: "codex"})
	require.NoError(t, err)
	assert.Equal(t, "codex", called.Type)
}

func TestHarnessLabel(t *testing.T) {
	t.Parallel()
	assert.Empty(t, harnessLabel(nil))
	assert.Equal(t, "codex", harnessLabel(&latest.HarnessConfig{Type: "codex"}))
	assert.Equal(t, "claude-code/opus", harnessLabel(&latest.HarnessConfig{Type: "claude-code", Model: " opus "}))
}

func TestWithHarnessFactory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var called *latest.HarnessConfig
			r := &LocalRuntime{}
			WithHarnessFactory(func(cfg *latest.HarnessConfig) (harness.Provider, error) {
				called = cfg
				return nil, nil
			})(r)
			require.Nil(t, called, "factory must remain lazy")
			cfg := &latest.HarnessConfig{Type: name}
			_, err := r.newHarnessProvider(cfg)
			require.NoError(t, err)
			assert.Same(t, cfg, called)
		})
	}
}

func TestHarnessFactoryOverridesGlobal(t *testing.T) {
	original := harnessFactory.Load()
	t.Cleanup(func() { harnessFactory.Store(original) })

	r := &LocalRuntime{}
	globalCalls := 0
	RegisterHarness(func(*latest.HarnessConfig) (harness.Provider, error) {
		globalCalls++
		return nil, nil
	})
	_, err := r.newHarnessProvider(nil)
	require.NoError(t, err)
	assert.Equal(t, 1, globalCalls, "registration after construction remains supported")

	WithHarnessFactory(nil)(r)
	_, err = r.newHarnessProvider(nil)
	require.ErrorIs(t, err, ErrHarnessNotRegistered)
	assert.Equal(t, 1, globalCalls)

	localCalls := 0
	WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) {
		localCalls++
		return nil, nil
	})(r)
	_, err = r.newHarnessProvider(nil)
	require.NoError(t, err)
	assert.Equal(t, 1, localCalls)
	assert.Equal(t, 1, globalCalls)
}
