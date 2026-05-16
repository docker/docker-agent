package markdown

import "container/list"

// lruCache is a simple LRU (Least Recently Used) cache with a fixed maximum size.
// It is NOT safe for concurrent use; callers must provide their own synchronization.
type lruCache[K comparable, V any] struct {
	maxSize int
	items   map[K]*list.Element
	order   *list.List // front = most recently used
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

// newLRUCache creates an LRU cache that holds at most maxSize entries.
func newLRUCache[K comparable, V any](maxSize int) *lruCache[K, V] {
	return &lruCache[K, V]{
		maxSize: maxSize,
		items:   make(map[K]*list.Element, maxSize),
		order:   list.New(),
	}
}

// get retrieves a value from the cache, promoting it to most-recently-used.
// Returns the value and true if found, or the zero value and false otherwise.
func (c *lruCache[K, V]) get(key K) (V, bool) {
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		return elem.Value.(*lruEntry[K, V]).value, true
	}
	var zero V
	return zero, false
}

// put adds or updates a key-value pair in the cache.
// If the cache is at capacity, the least recently used entry is evicted.
func (c *lruCache[K, V]) put(key K, value V) {
	if elem, ok := c.items[key]; ok {
		// Update existing entry
		c.order.MoveToFront(elem)
		elem.Value.(*lruEntry[K, V]).value = value
		return
	}

	// Evict if at capacity
	if c.order.Len() >= c.maxSize {
		c.evictOldest()
	}

	entry := &lruEntry[K, V]{key: key, value: value}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
}

// clear removes all entries from the cache.
func (c *lruCache[K, V]) clear() {
	c.items = make(map[K]*list.Element, c.maxSize)
	c.order.Init()
}

// evictOldest removes the least recently used entry.
func (c *lruCache[K, V]) evictOldest() {
	oldest := c.order.Back()
	if oldest == nil {
		return
	}
	c.order.Remove(oldest)
	delete(c.items, oldest.Value.(*lruEntry[K, V]).key)
}
