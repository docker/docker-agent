package concurrent

import (
	"slices"
	"sync"
)

type Slice[V any] struct {
	mu     sync.RWMutex
	values []V
}

func NewSlice[V any]() *Slice[V] {
	return &Slice[V]{}
}

func (s *Slice[V]) Append(value V) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.values = append(s.values, value)
}

func (s *Slice[V]) Get(index int) (V, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if index < 0 || index >= len(s.values) {
		var zero V
		return zero, false
	}
	return s.values[index], true
}

func (s *Slice[V]) Set(index int, value V) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if index < 0 || index >= len(s.values) {
		return false
	}
	s.values[index] = value
	return true
}

func (s *Slice[V]) Length() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.values)
}

func (s *Slice[V]) All() []V {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return slices.Clone(s.values)
}

func (s *Slice[V]) Range(f func(index int, value V) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i, v := range s.values {
		if !f(i, v) {
			break
		}
	}
}

func (s *Slice[V]) Find(predicate func(V) bool) (V, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i, v := range s.values {
		if predicate(v) {
			return v, i
		}
	}
	var zero V
	return zero, -1
}

func (s *Slice[V]) Update(index int, f func(V) V) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if index < 0 || index >= len(s.values) {
		return false
	}
	s.values[index] = f(s.values[index])
	return true
}

func (s *Slice[V]) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.values = nil
}
