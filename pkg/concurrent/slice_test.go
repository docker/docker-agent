package concurrent

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlice_Append(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()

	s.Append(1)
	s.Append(2)
	s.Append(3)

	assert.Equal(t, 3, s.Length())
	assert.Equal(t, []int{1, 2, 3}, s.All())
}

func TestSlice_Get(t *testing.T) {
	t.Parallel()
	s := NewSlice[string]()
	s.Append("a")
	s.Append("b")

	val, ok := s.Get(0)
	assert.True(t, ok)
	assert.Equal(t, "a", val)

	val, ok = s.Get(1)
	assert.True(t, ok)
	assert.Equal(t, "b", val)

	_, ok = s.Get(-1)
	assert.False(t, ok)

	_, ok = s.Get(2)
	assert.False(t, ok)
}

func TestSlice_Set(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()
	s.Append(1)
	s.Append(2)

	ok := s.Set(0, 10)
	assert.True(t, ok)

	val, _ := s.Get(0)
	assert.Equal(t, 10, val)

	ok = s.Set(-1, 100)
	assert.False(t, ok)

	ok = s.Set(5, 100)
	assert.False(t, ok)
}

func TestSlice_All(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()
	s.Append(1)
	s.Append(2)

	all := s.All()
	assert.Equal(t, []int{1, 2}, all)

	// Verify it's a copy
	all[0] = 100
	val, _ := s.Get(0)
	assert.Equal(t, 1, val)
}

func TestSlice_AllEmpty(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()
	all := s.All()
	assert.NotNil(t, all)
	assert.Empty(t, all)
}

func TestSlice_Range(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()
	s.Append(10)
	s.Append(20)
	s.Append(30)

	var collected []int
	s.Range(func(_, v int) bool {
		collected = append(collected, v)
		return true
	})
	assert.Equal(t, []int{10, 20, 30}, collected)

	// Test early termination
	collected = nil
	s.Range(func(i, v int) bool {
		collected = append(collected, v)
		return i < 1
	})
	assert.Equal(t, []int{10, 20}, collected)
}

func TestSlice_Find(t *testing.T) {
	t.Parallel()
	s := NewSlice[string]()
	s.Append("apple")
	s.Append("banana")
	s.Append("cherry")

	val, idx := s.Find(func(v string) bool { return v == "banana" })
	assert.Equal(t, 1, idx)
	assert.Equal(t, "banana", val)

	_, idx = s.Find(func(v string) bool { return v == "grape" })
	assert.Equal(t, -1, idx)
}

func TestSlice_Update(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()
	s.Append(1)
	s.Append(2)

	ok := s.Update(0, func(v int) int { return v * 10 })
	assert.True(t, ok)

	val, _ := s.Get(0)
	assert.Equal(t, 10, val)

	ok = s.Update(5, func(v int) int { return v * 10 })
	assert.False(t, ok)
}

func TestSlice_Concurrent(t *testing.T) {
	t.Parallel()
	s := NewSlice[int]()
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Go(func() {
			s.Append(i)
		})
	}

	wg.Wait()
	require.Equal(t, 100, s.Length())
}
