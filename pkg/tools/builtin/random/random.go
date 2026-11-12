// Package random provides random integer generation with agent-chosen bounds.
package random

import (
	"context"
	"crypto/rand"
	"math/big"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameRandomInt = "random_int"

// CreateToolSet is used by the tools registry.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

type ToolSet struct{}

type Args struct {
	Min *int64 `json:"min" jsonschema:"Minimum integer value (inclusive)"`
	Max *int64 `json:"max" jsonschema:"Maximum integer value (inclusive); must be greater than or equal to min"`
}

func New() *ToolSet {
	return &ToolSet{}
}

func (t *ToolSet) callTool(_ context.Context, params Args) (*tools.ToolCallResult, error) {
	if params.Min == nil || params.Max == nil {
		return tools.ResultError("min and max are required"), nil
	}
	lower, upper := *params.Min, *params.Max
	if lower > upper {
		return tools.ResultError("min must be less than or equal to max"), nil
	}
	if lower == upper {
		return tools.ResultJSON(lower), nil
	}

	// Big integers keep the inclusive range width from overflowing int64.
	width := new(big.Int).Sub(big.NewInt(upper), big.NewInt(lower))
	width.Add(width, big.NewInt(1))
	value, err := rand.Int(rand.Reader, width)
	if err != nil {
		return tools.ResultError("failed to generate random integer: " + err.Error()), nil
	}
	value.Add(value, big.NewInt(lower))
	return tools.ResultJSON(value.Int64()), nil
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:               ToolNameRandomInt,
			Category:           "random",
			Description:        "Generate a uniformly random integer between min and max (both inclusive). Supply both bounds on every call. Negative values are allowed; min must be less than or equal to max.",
			Parameters:         tools.MustSchemaFor[Args](),
			OutputSchema:       tools.MustSchemaFor[int64](),
			Handler:            tools.NewHandler(t.callTool),
			AllowRepeatedCalls: true,
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Random Integer",
			},
		},
	}, nil
}
