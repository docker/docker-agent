package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// argsWithStrings exercises the slice-of-string repair path that's by far
// the most commonly broken in real LLM tool calls (paths, urls, patterns).
type argsWithStrings struct {
	Paths []string `json:"paths"`
	JSON  bool     `json:"json,omitempty"`
}

type argsWithInt struct {
	N    int      `json:"n"`
	Tags []string `json:"tags,omitempty"`
}

func TestRepair_UnwrapsStringifiedArray(t *testing.T) {
	// Common DeepSeek/Qwen mistake: send an array as a JSON string.
	in := []byte(`{"paths": "[\"a.txt\",\"b.txt\"]"}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	require.True(t, ok)
	assert.Equal(t, []repairKind{repairUnwrapStringArray}, kinds)
	assert.JSONEq(t, `{"paths":["a.txt","b.txt"]}`, string(out))
}

func TestRepair_WrapsBareString(t *testing.T) {
	// Single-string-instead-of-array, the most common shape mistake.
	in := []byte(`{"paths": "only.txt"}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	require.True(t, ok)
	assert.Equal(t, []repairKind{repairWrapInArray}, kinds)
	assert.JSONEq(t, `{"paths":["only.txt"]}`, string(out))
}

func TestRepair_WrapsSingleObjectPlaceholder(t *testing.T) {
	// Some models wrap a single argument in an object.
	in := []byte(`{"paths": {"path": "only.txt"}}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	require.True(t, ok)
	assert.Equal(t, []repairKind{repairWrapObjectInArray}, kinds)
	assert.JSONEq(t, `{"paths":["only.txt"]}`, string(out))
}

func TestRepair_OrderingPreventsDoubleWrap(t *testing.T) {
	// If the bare-string-wrap fired before the unwrap-stringified-array
	// repair, this input would become [`["a","b"]`] instead of ["a","b"].
	// The fact that we get a clean array out is the load-bearing assertion
	// of this test.
	in := []byte(`{"paths": "[\"a\",\"b\"]"}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	require.True(t, ok)
	assert.Equal(t, []repairKind{repairUnwrapStringArray}, kinds)
	assert.JSONEq(t, `{"paths":["a","b"]}`, string(out))
}

func TestRepair_DropsNullForPrimitive(t *testing.T) {
	// Some custom UnmarshalJSON impls trip on null where a primitive is
	// expected. Dropping the field lets the type's zero value win.
	in := []byte(`{"n": null}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithInt]())
	require.True(t, ok)
	assert.Equal(t, []repairKind{repairDropNull}, kinds)
	assert.JSONEq(t, `{}`, string(out))
}

func TestRepair_LeavesValidArrayUntouched(t *testing.T) {
	// The repair entry point should only be reached after a strict parse
	// already failed, but defensively ensure that a well-formed array is
	// not "repaired" if we ever do get called with one.
	in := []byte(`{"paths": ["a","b"]}`)
	_, _, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	assert.False(t, ok)
}

func TestRepair_LeavesUnknownFieldsAlone(t *testing.T) {
	// Field not declared on the struct: out of repair scope.
	in := []byte(`{"unknown": "foo"}`)
	_, _, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	assert.False(t, ok)
}

func TestRepair_ReturnsFalseOnNonObjectInput(t *testing.T) {
	// Top-level non-object payloads are unparseable as field-shape errors.
	in := []byte(`"just a string"`)
	_, _, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	assert.False(t, ok)
}

func TestRepair_RefusesMultiKeyObjectAsArray(t *testing.T) {
	// Two keys in the placeholder object — too ambiguous to safely wrap.
	in := []byte(`{"paths": {"path": "a.txt", "extra": "ignore"}}`)
	_, _, ok := tryRepairToolArgs(in, reflect.TypeFor[argsWithStrings]())
	assert.False(t, ok)
}

// TestRepair_VisitsPromotedFieldsFromEmbeddedStruct mirrors the shape of
// LSP arg structs in pkg/tools/builtin/lsp.go (ReferencesArgs and friends
// embed PositionArgs). reflect.VisibleFields is what makes promoted fields
// visit-able; iterating NumField/Field directly would silently skip them.
func TestRepair_VisitsPromotedFieldsFromEmbeddedStruct(t *testing.T) {
	type Base struct {
		Files []string `json:"files"`
	}
	type WithEmbedding struct {
		Base

		Extra string `json:"extra,omitempty"`
	}
	in := []byte(`{"files":"only.txt"}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[WithEmbedding]())
	require.True(t, ok)
	assert.Equal(t, []repairKind{repairWrapInArray}, kinds)
	assert.JSONEq(t, `{"files":["only.txt"]}`, string(out))
}

func TestRepair_RepairsMultipleFieldsInOneCall(t *testing.T) {
	type combo struct {
		Paths []string `json:"paths"`
		Tags  []string `json:"tags"`
	}
	in := []byte(`{"paths":"only.txt","tags":"[\"go\",\"ai\"]"}`)
	out, kinds, ok := tryRepairToolArgs(in, reflect.TypeFor[combo]())
	require.True(t, ok)
	assert.Len(t, kinds, 2)
	assert.JSONEq(t, `{"paths":["only.txt"],"tags":["go","ai"]}`, string(out))
}

// End-to-end test that exercises the NewHandler integration: invalid input
// reshaped by repair, handler called with the typed value.
func TestNewHandler_RepairsBareStringArray(t *testing.T) {
	type fileArgs struct {
		Paths []string `json:"paths"`
	}
	var got fileArgs
	handler := NewHandler(func(_ context.Context, args fileArgs) (*ToolCallResult, error) {
		got = args
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"only.txt"}`,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
	assert.Equal(t, []string{"only.txt"}, got.Paths)
}

func TestNewHandler_RepairsStringifiedArray(t *testing.T) {
	type fileArgs struct {
		Paths []string `json:"paths"`
	}
	var got fileArgs
	handler := NewHandler(func(_ context.Context, args fileArgs) (*ToolCallResult, error) {
		got = args
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"[\"a.txt\",\"b.txt\"]"}`,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
	assert.Equal(t, []string{"a.txt", "b.txt"}, got.Paths)
}

func TestNewHandler_UnrepairableInputReturnsOriginalError(t *testing.T) {
	type fileArgs struct {
		Paths []string `json:"paths"`
	}
	handler := NewHandler(func(_ context.Context, _ fileArgs) (*ToolCallResult, error) {
		t.Fatal("handler should not be called for unrepairable input")
		return nil, nil
	})

	_, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{not even json`,
		},
	})
	require.Error(t, err)
}
