package editor

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor/completions"
)

// mockCompletion implements completions.Completion for testing
type mockCompletion struct {
	trigger string
	items   []completion.Item
}

func (m *mockCompletion) Trigger() string                 { return m.trigger }
func (m *mockCompletion) Items() []completion.Item        { return m.items }
func (m *mockCompletion) AutoSubmit() bool                { return false }
func (m *mockCompletion) RequiresEmptyEditor() bool       { return true }
func (m *mockCompletion) MatchMode() completion.MatchMode { return completion.MatchFuzzy }

var _ completions.Completion = (*mockCompletion)(nil)

// newTestEditor creates an editor with slash completion active for testing
func newTestEditor(value, completionWord string) *editor {
	ta := textarea.New()
	ta.SetWidth(80)
	ta.SetHeight(10)
	ta.Focus()
	ta.SetValue(value)
	ta.MoveToEnd()

	slashCompletion := &mockCompletion{
		trigger: "/",
		items: []completion.Item{
			{Label: "new", Value: "/new"},
			{Label: "exit", Value: "/exit"},
			{Label: "compact", Value: "/compact"},
		},
	}

	return &editor{
		textarea:          ta,
		completions:       []completions.Completion{slashCompletion},
		currentCompletion: slashCompletion,
		completionWord:    completionWord,
		userTyped:         true,
	}
}

// findQueryMsg looks for a QueryMsg in the command's results
func findQueryMsg(cmd tea.Cmd) (completion.QueryMsg, bool) {
	for _, msg := range collectMsgs(cmd) {
		if qm, ok := msg.(completion.QueryMsg); ok {
			return qm, true
		}
	}
	return completion.QueryMsg{}, false
}

// hasCloseMsg checks if a CloseMsg is in the command's results
func hasCloseMsg(cmd tea.Cmd) bool {
	for _, msg := range collectMsgs(cmd) {
		if _, ok := msg.(completion.CloseMsg); ok {
			return true
		}
	}
	return false
}

func TestBackspaceUpdatesCompletionQuery(t *testing.T) {
	t.Parallel()

	t.Run("backspace in completion word sends updated query", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/co", "co")
		_, cmd := e.handleGraphemeBackspace()

		assert.Equal(t, "/c", e.textarea.Value())
		require.NotNil(t, cmd)

		queryMsg, found := findQueryMsg(cmd)
		assert.True(t, found, "should have QueryMsg")
		assert.Equal(t, "c", queryMsg.Query)
	})

	t.Run("backspace to just trigger sends empty query", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/c", "c")
		_, cmd := e.handleGraphemeBackspace()

		assert.Equal(t, "/", e.textarea.Value())
		require.NotNil(t, cmd)

		queryMsg, found := findQueryMsg(cmd)
		assert.True(t, found, "should have QueryMsg")
		assert.Empty(t, queryMsg.Query)
	})

	t.Run("backspace removing trigger closes completion", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/", "")
		_, cmd := e.handleGraphemeBackspace()

		assert.Empty(t, e.textarea.Value())
		require.NotNil(t, cmd)
		assert.True(t, hasCloseMsg(cmd), "should have CloseMsg")
	})

	t.Run("backspace from no-match query sends QueryMsg", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/xyz", "xyz")
		_, cmd := e.handleGraphemeBackspace()

		assert.Equal(t, "/xy", e.textarea.Value())

		_, found := findQueryMsg(cmd)
		assert.True(t, found, "should send QueryMsg even when previous query had no matches")
	})
}

// collectMsgs executes a command (or batch of commands) and collects all returned messages
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()
	if batchMsg, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, innerCmd := range batchMsg {
			if innerCmd != nil {
				msgs = append(msgs, collectMsgs(innerCmd)...)
			}
		}
		return msgs
	}
	if msg != nil {
		return []tea.Msg{msg}
	}
	return nil
}
