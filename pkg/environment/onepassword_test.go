package environment

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOnePasswordProvider_Get(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		stored      map[string]string
		resolve     func(ctx context.Context, reference string) (string, bool)
		lookup      string
		wantValue   string
		wantFound   bool
		wantRefSeen string
	}{
		{
			name:      "plain value is passed through",
			stored:    map[string]string{"API_KEY": "plain-secret"},
			lookup:    "API_KEY",
			wantValue: "plain-secret",
			wantFound: true,
		},
		{
			name:        "op reference is resolved",
			stored:      map[string]string{"API_KEY": "op://vault/item/field"},
			lookup:      "API_KEY",
			wantValue:   "resolved-secret",
			wantFound:   true,
			wantRefSeen: "op://vault/item/field",
		},
		{
			name:      "missing variable is not resolved",
			stored:    map[string]string{},
			lookup:    "API_KEY",
			wantFound: false,
		},
		{
			name:   "failed resolution yields empty value but stays found",
			stored: map[string]string{"API_KEY": "op://vault/item/field"},
			resolve: func(context.Context, string) (string, bool) {
				return "", false
			},
			lookup:    "API_KEY",
			wantValue: "",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var refSeen string
			resolve := tt.resolve
			if resolve == nil {
				resolve = func(_ context.Context, reference string) (string, bool) {
					refSeen = reference
					return "resolved-secret", true
				}
			}

			provider := &OnePasswordProvider{
				provider: NewMapEnvProvider(tt.stored),
				resolve:  resolve,
			}

			value, found := provider.Get(t.Context(), tt.lookup)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantValue, value)
			if tt.wantRefSeen != "" {
				assert.Equal(t, tt.wantRefSeen, refSeen)
			}
		})
	}
}

func TestOnePasswordProvider_CachesResolvedReferences(t *testing.T) {
	t.Parallel()

	var calls int
	provider := &OnePasswordProvider{
		provider: NewMapEnvProvider(map[string]string{"API_KEY": "op://vault/item/field"}),
		resolve: func(context.Context, string) (string, bool) {
			calls++
			return "resolved-secret", true
		},
	}

	for range 3 {
		value, found := provider.Get(t.Context(), "API_KEY")
		assert.True(t, found)
		assert.Equal(t, "resolved-secret", value)
	}

	assert.Equal(t, 1, calls, "reference should only be resolved once")
}

func TestOnePasswordProvider_CachesFailedResolutions(t *testing.T) {
	t.Parallel()

	var calls int
	provider := &OnePasswordProvider{
		provider: NewMapEnvProvider(map[string]string{"API_KEY": "op://vault/item/field"}),
		resolve: func(context.Context, string) (string, bool) {
			calls++
			return "", false
		},
	}

	for range 3 {
		value, found := provider.Get(t.Context(), "API_KEY")
		assert.True(t, found)
		assert.Empty(t, value)
	}

	assert.Equal(t, 1, calls, "failed resolution should only be attempted once")
}

func TestOnePasswordProvider_DoesNotCacheContextCancelledFailures(t *testing.T) {
	t.Parallel()

	var calls int
	provider := &OnePasswordProvider{
		provider: NewMapEnvProvider(map[string]string{"API_KEY": "op://vault/item/field"}),
		resolve: func(ctx context.Context, _ string) (string, bool) {
			calls++
			if ctx.Err() != nil {
				return "", false
			}
			return "resolved-secret", true
		},
	}

	// A first lookup under a cancelled context must not poison the cache.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	value, found := provider.Get(cancelled, "API_KEY")
	assert.True(t, found)
	assert.Empty(t, value)

	// A subsequent lookup with a healthy context should retry and succeed.
	value, found = provider.Get(t.Context(), "API_KEY")
	assert.True(t, found)
	assert.Equal(t, "resolved-secret", value)
	assert.Equal(t, 2, calls, "cancelled failure must not be cached")
}

func TestOnePasswordProvider_ConcurrentLookups(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := map[string]int{}
	provider := &OnePasswordProvider{
		provider: NewMapEnvProvider(map[string]string{
			"A": "op://vault/a/field",
			"B": "op://vault/b/field",
		}),
		resolve: func(_ context.Context, reference string) (string, bool) {
			mu.Lock()
			calls[reference]++
			mu.Unlock()
			return "value-for-" + reference, true
		},
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			value, found := provider.Get(t.Context(), "A")
			assert.True(t, found)
			assert.Equal(t, "value-for-op://vault/a/field", value)
		})
		wg.Go(func() {
			value, found := provider.Get(t.Context(), "B")
			assert.True(t, found)
			assert.Equal(t, "value-for-op://vault/b/field", value)
		})
	}
	wg.Wait()

	assert.Equal(t, 1, calls["op://vault/a/field"], "reference A should resolve once")
	assert.Equal(t, 1, calls["op://vault/b/field"], "reference B should resolve once")
}

func TestNewOnePasswordProvider_AlwaysWraps(t *testing.T) {
	t.Parallel()

	// The constructor must always wrap so that "op://" references are never
	// silently passed through as if they were real secrets, regardless of
	// whether the `op` binary is installed on the host.
	base := NewMapEnvProvider(map[string]string{"API_KEY": "plain"})
	provider := NewOnePasswordProvider(base)

	_, ok := provider.(*OnePasswordProvider)
	assert.True(t, ok)
}
