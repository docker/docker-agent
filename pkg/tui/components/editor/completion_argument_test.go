package editor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor/completions"
	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// mockArgumentCompletion implements both completions.Completion and
// completions.ArgumentCompleter, mirroring commandCompletion for
// /toolset-restart-style argument completion without depending on the real
// commands package.
type mockArgumentCompletion struct {
	mockCompletion

	argItems   []completion.Item
	argMatched bool
}

func (m *mockArgumentCompletion) ArgumentItems(string) ([]completion.Item, bool) {
	return m.argItems, m.argMatched
}

// MatchMode overrides the embedded mockCompletion's fuzzy default: the real
// commandCompletion source uses prefix matching, and tests assert on it.
func (m *mockArgumentCompletion) MatchMode() completion.MatchMode {
	return completion.MatchPrefix
}

var _ completions.ArgumentCompleter = (*mockArgumentCompletion)(nil)

func newArgumentCompletion(matched bool, items ...completion.Item) *mockArgumentCompletion {
	return &mockArgumentCompletion{
		mockCompletion: mockCompletion{trigger: "/"},
		argItems:       items,
		argMatched:     matched,
	}
}

// dynamicArgumentCompletion is an ArgumentCompleter whose candidates are
// computed live by a callback on every call, mirroring how the real
// commandCompletion source calls CompleteArgument() fresh each time instead
// of returning a cached snapshot.
type dynamicArgumentCompletion struct {
	mockCompletion

	itemsFunc func() []completion.Item
}

func (d *dynamicArgumentCompletion) ArgumentItems(string) ([]completion.Item, bool) {
	return d.itemsFunc(), true
}

var _ completions.ArgumentCompleter = (*dynamicArgumentCompletion)(nil)

// newArgumentTestEditor builds a bare editor with the given value and
// completion sources, mirroring newTestEditor but without pre-seeding any
// active completion session.
func newArgumentTestEditor(value string, comps ...completions.Completion) *editor {
	ta := input.NewArea()
	ta.SetWidth(80)
	ta.SetHeight(10)
	ta.Focus()
	ta.SetValue(value)
	ta.MoveToEnd()

	return &editor{
		textarea:    ta,
		completions: comps,
		userTyped:   true,
	}
}

func TestTryStartArgumentCompletion(t *testing.T) {
	t.Parallel()

	t.Run("no space in value returns nil without mutating state", func(t *testing.T) {
		t.Parallel()

		argComp := newArgumentCompletion(true, completion.Item{Label: "github", Value: "/toolset-restart github"})
		e := newArgumentTestEditor("/toolset-restart", argComp)

		cmd := e.TryStartArgumentCompletion()

		assert.Nil(t, cmd)
		assert.Empty(t, e.argumentPrefix)
		assert.Nil(t, e.currentCompletion)
	})

	t.Run("unknown command returns nil", func(t *testing.T) {
		t.Parallel()

		argComp := newArgumentCompletion(false)
		e := newArgumentTestEditor("/nope something", argComp)

		cmd := e.TryStartArgumentCompletion()

		assert.Nil(t, cmd)
		assert.Empty(t, e.argumentPrefix)
	})

	t.Run("command without candidates returns nil", func(t *testing.T) {
		t.Parallel()

		argComp := newArgumentCompletion(true) // matched, but zero items
		e := newArgumentTestEditor("/toolset-restart ", argComp)

		cmd := e.TryStartArgumentCompletion()

		assert.Nil(t, cmd)
		assert.Empty(t, e.argumentPrefix)
	})

	t.Run("plain text (no leading command) returns nil", func(t *testing.T) {
		t.Parallel()

		argComp := newArgumentCompletion(false)
		e := newArgumentTestEditor("hello world", argComp)

		cmd := e.TryStartArgumentCompletion()

		assert.Nil(t, cmd)
		assert.Empty(t, e.argumentPrefix)
	})

	t.Run("matching command opens popup, sets prefix and seeds query", func(t *testing.T) {
		t.Parallel()

		argComp := newArgumentCompletion(true,
			completion.Item{Label: "github", Value: "/toolset-restart github"},
			completion.Item{Label: "filesystem", Value: "/toolset-restart filesystem", Disabled: true},
		)
		e := newArgumentTestEditor("/toolset-restart gi", argComp)

		cmd := e.TryStartArgumentCompletion()
		require.NotNil(t, cmd)

		assert.Equal(t, "/toolset-restart ", e.argumentPrefix)
		assert.Same(t, argComp, e.currentCompletion)

		msgs := collectMsgs(cmd)
		require.Len(t, msgs, 2)

		openMsg, ok := msgs[0].(completion.OpenMsg)
		require.True(t, ok, "first message should be OpenMsg")
		assert.Equal(t, argComp.argItems, openMsg.Items)
		assert.Equal(t, completion.MatchPrefix, openMsg.MatchMode)

		queryMsg, ok := msgs[1].(completion.QueryMsg)
		require.True(t, ok, "second message should be the seeded QueryMsg")
		assert.Equal(t, "gi", queryMsg.Query)
	})

	t.Run("trailing space with no typed argument yet seeds an empty query", func(t *testing.T) {
		t.Parallel()

		argComp := newArgumentCompletion(true, completion.Item{Label: "github", Value: "/toolset-restart github"})
		e := newArgumentTestEditor("/toolset-restart ", argComp)

		cmd := e.TryStartArgumentCompletion()
		require.NotNil(t, cmd)

		msgs := collectMsgs(cmd)
		require.Len(t, msgs, 2)
		queryMsg, ok := msgs[1].(completion.QueryMsg)
		require.True(t, ok)
		assert.Empty(t, queryMsg.Query)
	})

	// Regression test for bug 2 (PR #3728): a single Tab must reflect the
	// current candidate set on every call, never a snapshot captured by an
	// earlier invocation (the bug required typing a space, deleting it, then
	// Tab again to see fresh candidates).
	t.Run("reflects the current candidate set on each call, not a stale snapshot", func(t *testing.T) {
		t.Parallel()

		current := []completion.Item{{Label: "oldtool", Value: "/toolset-restart oldtool"}}
		argComp := &dynamicArgumentCompletion{
			mockCompletion: mockCompletion{trigger: "/"},
			itemsFunc:      func() []completion.Item { return current },
		}
		e := newArgumentTestEditor("/toolset-restart ", argComp)

		firstCmd := e.TryStartArgumentCompletion()
		require.NotNil(t, firstCmd)
		openMsg, ok := collectMsgs(firstCmd)[0].(completion.OpenMsg)
		require.True(t, ok)
		require.Len(t, openMsg.Items, 1)
		assert.Equal(t, "oldtool", openMsg.Items[0].Label)

		// The underlying toolset set changes (e.g. the agent restarted with a
		// different toolset). A single, fresh Tab press must pick that up.
		e.currentCompletion = nil
		e.argumentPrefix = ""
		current = []completion.Item{{Label: "newtool", Value: "/toolset-restart newtool"}}

		secondCmd := e.TryStartArgumentCompletion()
		require.NotNil(t, secondCmd)
		openMsg, ok = collectMsgs(secondCmd)[0].(completion.OpenMsg)
		require.True(t, ok)
		require.Len(t, openMsg.Items, 1)
		assert.Equal(t, "newtool", openMsg.Items[0].Label, "a fresh Tab must reflect the current candidates, not replay the first call's snapshot")
	})
}

func TestArgumentModeSelectedMsg(t *testing.T) {
	t.Parallel()

	t.Run("Tab (no auto-submit) inserts the full value and keeps editing", func(t *testing.T) {
		t.Parallel()

		e := newArgumentTestEditor("/toolset-restart gi")
		e.argumentPrefix = "/toolset-restart "

		_, cmd := e.Update(completion.SelectedMsg{
			Value:      "/toolset-restart github",
			AutoSubmit: false,
		})

		assert.Nil(t, cmd)
		assert.Equal(t, "/toolset-restart github", e.textarea.Value())
	})

	t.Run("Enter (auto-submit) sends the full value", func(t *testing.T) {
		t.Parallel()

		e := newArgumentTestEditor("/toolset-restart gi")
		e.argumentPrefix = "/toolset-restart "

		_, cmd := e.Update(completion.SelectedMsg{
			Value:      "/toolset-restart github",
			AutoSubmit: true,
		})

		require.NotNil(t, cmd)
		var found bool
		for _, msg := range collectMsgs(cmd) {
			if sm, ok := msg.(messages.SendMsg); ok {
				assert.Equal(t, "/toolset-restart github", sm.Content)
				found = true
			}
		}
		assert.True(t, found, "should send the full argument-mode value")
	})
}

func TestUpdateCompletionQueryArgumentMode(t *testing.T) {
	t.Parallel()

	t.Run("typing further narrows the query", func(t *testing.T) {
		t.Parallel()

		e := newArgumentTestEditor("/toolset-restart git")
		e.argumentPrefix = "/toolset-restart "

		cmd := e.updateCompletionQuery()
		require.NotNil(t, cmd)

		queryMsg, ok := findQueryMsg(cmd)
		require.True(t, ok)
		assert.Equal(t, "git", queryMsg.Query)
		assert.Equal(t, "/toolset-restart ", e.argumentPrefix, "prefix should stay active")
	})

	t.Run("editing away the command prefix closes the session", func(t *testing.T) {
		t.Parallel()

		e := newArgumentTestEditor("/toolset-restar")
		e.argumentPrefix = "/toolset-restart "
		e.currentCompletion = newArgumentCompletion(true)

		cmd := e.updateCompletionQuery()
		require.NotNil(t, cmd)

		assert.True(t, hasCloseMsg(cmd))
		assert.Empty(t, e.argumentPrefix)
		assert.Nil(t, e.currentCompletion)
	})
}

func TestArgumentModeClosedMsgClearsPrefix(t *testing.T) {
	t.Parallel()

	e := newArgumentTestEditor("/toolset-restart github")
	e.argumentPrefix = "/toolset-restart "
	e.currentCompletion = newArgumentCompletion(true)

	_, _ = e.Update(completion.ClosedMsg{})

	assert.Empty(t, e.argumentPrefix)
	assert.Nil(t, e.currentCompletion)
}

// TestTryStartArgumentCompletionSkipsNonArgumentSources ensures
// TryStartArgumentCompletion safely skips sources that don't implement
// ArgumentCompleter (e.g. the "@" file source) instead of panicking on a
// failed type assertion.
func TestTryStartArgumentCompletionSkipsNonArgumentSources(t *testing.T) {
	t.Parallel()

	fileComp := &mockCompletion{trigger: "@"}
	e := newArgumentTestEditor("/toolset-restart gi", fileComp)

	cmd := e.TryStartArgumentCompletion()

	assert.Nil(t, cmd)
}
