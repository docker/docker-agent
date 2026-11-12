package calculator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestCalculate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		expression  string
		result      string
		decimal     string
		approximate bool
	}{
		{expression: "2 + 3 * 4", result: "14"},
		{expression: "(2 + 3) * 4", result: "20"},
		{expression: "  \n -12 + +2 \t", result: "-10"},
		{expression: "0.1 + 0.2", result: "0.3"},
		{expression: "(19.99 * 3) * (1 + 8.25 / 100)", result: "64.917525"},
		{expression: "9007199254740993 + 1", result: "9007199254740994"},
		{expression: "9223372036854775807 * 2", result: "18446744073709551614"},
		{expression: "1 / 8", result: "0.125"},
		{expression: "1 / 3", result: "1/3", decimal: "0.33333333333333333333", approximate: true},
		{expression: "2 / 3", result: "2/3", decimal: "0.66666666666666666667", approximate: true},
		{expression: "-1 / 6", result: "-1/6", decimal: "-0.16666666666666666667", approximate: true},
		{expression: "1 / 3 * 3", result: "1"},
		{expression: "1 / 3 + 1 / 6", result: "0.5"},
		{expression: "1e3 + 2.5E-2", result: "1000.025"},
		{expression: "1e+2", result: "100"},
		{expression: ".5 + 1.", result: "1.5"},
		{expression: "010 + 008", result: "18"},
		{expression: "00009", result: "9"},
		{expression: "0000", result: "0"},
		{expression: "010 / 008", result: "1.25"},
		{expression: "010e0 + 008.0", result: "18"},
		{expression: "-0", result: "0"},
		{expression: "10 / 4 / 2", result: "1.25"},
		{expression: "10 - 4 - 2", result: "4"},
		{expression: "17 % 5", result: "2"},
		{expression: "-17 % 5", result: "-2"},
		{expression: "17 % -5", result: "2"},
		{expression: "2^10", result: "1024"},
		{expression: "2^-3", result: "0.125"},
		{expression: "(1 / 3)^-2", result: "9"},
		{expression: "2^3^2", result: "512"},
		{expression: "-2^2", result: "-4"},
		{expression: "(-2)^2", result: "4"},
		{expression: "2^0", result: "1"},
		{expression: "0^2", result: "0"},
		{expression: "(-2)^3", result: "-8"},
	}

	for _, tt := range tests {
		t.Run(tt.expression, func(t *testing.T) {
			t.Parallel()

			result, err := New().calculate(t.Context(), Args{Expression: tt.expression})
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
			var got Result
			require.NoError(t, json.Unmarshal([]byte(result.Output), &got))
			decimal := tt.decimal
			if decimal == "" {
				decimal = tt.result
			}
			assert.Equal(t, Result{Result: tt.result, Decimal: decimal, Approximate: tt.approximate}, got)
		})
	}
}

func TestCalculateErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		expression string
		want       string
	}{
		{name: "empty", want: "expected a number"},
		{name: "whitespace", expression: " \n\t", want: "expected a number"},
		{name: "missing operand", expression: "1 +", want: "expected a number"},
		{name: "unclosed group", expression: "(1 + 2", want: "expected ')'"},
		{name: "extra group", expression: "1 + 2)", want: "unexpected character"},
		{name: "empty group", expression: "()", want: "expected a number"},
		{name: "dot", expression: ".", want: "expected a number"},
		{name: "two decimals", expression: "1.2.3", want: "unexpected character"},
		{name: "comma", expression: "1,000", want: "unexpected character"},
		{name: "implicit multiplication", expression: "2(3)", want: "unexpected character"},
		{name: "division by zero", expression: "1 / (2 - 2)", want: "division by zero"},
		{name: "remainder by zero", expression: "1 % 0", want: "remainder by zero"},
		{name: "fractional remainder", expression: "1.5 % 1", want: "integer operands"},
		{name: "fractional divisor", expression: "1 % .5", want: "integer operands"},
		{name: "fractional power", expression: "4^0.5", want: "exponent must be an integer"},
		{name: "zero to zero", expression: "0^0", want: "zero or negative power"},
		{name: "zero to negative", expression: "0^-1", want: "zero or negative power"},
		{name: "large exponent", expression: "2^1025", want: "exponent must be an integer"},
		{name: "large negative exponent", expression: "2^-1025", want: "exponent must be an integer"},
		{name: "overflowing exponent", expression: "2^99999999999999999999", want: "exponent must be an integer"},
		{name: "large scientific exponent", expression: "1e1025", want: "scientific exponent"},
		{name: "small scientific exponent", expression: "1e-1025", want: "scientific exponent"},
		{name: "overflowing scientific exponent", expression: "1e99999999999999999999", want: "scientific exponent"},
		{name: "missing scientific exponent", expression: "1e", want: "scientific exponent"},
		{name: "missing signed exponent", expression: "1e-", want: "scientific exponent"},
		{name: "hexadecimal", expression: "0xff", want: "unexpected character"},
		{name: "variable", expression: "x + 2", want: "expected a number"},
		{name: "function", expression: "sqrt(4)", want: "expected a number"},
		{name: "code", expression: "1; alert(1)", want: "unexpected character"},
		{name: "NaN", expression: "NaN", want: "expected a number"},
		{name: "infinity", expression: "Inf", want: "expected a number"},
		{name: "length limit", expression: strings.Repeat("1", maxExpressionBytes+1), want: "exceeds 8192 bytes"},
		{name: "nesting limit", expression: strings.Repeat("(", maxDepth+1) + "1" + strings.Repeat(")", maxDepth+1), want: "nesting exceeds"},
		{name: "unary limit", expression: strings.Repeat("-", maxDepth+1) + "1", want: "nesting exceeds"},
		{name: "power nesting limit", expression: strings.Repeat("1^", maxDepth+1) + "1", want: "nesting exceeds"},
		{name: "power size limit", expression: "(10^1000)^1000", want: "power exceeds"},
		{name: "value size limit", expression: "10^1000 * 10^1000 * 10^1000 * 10^1000 * 10^1000", want: "value exceeds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := New().calculate(t.Context(), Args{Expression: tt.expression})
			require.NoError(t, err)
			require.True(t, result.IsError)
			assert.Contains(t, result.Output, tt.want)
		})
	}
}

func TestCalculateCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := evaluate(ctx, "1 + 2")
	require.ErrorIs(t, err, context.Canceled)
}

func TestCalculatorTool(t *testing.T) {
	t.Parallel()

	ts, err := CreateToolSet()
	require.NoError(t, err)
	list, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, list, 1)
	tool := list[0]
	assert.Equal(t, ToolNameCalculate, tool.Name)
	assert.Equal(t, "calculator", tool.Category)
	assert.True(t, tool.Annotations.ReadOnlyHint)
	assert.True(t, tool.Annotations.IdempotentHint)
	assert.NotEmpty(t, tools.GetInstructions(ts))
	assert.Equal(t, tools.MustSchemaFor[Args](), tool.Parameters)
	assert.Equal(t, tools.MustSchemaFor[Result](), tool.OutputSchema)

	for _, args := range []string{`{"expression":"0.1 + 0.2"}`, `{"expression":"1 / 0"}`, `{}`} {
		result, err := tool.Handler(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{Name: ToolNameCalculate, Arguments: args},
		}, tools.NopRuntime{})
		require.NoError(t, err)
		if args == `{"expression":"0.1 + 0.2"}` {
			assert.False(t, result.IsError)
			assert.JSONEq(t, `{"result":"0.3","decimal":"0.3","approximate":false}`, result.Output)
		} else {
			assert.True(t, result.IsError)
		}
	}
}

func FuzzEvaluate(f *testing.F) {
	for _, expression := range []string{"0.1 + 0.2", "1/3", "2^-3^2", "0^0", "1e99999", "()", "1; alert(1)"} {
		f.Add(expression)
	}
	f.Fuzz(func(t *testing.T, expression string) {
		value, err := evaluate(t.Context(), expression)
		if err == nil {
			require.NotNil(t, value)
			require.NoError(t, checkSize(value))
		}
	})
}

func TestExactResultsCanBeReused(t *testing.T) {
	t.Parallel()

	for _, expression := range []string{"(2^-1023)^4", "1 / (2^1000 - 1)", "10^1000"} {
		t.Run(expression, func(t *testing.T) {
			t.Parallel()

			result, err := New().calculate(t.Context(), Args{Expression: expression})
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
			var got Result
			require.NoError(t, json.Unmarshal([]byte(result.Output), &got))
			value, err := evaluate(t.Context(), "("+got.Result+") * 1")
			require.NoError(t, err)
			original, err := evaluate(t.Context(), expression)
			require.NoError(t, err)
			assert.Zero(t, original.Cmp(value))
		})
	}
}
