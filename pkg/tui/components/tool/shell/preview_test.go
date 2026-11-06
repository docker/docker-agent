package shell

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestScriptRange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, source, language string
	}{
		{"python heredoc", "python3 - <<'PY'\nprint('hello')\nPY\necho done", "print('hello')\n", "python"},
		{"node heredoc", "node <<\"JS\"\nconsole.log('hello');\nJS", "console.log('hello');\n", "javascript"},
		{"ruby heredoc", "ruby <<RUBY\nputs 'hello'\nRUBY", "puts 'hello'\n", "ruby"},
		{"perl heredoc", "perl <<'PL'\nprint 1;\nPL", "print 1;\n", "perl"},
		{"shell heredoc", "bash <<'SH'\necho hello\nSH", "echo hello\n", "bash"},
		{"tab stripped heredoc", "python3 <<-'PY'\n\tprint(1)\n\tPY", "\tprint(1)\n", "python"},
		{"streaming heredoc", "python3 <<'PY'\nprint(1)", "print(1)", "python"},
		{"delimiter in source", "python3 <<'PY'\nprint('PY')\nPY", "print('PY')\n", "python"},
		{"python inline", "python3 -c 'print(1)'", "print(1)", "python"},
		{"absolute executable", "/usr/bin/python3 -c 'print(1)'", "print(1)", "python"},
		{"node inline", "node -e 'console.log(1)' && echo done", "console.log(1)", "javascript"},
		{"node eval", "node --eval \"console.log(1)\"", "console.log(1)", "javascript"},
		{"ruby inline", "ruby -e 'puts 1'", "puts 1", "ruby"},
		{"perl inline", "perl -e 'print 1'", "print 1", "perl"},
		{"shell inline", "sh -c 'echo hello'", "echo hello", "bash"},
		{"escaped quote", `python3 -c "print(\"hello\")"`, `print(\"hello\")`, "python"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start, end, language, ok := scriptRange(tc.command)
			require.True(t, ok)
			assert.Equal(t, tc.source, tc.command[start:end])
			assert.Equal(t, tc.language, language)
			preview, ok := commandPreview(tc.command, 200)
			require.True(t, ok)
			assert.Contains(t, preview, "\x1b[")
			plain := strings.TrimPrefix(ansi.Strip(preview), "  ")
			lines := strings.Split(strings.ReplaceAll(plain, "\n  ", "\n"), "\n")
			for i := range lines {
				lines[i] = strings.TrimRight(lines[i], " ")
			}
			assert.Equal(t, strings.ReplaceAll(tc.command, "\t", "    "), strings.Join(lines, "\n"))
			assert.Equal(t, tc.source, ansi.Strip(highlightSource(tc.source, language)))
		})
	}
}

func TestScriptRangeFallback(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"ls -la", "cat <<'EOF'\nhello\nEOF", "python3 script.py",
		"python3 -c 'unfinished", `python3 -c 'print('"'"'hi'"'"')'`,
		"node -c 'console.log(1)'", "python3 -e 'print(1)'",
		"echo python3 <<'PY'\nprint(1)\nPY",
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			_, _, _, ok := scriptRange(command)
			assert.False(t, ok)
		})
	}
}

func TestCommandPreviewFallback(t *testing.T) {
	t.Parallel()
	preview, ok := commandPreview("ls -la", 80)
	assert.False(t, ok)
	assert.Empty(t, preview)
	for _, command := range []string{"echo hello\necho world", "python3 -c 'unfinished\nscript"} {
		preview, ok := commandPreview(command, 80)
		require.True(t, ok)
		for line := range strings.SplitSeq(command, "\n") {
			assert.Contains(t, ansi.Strip(preview), line)
		}
	}
}

func TestCommandPreviewWrapsANSI(t *testing.T) {
	t.Parallel()
	command := "python3 <<'PY'\nprint('a long string with unicode: 界界界')\nPY"
	preview, ok := commandPreview(command, 22)
	require.True(t, ok)
	for line := range strings.SplitSeq(preview, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 22)
		assert.NotContains(t, ansi.Strip(line), "\x1b")
	}
}

func TestRenderShellScript(t *testing.T) {
	t.Parallel()
	command := "python3 <<'PY'\nprint(123)\nPY"
	raw, err := json.Marshal(map[string]string{"cmd": command})
	require.NoError(t, err)
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: string(raw)}}, tools.Tool{Name: "shell"}, types.ToolStatusCompleted)
	msg.Content = "123"
	s := spinner.New(animation.NewSnapshotRuntime(0), spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle)
	extract := toolcommon.ExtractField(func(a builtinshell.RunShellArgs) string { return a.Cmd })
	view := renderShell(msg, s, service.StaticSessionState{}, 80, 0, extract)
	plain := ansi.Strip(view)
	assert.Contains(t, plain, "print(123)")
	assert.Contains(t, plain, "123")
	assert.Contains(t, plain, "shell")
	assert.Less(t, strings.Index(plain, "shell"), strings.Index(plain, "python3"))
	lines := strings.Split(plain, "\n")
	assert.Contains(t, lines[len(lines)-1], "123")

	state := &service.EmbeddedSessionState{}
	state.ToggleHideToolResults()
	hidden := ansi.Strip(renderShell(msg, s, state, 80, 0, extract))
	assert.Contains(t, hidden, "shell")
	assert.NotContains(t, hidden, "print(123)")
}
