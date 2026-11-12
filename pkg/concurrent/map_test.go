package concurrent

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMap_StoreLoad(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()

	m.Store("a", 1)
	m.Store("b", 2)

	val, ok := m.Load("a")
	assert.True(t, ok)
	assert.Equal(t, 1, val)

	val, ok = m.Load("b")
	assert.True(t, ok)
	assert.Equal(t, 2, val)

	_, ok = m.Load("missing")
	assert.False(t, ok)
}

func TestMap_StoreOverwrites(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()
	m.Store("k", 1)
	m.Store("k", 2)

	val, ok := m.Load("k")
	assert.True(t, ok)
	assert.Equal(t, 2, val)
	assert.Equal(t, 1, m.Length())
}

func TestMap_Delete(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)

	m.Delete("a")
	_, ok := m.Load("a")
	assert.False(t, ok)
	assert.Equal(t, 1, m.Length())

	// Deleting a missing key is a no-op.
	m.Delete("missing")
	assert.Equal(t, 1, m.Length())
}

func TestMap_Length(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()
	assert.Equal(t, 0, m.Length())

	m.Store("a", 1)
	m.Store("b", 2)
	m.Store("c", 3)
	assert.Equal(t, 3, m.Length())
}

func TestMap_Range(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)
	m.Store("c", 3)

	collected := map[string]int{}
	m.Range(func(k string, v int) bool {
		collected[k] = v
		return true
	})
	assert.Equal(t, map[string]int{"a": 1, "b": 2, "c": 3}, collected)

	// Early termination: stop after the first element.
	count := 0
	m.Range(func(_ string, _ int) bool {
		count++
		return false
	})
	assert.Equal(t, 1, count)
}

func TestMap_RangeCallbackCanMutate(t *testing.T) {
	t.Parallel()
	// Range iterates over a snapshot, so callbacks may safely mutate the map
	// without deadlocking.
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)

	m.Range(func(k string, _ int) bool {
		m.Store(k+"!", 0)
		return true
	})

	assert.Equal(t, 4, m.Length())
}

func TestMap_ZeroValueStore(t *testing.T) {
	t.Parallel()
	// The zero value of Map must be usable: Store should lazily initialise
	// the underlying map instead of panicking.
	var m Map[string, int]
	m.Store("a", 1)

	val, ok := m.Load("a")
	assert.True(t, ok)
	assert.Equal(t, 1, val)
}

func TestMap_LoadOrStore(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()

	val, loaded := m.LoadOrStore("a", 1)
	assert.False(t, loaded)
	assert.Equal(t, 1, val)

	val, loaded = m.LoadOrStore("a", 2)
	assert.True(t, loaded)
	assert.Equal(t, 1, val)

	// The original value is preserved even after a same-key LoadOrStore.
	val, ok := m.Load("a")
	assert.True(t, ok)
	assert.Equal(t, 1, val)
}

func TestMap_LoadOrStoreZeroValue(t *testing.T) {
	t.Parallel()
	// The zero value of Map must be usable for LoadOrStore as well.
	var m Map[string, int]
	val, loaded := m.LoadOrStore("a", 42)
	assert.False(t, loaded)
	assert.Equal(t, 42, val)
}

func TestMap_LoadOrStoreConcurrent(t *testing.T) {
	t.Parallel()
	// Concurrent LoadOrStore calls for the same key must all return the
	// same value, with exactly one of them reporting loaded == false.
	m := NewMap[int, int]()
	var wg sync.WaitGroup
	const writers = 100

	values := make([]int, writers)
	loadedFlags := make([]bool, writers)
	for i := range writers {
		wg.Go(func() {
			val, loaded := m.LoadOrStore(0, i)
			values[i] = val
			loadedFlags[i] = loaded
		})
	}
	wg.Wait()

	first := values[0]
	newCount := 0
	for i := range writers {
		require.Equal(t, first, values[i])
		if !loadedFlags[i] {
			newCount++
		}
	}
	require.Equal(t, 1, newCount, "exactly one caller should report loaded == false")
}

func TestMap_Concurrent(t *testing.T) {
	t.Parallel()
	m := NewMap[int, int]()
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Go(func() {
			m.Store(i, i*2)
		})
	}

	wg.Wait()
	require.Equal(t, 100, m.Length())

	for i := range 100 {
		val, ok := m.Load(i)
		require.True(t, ok)
		require.Equal(t, i*2, val)
	}
}

func TestMap_Clear(t *testing.T) {
	t.Parallel()
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)
	m.Store("c", 3)

	m.Clear()
	assert.Equal(t, 0, m.Length())

	_, ok := m.Load("a")
	assert.False(t, ok)

	// Map is usable after Clear.
	m.Store("d", 4)
	val, ok := m.Load("d")
	assert.True(t, ok)
	assert.Equal(t, 4, val)
}

func TestMap_ClearZeroValue(t *testing.T) {
	t.Parallel()
	var m Map[string, int]
	m.Clear() // should not panic on zero-value map
	assert.Equal(t, 0, m.Length())
}
