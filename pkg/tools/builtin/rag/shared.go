package rag

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// defaultSharedRegistry lets every reference to one RAG definition run a
// single index, file watcher and embedding queue instead of one per agent.
var defaultSharedRegistry = newSharedRegistry()

// sharedKey identifies every input that builds a RAG manager. Identical
// inputs resolve to the same database, so sharing is safe; the env provider
// and provider registry are compared by identity so references never share
// an embedding client across different credentials. A zero key never shares.
type sharedKey struct {
	config   string
	env      environment.Provider
	registry *provider.Registry
}

func (k sharedKey) shareable() bool { return k.config != "" }

func sharedKeyFor(ragName string, ragCfg *latest.RAGConfig, parentDir string, rc *config.RuntimeConfig) (sharedKey, bool) {
	env := rc.EnvProvider()
	if env != nil && !reflect.TypeOf(env).Comparable() {
		return sharedKey{}, false
	}

	dir := parentDir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	fingerprint, err := json.Marshal(struct {
		Name      string
		ParentDir string
		Gateway   string
		RAG       *latest.RAGConfig
		Models    map[string]latest.ModelConfig
		Providers map[string]latest.ProviderConfig
	}{ragName, dir, rc.ModelsGateway, ragCfg, rc.Models, rc.Providers})
	if err != nil {
		return sharedKey{}, false
	}
	return sharedKey{config: string(fingerprint), env: env, registry: rc.ProviderRegistry}, true
}

type sharedRegistry struct {
	mu      sync.Mutex
	entries map[sharedKey]*sharedEntry
}

func newSharedRegistry() *sharedRegistry {
	return &sharedRegistry{entries: make(map[sharedKey]*sharedEntry)}
}

// sharedEntry is a started toolset serving every reference that acquired it.
type sharedEntry struct {
	ts   *ToolSet
	refs int // guarded by sharedRegistry.mu

	mu      sync.Mutex // serializes Start and Stop of ts
	started bool
}

// acquire returns the entry for key, installing own when none is live.
func (r *sharedRegistry) acquire(key sharedKey, own *ToolSet) *sharedEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		e = &sharedEntry{ts: own}
		r.entries[key] = e
	}
	e.refs++
	return e
}

// release drops one reference and reports whether it was the last.
func (r *sharedRegistry) release(key sharedKey, e *sharedEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.refs--
	if e.refs > 0 {
		return false
	}
	if r.entries[key] == e {
		delete(r.entries, key)
	}
	return true
}

// serving reports whether ts backs a live entry for key.
func (r *sharedRegistry) serving(key sharedKey, ts *ToolSet) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	return ok && e.ts == ts
}

func (e *sharedEntry) start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}
	// The index outlives the starting session and serves others: keep the
	// context's logging and tracing but not its session identity.
	ctx = httpclient.ContextWithSessionID(ctx, "")
	if err := e.ts.Start(ctx); err != nil {
		return err
	}
	e.started = true
	return nil
}

func (e *sharedEntry) stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return nil
	}
	e.started = false
	return e.ts.Stop(ctx)
}

// sharedToolSet is one agent's reference to a possibly shared RAG toolset.
// Each reference builds its own toolset at load time, as before; on Start the
// first reference installs it and later ones attach to it instead of
// indexing again. A reference that never starts never enters the registry.
type sharedToolSet struct {
	reg *sharedRegistry
	key sharedKey
	own *ToolSet

	// lifeMu serializes Start and Stop, which may block on indexing; mu
	// only guards entry so Tools never waits for an index to build.
	lifeMu      sync.Mutex
	mu          sync.Mutex
	entry       *sharedEntry
	unsubTarget func()
	ownStarted  bool // started outside the registry (unshareable key)
	ownStopped  bool

	subscribers tools.Subscribers[ragtypes.Event]
	legacyMu    sync.Mutex
	unsubLegacy func()
}

var (
	_ tools.ToolSet            = (*sharedToolSet)(nil)
	_ tools.Instructable       = (*sharedToolSet)(nil)
	_ tools.Startable          = (*sharedToolSet)(nil)
	_ ragtypes.EventForwarder  = (*sharedToolSet)(nil)
	_ ragtypes.EventSubscriber = (*sharedToolSet)(nil)
)

func newSharedToolSet(reg *sharedRegistry, key sharedKey, own *ToolSet) *sharedToolSet {
	return &sharedToolSet{reg: reg, key: key, own: own}
}

func (s *sharedToolSet) Name() string { return s.own.Name() }

func (s *sharedToolSet) Instructions() string { return s.own.Instructions() }

// Tools serves the toolset that was actually initialized: a reference that
// attached never indexed its own manager.
func (s *sharedToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	return s.target().Tools(ctx)
}

func (s *sharedToolSet) target() *ToolSet {
	if e := s.currentEntry(); e != nil {
		return e.ts
	}
	return s.own
}

func (s *sharedToolSet) currentEntry() *sharedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entry
}

func (s *sharedToolSet) setEntry(e *sharedEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry = e
}

func (s *sharedToolSet) Start(ctx context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.currentEntry() != nil || s.ownStarted {
		return nil
	}
	if !s.key.shareable() {
		if err := s.own.Start(ctx); err != nil {
			return err
		}
		s.ownStarted = true
		s.forwardFrom(s.own)
		return nil
	}

	e := s.reg.acquire(s.key, s.own)
	if err := e.start(ctx); err != nil {
		s.reg.release(s.key, e)
		return err
	}
	s.setEntry(e)
	s.forwardFrom(e.ts)
	return nil
}

func (s *sharedToolSet) Stop(ctx context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.unsubTarget != nil {
		s.unsubTarget()
		s.unsubTarget = nil
	}

	var err error
	if e := s.currentEntry(); e != nil {
		s.setEntry(nil)
		if s.reg.release(s.key, e) {
			err = e.stop(ctx)
			if e.ts == s.own {
				s.ownStopped = true
				return err
			}
		}
	}
	if s.ownStopped {
		return err
	}
	// Still backing other references: the entry closes it on last release.
	s.ownStopped = true
	if s.reg.serving(s.key, s.own) {
		return err
	}
	s.ownStarted = false
	return errors.Join(err, s.own.Stop(ctx))
}

// forwardFrom relays the started toolset's events to this reference's
// subscribers. Callers hold s.lifeMu.
func (s *sharedToolSet) forwardFrom(ts *ToolSet) {
	s.unsubTarget = ts.SubscribeEvents(s.subscribers.Notify)
}

func (s *sharedToolSet) SetEventCallback(cb EventCallback) {
	s.legacyMu.Lock()
	defer s.legacyMu.Unlock()
	if s.unsubLegacy != nil {
		s.unsubLegacy()
	}
	s.unsubLegacy = s.subscribers.Subscribe(cb)
}

func (s *sharedToolSet) SubscribeEvents(cb EventCallback) func() {
	return s.subscribers.Subscribe(cb)
}
