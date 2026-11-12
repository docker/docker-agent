package think

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type scratchpadRuntime struct {
	tools.NopRuntime

	mu       sync.Mutex
	thoughts []string
}

func (r *scratchpadRuntime) AppendThought(ctx context.Context, _ any, thought string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.thoughts = append(r.thoughts, thought)
	return slices.Clone(r.thoughts), nil
}

func TestThinkTool_Handler(t *testing.T) {
	t.Parallel()
	handler := thinkHandler(t)
	rt := &scratchpadRuntime{}
	output, err := callThink(t.Context(), handler, "This is a test thought", rt)
	require.NoError(t, err)
	assert.Equal(t, "Thoughts:\nThis is a test thought", output)
	output, err = callThink(t.Context(), handler, "Another thought", rt)
	require.NoError(t, err)
	assert.Equal(t, "Thoughts:\nThis is a test thought\nAnother thought", output)
}

func thinkHandler(t *testing.T) tools.ToolHandler {
	t.Helper()
	allTools, err := New().Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	return allTools[0].Handler
}

func callThink(ctx context.Context, handler tools.ToolHandler, thought string, rt tools.Runtime) (string, error) {
	result, err := handler(ctx, tools.ToolCall{
		Function: tools.FunctionCall{Name: ToolNameThink, Arguments: fmt.Sprintf(`{"thought":%q}`, thought)},
	}, rt)
	if err != nil {
		return "", err
	}
	if result.IsError {
		return "", fmt.Errorf("%s", result.Output)
	}
	return result.Output, nil
}

func TestThinkTool_SequentialOutput(t *testing.T) {
	t.Parallel()
	handler := thinkHandler(t)
	rt := &scratchpadRuntime{}

	output, err := callThink(t.Context(), handler, "first", rt)
	require.NoError(t, err)
	assert.Equal(t, "Thoughts:\nfirst", output)

	output, err = callThink(t.Context(), handler, "second", rt)
	require.NoError(t, err)
	assert.Equal(t, "Thoughts:\nfirst\nsecond", output)
}

func TestThinkTool_ConcurrentCalls(t *testing.T) {
	t.Parallel()
	handler := thinkHandler(t)
	rt := &scratchpadRuntime{}

	const n = 64
	thoughts := make([]string, n)
	outputs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		thoughts[i] = fmt.Sprintf("thought-%02d", i)
		wg.Go(func() {
			outputs[i], errs[i] = callThink(t.Context(), handler, thoughts[i], rt)
		})
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		lines := strings.Split(outputs[i], "\n")
		assert.Equal(t, "Thoughts:", lines[0])
		assert.Contains(t, lines[1:], thoughts[i])
	}

	output, err := callThink(t.Context(), handler, "final", rt)
	require.NoError(t, err)
	lines := strings.Split(output, "\n")
	require.Len(t, lines, n+2)
	assert.Equal(t, "Thoughts:", lines[0])
	assert.ElementsMatch(t, thoughts, lines[1:n+1])
	assert.Equal(t, "final", lines[n+1])
}

func TestThinkTool_OutputSchema(t *testing.T) {
	t.Parallel()
	tool := New()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestThinkTool_ParametersAreObjects(t *testing.T) {
	t.Parallel()
	tool := New()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)

		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

func TestThinkTool_UnscopedIsEphemeral(t *testing.T) {
	t.Parallel()
	handler := thinkHandler(t)
	for _, thought := range []string{"first\nline", "second", ""} {
		output, err := callThink(t.Context(), handler, thought, tools.NopRuntime{})
		require.NoError(t, err)
		assert.Equal(t, "Thoughts:\n"+thought, output)
	}
}

type scopedWithoutScratchpad struct{ tools.NopRuntime }

func TestThinkTool_Failures(t *testing.T) {
	t.Parallel()
	handler := thinkHandler(t)
	_, err := callThink(t.Context(), handler, "hidden", scopedWithoutScratchpad{})
	require.ErrorContains(t, err, "unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rt := &scratchpadRuntime{}
	_, err = callThink(ctx, handler, "cancelled", rt)
	require.ErrorContains(t, err, "canceled")
	assert.Empty(t, rt.thoughts)
}
