package userprompt

import (
	"context"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestHandlerReplacementWhilePrompting(t *testing.T) {
	t.Parallel()
	ts := New()
	handler := func(context.Context, *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	}
	ts.SetElicitationHandler(handler)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			ts.SetElicitationHandler(handler)
		}
	})
	errs := make(chan error, 1)
	wg.Go(func() {
		for range 1000 {
			result, err := ts.userPrompt(t.Context(), Args{Message: "question"})
			if err != nil {
				errs <- err
				return
			}
			assert.True(t, result.IsError)
			assert.Contains(t, result.Output, `"action":"decline"`)
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestHandlerIsCalledOutsideLock(t *testing.T) {
	t.Parallel()
	ts := New()
	ts.SetElicitationHandler(func(context.Context, *mcp.ElicitParams) (tools.ElicitationResult, error) {
		ts.SetElicitationHandler(nil)
		return tools.ElicitationResult{Action: tools.ElicitationActionAccept}, nil
	})
	result, err := ts.userPrompt(t.Context(), Args{Message: "question"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	result, err = ts.userPrompt(t.Context(), Args{Message: "next question"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "no elicitation handler")
}
