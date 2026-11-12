package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	plan "github.com/docker/docker-agent/pkg/tools/builtin/plan/types"
	shelltool "github.com/docker/docker-agent/pkg/tools/builtin/shell/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaulttool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/plantool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestBuiltinRouting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		args    string
		result  string
		meta    any
		builder tool.Builder
	}{
		{name: shelltool.ToolNameShell, args: `{"cmd":"echo hello","timeout":10}`, result: "hello", builder: shell.New},
		{name: filesystem.ToolNameReadFile, args: `{"path":"test.txt"}`, result: "body", meta: filesystem.ReadFileMeta{LineCount: 42}, builder: readfile.New},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ar := animation.NewRuntime()
			state := service.StaticSessionState{}
			msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{
				Name: tc.name, Arguments: tc.args,
			}}, tools.Tool{Name: tc.name}, types.ToolStatusCompleted)
			msg.Content = tc.result
			msg.ToolResult = &tools.ToolCallResult{Output: tc.result, Meta: tc.meta}
			view := NewRegistry().New(ar, msg, state)
			expected := tc.builder(ar, msg, state)
			generic := defaulttool.New(ar, msg, state)
			for _, v := range []layout.Model{view, expected, generic} {
				v.SetSize(120, 0)
			}
			assert.Equal(t, expected.View(), view.View())
			assert.NotEqual(t, generic.View(), view.View(), "the full bundle must not silently fall back to generic rendering")
		})
	}
}

func TestPlanToolsRouting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		dedicated bool
	}{
		{plan.ToolNameWritePlan, true},
		{plan.ToolNameSetPlanStatus, true},
		{plan.ToolNameGetPlanStatus, true},
		{plan.ToolNameUpdatePlanFromFile, true},
		{plan.ToolNameExportPlanToFile, true},
		{plan.ToolNameReadPlan, false},
		{plan.ToolNameListPlans, false},
		{plan.ToolNameDeletePlan, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ar := animation.NewRuntime()
			state := service.StaticSessionState{}
			msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{
				Name: tc.name, Arguments: `{"name":"release"}`,
			}}, tools.Tool{Name: tc.name}, types.ToolStatusCompleted)
			msg.Content = `{"title":"Release plan","status":"in-progress","revision":3,"content":"Full plan body"}`
			view := NewRegistry().New(ar, msg, state)
			view.SetSize(140, 0)
			builder := defaulttool.New
			if tc.dedicated {
				builder = plantool.New
			}
			expected := builder(ar, msg, state)
			expected.SetSize(140, 0)
			assert.Equal(t, expected.View(), view.View())
			if tc.dedicated {
				assert.Contains(t, view.View(), "rev 3")
				assert.NotContains(t, view.View(), "Full plan body")
			} else {
				assert.Contains(t, view.View(), "Full plan body")
			}
		})
	}
}

func TestRegistriesHaveIndependentBuiltinMaps(t *testing.T) {
	t.Parallel()

	first, second := NewRegistry(), NewRegistry()
	ar := animation.NewRuntime()
	state := service.StaticSessionState{}
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{
		Name: shelltool.ToolNameShell, Arguments: `{"cmd":"pwd","timeout":10}`,
	}}, tools.Tool{Name: shelltool.ToolNameShell}, types.ToolStatusCompleted)
	first.RegisterBuiltin(shelltool.ToolNameShell, defaulttool.New)
	generic := defaulttool.New(ar, msg, state)
	builtin := shell.New(ar, msg, state)
	firstView, secondView := first.New(ar, msg, state), second.New(ar, msg, state)
	for _, v := range []layout.Model{firstView, secondView, generic, builtin} {
		v.SetSize(100, 0)
	}
	assert.Equal(t, generic.View(), firstView.View())
	assert.Equal(t, builtin.View(), secondView.View())
	assert.NotEqual(t, firstView.View(), secondView.View())
}
