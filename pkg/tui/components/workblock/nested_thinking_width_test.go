package workblock

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	tooldefaults "github.com/docker/docker-agent/pkg/tui/components/tool/defaults"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func expandedShellMessage(id string) *types.Message {
	call := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf output"}`}}
	msg := types.ToolCallMessage("root", call, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = "alpha output\nbeta output"
	return msg
}

// shellHeaderRows returns the expanded shell header rows in visual order.
func shellHeaderRows(view string) []int {
	var rows []int
	for i, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(line, "✓ shell") {
			rows = append(rows, i)
		}
	}
	return rows
}

func TestExpandedThinkingShellMatchesWorkSideMargins(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	direct := shell.New(ar, expandedShellMessage("direct"), service.StaticSessionState{})

	// Production transcripts build reasoning blocks with the default tool registry;
	// without it a nested shell falls back to the generic tool view.
	thinking := reasoningblock.New(ar, "thinking", "root", service.StaticSessionState{}, reasoningblock.WithToolRenderers(tooldefaults.NewRegistry()))
	thinking.AddToolCall(expandedShellMessage("nested"))

	model := New(ar)
	model.SetSize(60, 20)
	model.SetItems([]Item{{SourceIndex: 1, View: direct}, {SourceIndex: 2, View: thinking}})
	model.SetExpandedImmediately(true)
	t.Cleanup(model.StopAnimation)

	view := model.View()
	headers := shellHeaderRows(view)
	require.Len(t, headers, 2, "direct and nested shell headers are both visible: %q", ansi.Strip(view))
	plain := strings.Split(ansi.Strip(view), "\n")
	left := func(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

	// Painted bounds: every Work row spans the full composite width, and the
	// two margin cells on each side stay blank so no card paints outside the body.
	for line := headers[0]; line < len(plain); line++ {
		row := plain[line]
		if row == "" {
			continue // blank spacer between children paints nothing
		}
		require.Equal(t, 60, ansi.StringWidth(row), "Work row keeps its full width: %q", row)
		require.Equal(t, "  ", ansi.Cut(row, 0, 2), "left Work margin stays blank: %q", row)
		require.Equal(t, "  ", ansi.Cut(row, 58, 60), "right Work margin stays blank: %q", row)
	}

	direct0, nested := plain[headers[0]], plain[headers[1]]
	require.Equal(t, left(direct0), left(nested), "nested shell starts at the same Work inset as a direct card")
	require.GreaterOrEqual(t, left(nested), 2, "the nested shell header starts inside the two-column Work margin")
}
