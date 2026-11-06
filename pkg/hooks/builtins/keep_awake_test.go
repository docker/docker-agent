package builtins

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

func TestKeepAwakeLifecycle(t *testing.T) {
	t.Parallel()
	enabled := false
	acquired, released := 0, 0
	b := &keepAwakeBuiltin{
		enabled: func() bool { return enabled },
		acquire: func(context.Context) (func() error, error) {
			acquired++
			return func() error { released++; return nil }, nil
		},
		releases: make(map[string]func() error),
	}
	fire := func(event hooks.EventType, id string) {
		t.Helper()
		_, err := b.hook(t.Context(), &hooks.Input{HookEventName: event, SessionID: id}, nil)
		require.NoError(t, err)
	}
	fire(hooks.EventSessionStart, "disabled")
	assert.Zero(t, acquired)
	enabled = true
	fire(hooks.EventSessionStart, "first")
	fire(hooks.EventSessionStart, "first")
	fire(hooks.EventSessionStart, "second")
	assert.Equal(t, 2, acquired)
	// Turning off the preference must not suppress cleanup of existing runs.
	enabled = false
	fire(hooks.EventSessionEnd, "first")
	fire(hooks.EventSessionEnd, "first")
	assert.Equal(t, 1, released)
	fire(hooks.EventSessionEnd, "second")
	assert.Equal(t, 2, released)
	assert.Empty(t, b.releases)
}

func TestKeepAwakeFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("inhibitor unavailable")
	b := &keepAwakeBuiltin{
		enabled:  func() bool { return true },
		acquire:  func(context.Context) (func() error, error) { return nil, want },
		releases: make(map[string]func() error),
	}
	_, err := b.hook(t.Context(), &hooks.Input{HookEventName: hooks.EventSessionStart, SessionID: "s"}, nil)
	require.ErrorIs(t, err, want)
	assert.Empty(t, b.releases)
}

func TestKeepAwakeRegistration(t *testing.T) {
	t.Parallel()
	reg := hooks.NewRegistry()
	inj, err := RegisterKeepAwake(reg, func() bool { return false })
	require.NoError(t, err)
	cfg := &hooks.Config{}
	inj.AutoInject(cfg)
	require.Len(t, cfg.SessionStart, 1)
	require.Len(t, cfg.SessionEnd, 1)
	exec := hooks.NewExecutorWithRegistry(cfg, "", nil, reg)
	for _, event := range []hooks.EventType{hooks.EventSessionStart, hooks.EventSessionEnd} {
		res, err := exec.Dispatch(t.Context(), event, &hooks.Input{SessionID: "s"})
		require.NoError(t, err)
		assert.True(t, res.Allowed)
		assert.Empty(t, res.FailedHooks)
	}
}
