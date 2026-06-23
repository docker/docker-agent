package runtime

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// lazyModelStore is the default ModelStore wired in when the caller did not
// pass WithModelStore. It defers constructing the modelsdev store (which calls
// os.UserHomeDir and creates the ~/.cagent cache directory) until the first
// method invocation. This keeps NewLocalRuntime free of disk I/O — tests
// that never touch the catalog can build a runtime without paying the cost
// or hitting failure modes that depend on the host filesystem.
//
// The store is created with WithKnownProvider so that resolving a model for a
// user-defined custom provider never triggers an outbound models.dev fetch.
// Without it the request loop (loop.go) and session compaction would block on
// a doomed GET to models.dev for every custom-provider model in an
// internet-restricted environment (issue #3165).
//
// Each runtime gets its own *Store; the per-runtime sync.Once only defers
// construction, it does not share catalog state across runtimes.
type lazyModelStore struct {
	once sync.Once
	st   *modelsdev.Store
	err  error
}

func (l *lazyModelStore) load() (*modelsdev.Store, error) {
	l.once.Do(func() {
		l.st, l.err = modelsdev.NewStore(modelsdev.WithKnownProvider(provider.IsKnownProvider))
	})
	return l.st, l.err
}

func (l *lazyModelStore) GetModel(ctx context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	st, err := l.load()
	if err != nil {
		return nil, err
	}
	return st.GetModel(ctx, id)
}

func (l *lazyModelStore) GetDatabase(ctx context.Context) (*modelsdev.Database, error) {
	st, err := l.load()
	if err != nil {
		return nil, err
	}
	return st.GetDatabase(ctx)
}
