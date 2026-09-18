package tools_test

import (
	"context"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestHandlerScopeRoutesCallbacksByContext(t *testing.T) {
	t.Parallel()

	ctxA := tools.WithHandlerScope(t.Context(), tools.HandlerScope{
		Elicitation: func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
			return tools.ElicitationResult{Content: map[string]any{"owner": "a"}}, nil
		},
	})
	ctxB := tools.WithHandlerScope(t.Context(), tools.HandlerScope{
		Elicitation: func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
			return tools.ElicitationResult{Content: map[string]any{"owner": "b"}}, nil
		},
	})

	resultA, err := tools.ScopedElicitationHandler(ctxA, &gomcp.ElicitParams{})
	assert.NilError(t, err)
	resultB, err := tools.ScopedElicitationHandler(ctxB, &gomcp.ElicitParams{})
	assert.NilError(t, err)
	assert.Check(t, cmp.Equal(resultA.Content["owner"], "a"))
	assert.Check(t, cmp.Equal(resultB.Content["owner"], "b"))
}

func TestWithoutHandlerScopePreventsMisrouting(t *testing.T) {
	t.Parallel()

	ctx := tools.WithHandlerScope(t.Context(), tools.HandlerScope{
		Elicitation: func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
			return tools.ElicitationResult{}, nil
		},
	})
	_, err := tools.ScopedElicitationHandler(tools.WithoutHandlerScope(ctx), &gomcp.ElicitParams{})
	assert.ErrorContains(t, err, "no request-scoped")
}
