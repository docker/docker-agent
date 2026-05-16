package runtime

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// lazyModelStore is the default ModelStore wired in when the caller did not
// pass WithModelStore. It defers modelsdev.NewStore() (which calls
// os.UserHomeDir and creates the ~/.cagent cache directory) until the first
// method invocation. This keeps NewLocalRuntime free of disk I/O — tests
// that never touch the catalog can build a runtime without paying the cost
// or hitting failure modes that depend on the host filesystem.
//
// The underlying modelsdev.NewStore is itself memoized via sync.OnceValues,
// so wrapping it in a per-runtime sync.Once does not change cross-runtime
// caching semantics; it simply isolates the failure of the home-dir lookup
// to the first caller that actually needs catalog data.
type lazyModelStore struct {
	once sync.Once
	st   *modelsdev.Store
	err  error
}

func (l *lazyModelStore) load() (*modelsdev.Store, error) {
	l.once.Do(func() {
		l.st, l.err = modelsdev.NewStore()
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
