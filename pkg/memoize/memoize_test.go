package memoize

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoizeCachesValue(t *testing.T) {
	m := New[int](NoExpiration)

	var calls atomic.Int32
	compute := func() (int, error) {
		calls.Add(1)
		return 42, nil
	}

	for range 3 {
		v, err := m.Memoize("key", compute)
		require.NoError(t, err)
		assert.Equal(t, 42, v)
	}
	assert.Equal(t, int32(1), calls.Load())
}

func TestMemoizeCachesZeroValue(t *testing.T) {
	m := New[int](NoExpiration)

	var calls atomic.Int32
	compute := func() (int, error) {
		calls.Add(1)
		return 0, nil
	}

	for range 3 {
		v, err := m.Memoize("key", compute)
		require.NoError(t, err)
		assert.Equal(t, 0, v)
	}
	assert.Equal(t, int32(1), calls.Load(), "a zero value must still be cached")
}

func TestMemoizeIsolatesKeys(t *testing.T) {
	m := New[string](NoExpiration)

	a, err := m.Memoize("a", func() (string, error) { return "value-a", nil })
	require.NoError(t, err)
	b, err := m.Memoize("b", func() (string, error) { return "value-b", nil })
	require.NoError(t, err)

	assert.Equal(t, "value-a", a)
	assert.Equal(t, "value-b", b)
}

func TestMemoizeDoesNotCacheErrors(t *testing.T) {
	m := New[int](NoExpiration)

	var calls atomic.Int32
	wantErr := errors.New("boom")
	compute := func() (int, error) {
		calls.Add(1)
		return 0, wantErr
	}

	_, err := m.Memoize("key", compute)
	require.ErrorIs(t, err, wantErr)
	_, err = m.Memoize("key", compute)
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, int32(2), calls.Load())
}

func TestMemoizeRetriesAfterErrorThenCaches(t *testing.T) {
	m := New[int](NoExpiration)

	var calls atomic.Int32
	compute := func() (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("transient")
		}
		return 99, nil
	}

	_, err := m.Memoize("key", compute)
	require.Error(t, err)

	v, err := m.Memoize("key", compute)
	require.NoError(t, err)
	assert.Equal(t, 99, v)

	v, err = m.Memoize("key", compute)
	require.NoError(t, err)
	assert.Equal(t, 99, v)
	assert.Equal(t, int32(2), calls.Load(), "value must be cached after first success")
}

func TestMemoizeExpires(t *testing.T) {
	m := New[int](10 * time.Millisecond)

	var calls atomic.Int32
	compute := func() (int, error) {
		calls.Add(1)
		return int(calls.Load()), nil
	}

	v, err := m.Memoize("key", compute)
	require.NoError(t, err)
	assert.Equal(t, 1, v)

	time.Sleep(20 * time.Millisecond)

	v, err = m.Memoize("key", compute)
	require.NoError(t, err)
	assert.Equal(t, 2, v)
}

// TestMemoizeZeroTTLNeverExpires verifies the go-cache compatible behavior that
// a non-positive ttl caches forever rather than expiring immediately.
func TestMemoizeZeroTTLNeverExpires(t *testing.T) {
	for _, ttl := range []time.Duration{0, NoExpiration, -time.Hour} {
		m := New[int](ttl)

		var calls atomic.Int32
		compute := func() (int, error) {
			calls.Add(1)
			return 7, nil
		}

		v, err := m.Memoize("key", compute)
		require.NoError(t, err)
		assert.Equal(t, 7, v)

		time.Sleep(5 * time.Millisecond)

		v, err = m.Memoize("key", compute)
		require.NoError(t, err)
		assert.Equal(t, 7, v)
		assert.Equal(t, int32(1), calls.Load(), "ttl=%v must never expire", ttl)
	}
}

// TestMemoizeEvictsExpiredEntry ensures expired entries are removed on access,
// so memory does not grow without bound for keys that stop being requested.
func TestMemoizeEvictsExpiredEntry(t *testing.T) {
	m := New[int](5 * time.Millisecond)

	_, err := m.Memoize("key", func() (int, error) { return 1, nil })
	require.NoError(t, err)
	assert.Len(t, m.entries, 1)

	time.Sleep(10 * time.Millisecond)

	_, ok := m.load("key")
	assert.False(t, ok)
	assert.Empty(t, m.entries, "expired entry must be evicted on access")
}

func TestMemoizePanicPropagates(t *testing.T) {
	m := New[int](NoExpiration)

	// singleflight wraps the panic value and re-raises it, so we assert that a
	// panic propagates (matching go-memoize) rather than the exact value.
	assert.Panics(t, func() {
		_, _ = m.Memoize("key", func() (int, error) {
			panic("kaboom")
		})
	})

	// After a panic the key must not be cached: a subsequent successful call works.
	v, err := m.Memoize("key", func() (int, error) { return 5, nil })
	require.NoError(t, err)
	assert.Equal(t, 5, v)
	assert.Len(t, m.entries, 1)
}

// TestMemoizeNilInterfaceValue guards the type assertion: when T is an
// interface and fn returns nil, Memoize must return nil without panicking.
func TestMemoizeNilInterfaceValue(t *testing.T) {
	m := New[fmt.Stringer](NoExpiration)

	v, err := m.Memoize("key", func() (fmt.Stringer, error) {
		return nil, nil
	})
	require.NoError(t, err)
	assert.Nil(t, v)
}

func TestMemoizeConcurrentSingleFlight(t *testing.T) {
	m := New[int](NoExpiration)

	var calls atomic.Int32

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			v, err := m.Memoize("key", func() (int, error) {
				calls.Add(1)
				time.Sleep(20 * time.Millisecond)
				return 7, nil
			})
			require.NoError(t, err)
			assert.Equal(t, 7, v)
		})
	}
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load())
}

// TestMemoizeConcurrentDistinctKeys exercises the lock under contention across
// many keys to catch data races (run with -race).
func TestMemoizeConcurrentDistinctKeys(t *testing.T) {
	m := New[int](time.Millisecond)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			key := string(rune('a' + i%5))
			for range 20 {
				_, err := m.Memoize(key, func() (int, error) {
					return i, nil
				})
				require.NoError(t, err)
			}
		})
	}
	wg.Wait()
}
