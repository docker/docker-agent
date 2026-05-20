package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowContext_EvalCondition(t *testing.T) {
	t.Parallel()

	wf := &Workflow{name: "test_workflow"}
	ctx, err := NewWorkflowContext(wf, "test_checkpoint")
	require.NoError(t, err)

	ctx.SetAgentOutput("qa", `{"is_approved": true}`, "qa_agent")

	ok, resolved := ctx.EvalCondition("{{ $steps.qa.output.is_approved }}")
	require.True(t, resolved)
	assert.True(t, ok)

	ctx.SetAgentOutput("qa", `{"is_approved": false}`, "qa_agent")
	ok, resolved = ctx.EvalCondition("{{ $steps.qa.output.is_approved }}")
	require.True(t, resolved)
	assert.False(t, ok)
}
