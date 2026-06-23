// Package memoize caches the results of expensive function calls, keyed by
// string, with an optional time-to-live. Concurrent calls for the same key are
// de-duplicated so the underlying function runs only once at a time.
//
// A failed call (one that returns a non-nil error) is never cached, so the next
// call retries. A panic in the memoized function is propagated to every caller
// waiting on the same key (the value is wrapped and re-raised by the underlying
// golang.org/x/sync/singleflight group).
//
// Expired entries are evicted lazily, when their key is next accessed or
// overwritten; there is no background janitor. Memory use is therefore bounded
// by the number of distinct keys, which suits callers that memoize a small,
// fixed set of keys.
package memoize

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// NoExpiration, used as the ttl passed to New, keeps cached values forever.
// Any ttl <= 0 has the same effect.
const NoExpiration time.Duration = -1

type entry[T any] struct {
	value   T
	expires time.Time // zero means the entry never expires
}

// Memoizer caches values of type T keyed by string. The zero value is not
// usable; create one with New. A Memoizer is safe for concurrent use by
// multiple goroutines.
type Memoizer[T any] struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]entry[T]
	group   singleflight.Group
}

// New creates a Memoizer whose cached values expire after ttl. A ttl <= 0
// (for example NoExpiration) keeps values forever.
func New[T any](ttl time.Duration) *Memoizer[T] {
	return &Memoizer[T]{
		ttl:     ttl,
		entries: make(map[string]entry[T]),
	}
}

// Memoize returns the cached value for key, computing it with fn on a miss or
// when the cached value has expired. Only one execution of fn is in-flight for
// a given key at a time; concurrent callers share its result. If fn returns an
// error, the result is not cached and the error is returned to the caller(s).
func (m *Memoizer[T]) Memoize(key string, fn func() (T, error)) (T, error) {
	if v, ok := m.load(key); ok {
		return v, nil
	}

	v, err, _ := m.group.Do(key, func() (any, error) {
		// Re-check under the singleflight key: a previous holder may have
		// populated the cache between our initial miss and acquiring the key,
		// which lets us skip a redundant call to fn.
		if v, ok := m.load(key); ok {
			return v, nil
		}
		v, err := fn()
		if err != nil {
			return v, err
		}
		m.store(key, v)
		return v, nil
	})

	// v is always a T (it originates from fn or from load), but use the
	// comma-ok form so a nil value of an interface T cannot panic here.
	result, _ := v.(T)
	return result, err
}

func (m *Memoizer[T]) load(key string) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[key]
	if !ok {
		var zero T
		return zero, false
	}
	if !e.expires.IsZero() && !time.Now().Before(e.expires) {
		delete(m.entries, key)
		var zero T
		return zero, false
	}
	return e.value, true
}

func (m *Memoizer[T]) store(key string, value T) {
	var expires time.Time
	if m.ttl > 0 {
		expires = time.Now().Add(m.ttl)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = entry[T]{value: value, expires: expires}
}
