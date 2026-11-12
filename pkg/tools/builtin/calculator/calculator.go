// Package calculator provides exact arithmetic without executing code.
package calculator

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameCalculate = "calculate"

type ToolSet struct{}

type Args struct {
	Expression string `json:"expression" jsonschema:"Arithmetic expression using decimal numbers (including scientific notation), parentheses, +, -, *, /, %, and ^ for integer powers. Example: (19.99 * 3) * (1 + 8.25 / 100). No variables or function calls. Maximum 8192 bytes."`
}

type Result struct {
	Result      string `json:"result" jsonschema:"Exact answer as a decimal or, for non-terminating decimals, a reduced fraction"`
	Decimal     string `json:"decimal" jsonschema:"Decimal answer; non-terminating decimals are rounded to 20 decimal places"`
	Approximate bool   `json:"approximate" jsonschema:"True only when the decimal field is rounded; result is always exact"`
}

// CreateToolSet is used by the tools registry.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

func New() *ToolSet {
	return &ToolSet{}
}

func (t *ToolSet) Instructions() string {
	return `## Calculator

Use calculate for numerical calculations instead of doing arithmetic yourself.
Pass a single expression with decimal numbers, parentheses, +, -, *, /, %, or ^.
Powers use integer exponents and associate right-to-left; -2^2 means -(2^2).
% is integer remainder, not a percentage: write 15 / 100 for 15%.
Scientific notation such as 1.5e3 is supported. Variables and functions are not.
The result field is always exact. When approximate is true, decimal is rounded
(to 20 decimal places); use the exact fraction in result for further calculations.`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{
		Name:         ToolNameCalculate,
		Category:     "calculator",
		Description:  "Calculate an arithmetic expression using exact rational arithmetic, avoiding floating-point rounding and integer overflow. Supports +, -, *, /, integer remainder %, integer powers ^, parentheses, and scientific notation. No code execution or side effects. Returns an exact result plus a decimal representation, explicitly marked when rounded.",
		Parameters:   tools.MustSchemaFor[Args](),
		OutputSchema: tools.MustSchemaFor[Result](),
		Handler:      tools.NewHandler(t.calculate),
		Annotations: tools.ToolAnnotations{
			Title:          "Calculate",
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}}, nil
}

func (t *ToolSet) calculate(ctx context.Context, args Args) (*tools.ToolCallResult, error) {
	value, err := evaluate(ctx, args.Expression)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	places, exact := value.FloatPrec()
	if !exact {
		return tools.ResultJSON(Result{
			Result:      value.RatString(),
			Decimal:     value.FloatString(20),
			Approximate: true,
		}), nil
	}

	decimal := value.FloatString(places)
	return tools.ResultJSON(Result{Result: decimal, Decimal: decimal}), nil
}
