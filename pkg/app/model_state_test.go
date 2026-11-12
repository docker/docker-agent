package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type modelStateRuntime struct {
	mockRuntime

	setModelErr error
}

func (r *modelStateRuntime) SupportsModelSwitching() bool { return true }
func (r *modelStateRuntime) SetAgentModel(context.Context, string, string) error {
	return r.setModelErr
}

func TestAppModelStateSwitching(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.SetAgentModelOverride("other", "named-model")
	a := &App{runtime: &modelStateRuntime{mockRuntime: mockRuntime{store: store}}, session: sess}

	for _, ref := range []string{"openai/model-a", "openai/model-a", "named-model", "openai/model-b", ""} {
		require.NoError(t, a.SetCurrentAgentModel(ctx, ref))
		assert.Equal(t, ref, a.CurrentAgentModel(ctx))
		overrides, custom := sess.ModelStateSnapshot()
		assert.Equal(t, "named-model", overrides["other"])
		if ref == "" {
			assert.NotContains(t, overrides, "mock")
		} else {
			assert.Equal(t, ref, overrides["mock"])
		}
		persisted, err := store.GetSession(ctx, sess.ID)
		require.NoError(t, err)
		persistedOverrides, persistedCustom := persisted.ModelStateSnapshot()
		assert.Equal(t, overrides, persistedOverrides)
		assert.Equal(t, custom, persistedCustom)
	}
	_, custom := sess.ModelStateSnapshot()
	assert.Equal(t, []string{"openai/model-a", "openai/model-b"}, custom)
	models := a.AvailableModels(ctx)
	require.Len(t, models, 2)
	assert.False(t, models[0].IsCurrent)
	assert.False(t, models[1].IsCurrent)

	require.NoError(t, a.SetCurrentAgentModel(ctx, "openai/model-b"))
	models = a.AvailableModels(ctx)
	require.Len(t, models, 2)
	assert.False(t, models[0].IsCurrent)
	assert.True(t, models[1].IsCurrent)

	a.TrackCurrentAgentModel("tracked")
	assert.Equal(t, "tracked", a.CurrentAgentModel(ctx))
}

func TestAppModelStateRejectedSwitch(t *testing.T) {
	t.Parallel()

	for _, err := range []error{errors.New("invalid model"), runtime.ErrUnsupported} {
		t.Run(err.Error(), func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			sess.SetAgentModelOverride("mock", "openai/original")
			a := &App{runtime: &modelStateRuntime{setModelErr: err}, session: sess}
			require.Error(t, a.SetCurrentAgentModel(t.Context(), "openai/rejected"))
			overrides, custom := sess.ModelStateSnapshot()
			assert.Equal(t, map[string]string{"mock": "openai/original"}, overrides)
			assert.Equal(t, []string{"openai/original"}, custom)
		})
	}
}

func TestAppModelStateConcurrentReaders(t *testing.T) {
	t.Parallel()

	for _, reader := range []string{"persistence", "model picker"} {
		t.Run(reader, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := session.NewInMemorySessionStore()
			sess := session.New()
			a := &App{runtime: &modelStateRuntime{mockRuntime: mockRuntime{store: store}}, session: sess}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Go(func() {
				<-start
				for range 200 {
					if reader == "persistence" {
						assert.NoError(t, store.UpdateSession(ctx, sess))
					} else {
						_ = a.AvailableModels(ctx)
					}
				}
			})
			close(start)
			for range 200 {
				for _, ref := range []string{"openai/model-a", "openai/model-b", ""} {
					require.NoError(t, a.SetCurrentAgentModel(ctx, ref))
				}
			}
			wg.Wait()
			overrides, custom := sess.ModelStateSnapshot()
			assert.Empty(t, overrides)
			assert.Equal(t, []string{"openai/model-a", "openai/model-b"}, custom)
		})
	}
}
