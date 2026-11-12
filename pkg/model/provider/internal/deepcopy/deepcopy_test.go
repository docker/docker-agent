package deepcopy

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValuePreservesTypesAndIsolatesReferences(t *testing.T) {
	t.Parallel()
	type number int64
	type plain struct {
		Value  *int64
		hidden int
	}
	shared := map[string]any{"items": []any{int64(9007199254740993)}}
	source := map[string]any{
		"int64": int64(9007199254740993), "uint64": uint64(math.MaxUint64),
		"int8": int8(-7), "float32": float32(0.1), "number": json.Number("9007199254740993"),
		"named": number(9007199254740993), "array": [1]*int64{new(int64(7))},
		"struct": plain{Value: new(int64(9)), hidden: 4},
		"first":  shared, "second": shared, "nil": nil,
		"typedNil": (*int)(nil), "nilMap": map[string]any(nil), "empty": []any{},
	}
	copied, err := Value(source)
	require.NoError(t, err)
	require.Equal(t, source, copied)
	for key, value := range source {
		assert.Equal(t, reflect.TypeOf(value), reflect.TypeOf(copied[key]))
	}
	copied["first"].(map[string]any)["items"].([]any)[0] = int64(0)
	assert.Equal(t, int64(9007199254740993), shared["items"].([]any)[0])
	assert.Equal(t, int64(9007199254740993), copied["second"].(map[string]any)["items"].([]any)[0])
	*copied["array"].([1]*int64)[0] = 0
	*copied["struct"].(plain).Value = 0
	assert.Equal(t, int64(7), *source["array"].([1]*int64)[0])
	assert.Equal(t, int64(9), *source["struct"].(plain).Value)
	var absent any
	got, err := Value(absent)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestValuePreservesNonFiniteBits(t *testing.T) {
	t.Parallel()
	for _, bits := range []uint64{0x7ff8000000000042, math.Float64bits(math.Inf(1)), math.Float64bits(math.Copysign(0, -1))} {
		source := map[string]any{"value": math.Float64frombits(bits)}
		copied, err := Value(source)
		require.NoError(t, err)
		assert.Equal(t, bits, math.Float64bits(copied["value"].(float64)))
	}
}

func TestValueRejectsUnsupportedValues(t *testing.T) {
	t.Parallel()
	type private struct{ hidden map[string]any }
	hidden := private{hidden: map[string]any{"value": 1}}
	require.NotNil(t, hidden.hidden)
	for _, value := range []any{func() {}, make(chan int), unsafe.Pointer(new(int)), hidden} {
		copied, err := Value(map[string]any{"nested": []any{value}})
		require.ErrorIs(t, err, ErrUnsupported)
		require.ErrorContains(t, err, "$[nested][0]")
		assert.Nil(t, copied)
	}
}

func TestValueRejectsCycles(t *testing.T) {
	t.Parallel()
	cycle := map[string]any{}
	cycle["self"] = cycle
	slice := []any{nil}
	slice[0] = slice
	type node struct{ Next *node }
	pointer := &node{}
	pointer.Next = pointer
	for _, value := range []any{cycle, slice, pointer} {
		copied, err := Value(value)
		require.ErrorIs(t, err, ErrCycle)
		assert.Nil(t, copied)
	}
}

func TestValueBoundsDepthAndBreadth(t *testing.T) {
	t.Parallel()
	var deep any = "end"
	for range 130 {
		deep = []any{deep}
	}
	_, err := Value(deep)
	require.ErrorIs(t, err, ErrLimit)
	var diamond any = "end"
	for range 18 {
		diamond = []any{diamond, diamond}
	}
	_, err = Value(diamond)
	require.ErrorIs(t, err, ErrLimit)
}
