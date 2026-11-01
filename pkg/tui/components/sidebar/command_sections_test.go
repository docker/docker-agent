package sidebar

import (
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newCommandSectionModel(t *testing.T, sections ...CommandSection) *model {
	t.Helper()
	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)
	m.SetCommandSections(sections)
	return m
}

func sectionsText(m *model) string {
	return ansi.Strip(strings.Join(m.renderSections(40), "\n"))
}

func TestParseCommandSection(t *testing.T) {
	t.Parallel()

	section, err := ParseCommandSection(" Apps = status --short | grep a=b ")
	require.NoError(t, err)
	assert.Equal(t, CommandSection{Title: "Apps", Command: "status --short | grep a=b"}, section)

	for _, spec := range []string{"", "Apps", "=status", "Apps= "} {
		_, err := ParseCommandSection(spec)
		require.Error(t, err, spec)
	}
}

func TestCommandSectionShowsItsOutputAndHidesWhileEmpty(t *testing.T) {
	t.Parallel()

	m := newCommandSectionModel(t, CommandSection{Title: "Apps", Command: "unused"})
	assert.NotContains(t, sectionsText(m), "Apps")

	next := m.handleCommandSection(commandSectionResultMsg{lines: []string{"relay running", strings.Repeat("x", 80)}})
	require.NotNil(t, next, "a result schedules the next run")
	text := sectionsText(m)
	assert.Contains(t, text, "Apps")
	assert.Contains(t, text, "relay running")
	assert.NotContains(t, text, strings.Repeat("x", 41), "a long line is cut to the sidebar")

	m.handleCommandSection(commandSectionResultMsg{})
	assert.NotContains(t, sectionsText(m), "relay running")
}

func TestCommandSectionLinesKeepsPrintableTextAndBoundsIt(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"a b", "c"}, commandSectionLines("\x1b[31ma\tb\x1b[0m  \r\n\n\nc\n"))

	lines := commandSectionLines(strings.Repeat("line\n", commandSectionMaxLines+5))
	require.Len(t, lines, commandSectionMaxLines)
	assert.Equal(t, "…", lines[len(lines)-1])
}

func TestCommandSectionRunsItsCommandOnce(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the command is written for a POSIX shell")
	}

	m := newCommandSectionModel(t, CommandSection{Title: "Apps", Command: "echo relay; echo whoami; exit 3"})
	cmd := m.pollCommandSections()
	require.NotNil(t, cmd)
	assert.Nil(t, m.pollCommandSections(), "Init runs again on a tab switch and must not start a second chain")

	msg := m.runCommandSection(0)().(commandSectionResultMsg)
	assert.Equal(t, []string{"relay", "whoami"}, msg.lines, "a failing command still shows what it printed")
	assert.Nil(t, m.runCommandSection(7))
}
