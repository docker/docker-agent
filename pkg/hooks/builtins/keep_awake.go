package builtins

import (
	"context"
	"sync"

	"github.com/rumpl/keepawake"

	"github.com/docker/docker-agent/pkg/hooks"
)

// KeepAwake is the registered name of the sleep-inhibition builtin.
const KeepAwake = "keep_awake"

// RegisterKeepAwake installs a session-scoped sleep inhibitor. The preference
// is read at session_start so changes apply without rebuilding the runtime.
func RegisterKeepAwake(r *hooks.Registry, enabled func() bool) (AutoInjector, error) {
	b := &keepAwakeBuiltin{enabled: enabled, acquire: keepawake.Acquire, releases: make(map[string]func() error)}
	if err := r.RegisterBuiltin(KeepAwake, b.hook); err != nil {
		return nil, err
	}
	return b, nil
}

type keepAwakeBuiltin struct {
	mu       sync.Mutex
	enabled  func() bool
	acquire  func(context.Context) (func() error, error)
	releases map[string]func() error
}

func (b *keepAwakeBuiltin) AutoInject(cfg *hooks.Config) {
	hook := hooks.Hook{Type: hooks.HookTypeBuiltin, Command: KeepAwake}
	cfg.SessionStart = append(cfg.SessionStart, hook)
	cfg.SessionEnd = append(cfg.SessionEnd, hook)
}

func (b *keepAwakeBuiltin) hook(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.SessionID == "" {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch in.HookEventName {
	case hooks.EventSessionStart:
		if b.releases[in.SessionID] != nil || !b.enabled() {
			return nil, nil
		}
		release, err := b.acquire(ctx)
		if err != nil {
			return nil, err
		}
		b.releases[in.SessionID] = release
	case hooks.EventSessionEnd:
		if release := b.releases[in.SessionID]; release != nil {
			delete(b.releases, in.SessionID)
			return nil, release()
		}
	}
	return nil, nil
}
