package modelcatalog

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// discoveryCache caches both results and failures, but never caller cancellation.
// It intentionally differs from memoize, which retries every failed lookup.
type discoveryCache[T any] struct {
	mu        sync.Mutex
	sf        singleflight.Group
	value     T
	err       error
	fetchedAt time.Time
}

func (c *discoveryCache[T]) get(ctx context.Context, now func() time.Time, ttl time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	read := func() (T, error, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.value, c.err, !c.fetchedAt.IsZero() && now().Sub(c.fetchedAt) < ttl
	}
	if value, err, fresh := read(); fresh {
		return value, err
	}
	type result struct {
		value T
		err   error
	}
	v, _, _ := c.sf.Do("models", func() (any, error) {
		if value, err, fresh := read(); fresh {
			return result{value, err}, nil
		}
		value, err := fetch(ctx)
		if err == nil || ctx.Err() == nil {
			c.mu.Lock()
			c.value, c.err, c.fetchedAt = value, err, now()
			c.mu.Unlock()
		}
		return result{value, err}, nil
	})
	out := v.(result)
	return out.value, out.err
}

func (r *Catalog) clock() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}
