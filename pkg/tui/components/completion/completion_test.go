package completion

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
)

func TestCompletionManagerStaysOpenWithNoResults(t *testing.T) {
	t.Parallel()

	t.Run("query with no matches keeps popup visible", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with some items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "new", Value: "/new"},
				{Label: "exit", Value: "/exit"},
			},
		})

		assert.True(t, m.visible, "should be visible after open")
		assert.Len(t, m.filteredItems, 2, "should have 2 items")

		// Query that matches nothing
		m.Update(QueryMsg{Query: "xyz"})

		assert.True(t, m.visible, "should stay visible even with no matches")
		assert.Empty(t, m.filteredItems, "should have no filtered items")
	})

	t.Run("backspace from no-match restores items", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with some items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "new", Value: "/new"},
				{Label: "exit", Value: "/exit"},
			},
		})

		// Query that matches nothing
		m.Update(QueryMsg{Query: "xyz"})
		assert.True(t, m.visible, "should stay visible with no matches")
		assert.Empty(t, m.filteredItems)

		// Backspace to shorter query that still matches nothing
		m.Update(QueryMsg{Query: "xy"})
		assert.True(t, m.visible, "should stay visible")
		assert.Empty(t, m.filteredItems)

		// Backspace to query that matches "exit"
		m.Update(QueryMsg{Query: "ex"})
		assert.True(t, m.visible, "should stay visible")
		assert.Len(t, m.filteredItems, 1, "should match 'exit'")
		assert.Equal(t, "exit", m.filteredItems[0].Label)

		// Backspace to empty query - should show all items
		m.Update(QueryMsg{Query: ""})
		assert.True(t, m.visible, "should stay visible")
		assert.Len(t, m.filteredItems, 2, "should show all items")
	})

	t.Run("view shows no results message when empty", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)
		m.width = 80
		m.height = 24

		// Open with some items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "new", Value: "/new"},
			},
		})

		// Query that matches nothing
		m.Update(QueryMsg{Query: "xyz"})

		view := m.View()
		assert.Contains(t, view, "No results", "should show no results message")
	})
}

func TestCompletionManagerAppendItems(t *testing.T) {
	t.Parallel()

	t.Run("AppendItemsMsg adds items to existing list", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with initial items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "initial", Value: "/initial"},
			},
		})
		assert.Len(t, m.items, 1)
		assert.Len(t, m.filteredItems, 1)

		// Append more items
		m.Update(AppendItemsMsg{
			Items: []Item{
				{Label: "appended1", Value: "/appended1"},
				{Label: "appended2", Value: "/appended2"},
			},
		})

		assert.Len(t, m.items, 3, "should have 3 total items")
		assert.Len(t, m.filteredItems, 3, "filtered should also have 3 items")
	})

	t.Run("AppendItemsMsg respects current query filter", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with initial items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "foo", Value: "/foo"},
			},
		})

		// Set a query filter
		m.Update(QueryMsg{Query: "bar"})
		assert.Empty(t, m.filteredItems, "no items match 'bar'")

		// Append items that match the filter
		m.Update(AppendItemsMsg{
			Items: []Item{
				{Label: "bar", Value: "/bar"},
				{Label: "barbaz", Value: "/barbaz"},
				{Label: "nomatch", Value: "/nomatch"},
			},
		})

		assert.Len(t, m.items, 4, "should have 4 total items")
		assert.Len(t, m.filteredItems, 2, "only 2 items match 'bar'")
	})

	t.Run("AppendItemsMsg makes popup visible if items match", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with no items initially
		m.Update(OpenMsg{Items: []Item{}})
		assert.False(t, m.visible, "should not be visible with no items")

		// Append items
		m.Update(AppendItemsMsg{
			Items: []Item{
				{Label: "new", Value: "/new"},
			},
		})

		assert.True(t, m.visible, "should become visible after appending items")
	})
}

func TestCompletionManagerReplaceItems(t *testing.T) {
	t.Parallel()

	t.Run("ReplaceItemsMsg replaces non-pinned items", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with initial items (some pinned, some not)
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "Browse files…", Value: "", Pinned: true},
				{Label: "old1.txt", Value: "@old1.txt"},
				{Label: "old2.txt", Value: "@old2.txt"},
			},
		})
		assert.Len(t, m.items, 3)

		// Replace with new items
		m.Update(ReplaceItemsMsg{
			Items: []Item{
				{Label: "new1.txt", Value: "@new1.txt"},
				{Label: "new2.txt", Value: "@new2.txt"},
				{Label: "new3.txt", Value: "@new3.txt"},
			},
		})

		// Should have pinned item + 3 new items
		assert.Len(t, m.items, 4, "should have 4 items total")

		// First item should be pinned
		assert.Equal(t, "Browse files…", m.items[0].Label)
		assert.True(t, m.items[0].Pinned)

		// Rest should be new items
		assert.Equal(t, "new1.txt", m.items[1].Label)
		assert.Equal(t, "new2.txt", m.items[2].Label)
		assert.Equal(t, "new3.txt", m.items[3].Label)
	})

	t.Run("ReplaceItemsMsg preserves multiple pinned items", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with multiple pinned items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "paste-1", Value: "@paste-1", Pinned: true},
				{Label: "paste-2", Value: "@paste-2", Pinned: true},
				{Label: "Browse files…", Value: "", Pinned: true},
				{Label: "old.txt", Value: "@old.txt"},
			},
		})

		// Replace with new items
		m.Update(ReplaceItemsMsg{
			Items: []Item{
				{Label: "new.txt", Value: "@new.txt"},
			},
		})

		// Should have 3 pinned items + 1 new item
		assert.Len(t, m.items, 4, "should preserve all pinned items")

		pinnedCount := 0
		for _, item := range m.items {
			if item.Pinned {
				pinnedCount++
			}
		}
		assert.Equal(t, 3, pinnedCount, "should have 3 pinned items")
	})
}

func TestCompletionManagerLoading(t *testing.T) {
	t.Parallel()

	t.Run("SetLoadingMsg updates loading state", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)
		assert.False(t, m.loading)

		m.Update(SetLoadingMsg{Loading: true})
		assert.True(t, m.loading)

		m.Update(SetLoadingMsg{Loading: false})
		assert.False(t, m.loading)
	})

	t.Run("CloseMsg resets loading state", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)
		m.Update(SetLoadingMsg{Loading: true})
		assert.True(t, m.loading)

		m.Update(CloseMsg{})
		assert.False(t, m.loading)
	})

	t.Run("view shows Loading message when loading with no items", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)
		m.width = 80
		m.height = 24

		// Open with no items but set loading
		m.Update(OpenMsg{Items: []Item{}})
		m.visible = true // force visible for test
		m.Update(SetLoadingMsg{Loading: true})

		view := m.View()
		assert.Contains(t, view, "Loading", "should show loading message")
	})
}

func TestCompletionManagerPinnedItems(t *testing.T) {
	t.Parallel()

	t.Run("pinned items always appear regardless of query", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with mixed pinned and regular items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "Browse files…", Value: "", Pinned: true},
				{Label: "main.go", Value: "@main.go"},
				{Label: "utils.go", Value: "@utils.go"},
			},
		})

		// Query that doesn't match any regular items
		m.Update(QueryMsg{Query: "xyz"})

		// Pinned item should still appear
		assert.Len(t, m.filteredItems, 1, "pinned item should always appear")
		assert.Equal(t, "Browse files…", m.filteredItems[0].Label)
		assert.True(t, m.filteredItems[0].Pinned)
	})

	t.Run("pinned items appear at top of results", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with pinned item last in list
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "main.go", Value: "@main.go"},
				{Label: "utils.go", Value: "@utils.go"},
				{Label: "Browse files…", Value: "", Pinned: true},
			},
		})

		// Query that matches regular items
		m.Update(QueryMsg{Query: "main"})

		// Pinned item should be first
		assert.Len(t, m.filteredItems, 2, "pinned + matching item")
		assert.Equal(t, "Browse files…", m.filteredItems[0].Label, "pinned should be first")
		assert.Equal(t, "main.go", m.filteredItems[1].Label, "matching item should be second")
	})
}

// extractSequenceCmds extracts the slice of commands from a tea.SequenceMsg using reflection,
// since tea.sequenceMsg is unexported.
func extractSequenceCmds(c tea.Cmd) []tea.Cmd {
	if c == nil {
		return nil
	}
	seqMsg := c()
	v := reflect.ValueOf(seqMsg)
	var cmds []tea.Cmd
	if v.Kind() == reflect.Slice {
		for i := range v.Len() {
			cmd, ok := v.Index(i).Interface().(tea.Cmd)
			if ok {
				cmds = append(cmds, cmd)
			}
		}
	}
	return cmds
}

func TestCompletionManagerAutoSubmit(t *testing.T) {
	t.Parallel()

	t.Run("enter triggers auto submit", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		m.Update(OpenMsg{
			Items: []Item{
				{Label: "option", Value: "/option"},
			},
		})

		_, c := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

		cmds := extractSequenceCmds(c)

		assert.False(t, m.visible, "completion view should close")
		assert.Len(t, cmds, 2, "should return a sequence of 2 commands")

		if len(cmds) > 0 {
			msg0 := cmds[0]()
			selectedMsg, ok := msg0.(SelectedMsg)
			assert.True(t, ok, "first message should be SelectedMsg")
			assert.True(t, selectedMsg.AutoSubmit, "should have auto submit true")
		}
	})

	t.Run("tab disables auto submit", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		m.Update(OpenMsg{
			Items: []Item{
				{Label: "option", Value: "/option"},
			},
		})

		_, c := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})

		cmds := extractSequenceCmds(c)

		assert.False(t, m.visible, "completion view should close")
		assert.Len(t, cmds, 2, "should return a sequence of 2 commands")

		if len(cmds) > 0 {
			msg0 := cmds[0]()
			selectedMsg, ok := msg0.(SelectedMsg)
			assert.True(t, ok, "first message should be SelectedMsg")
			assert.False(t, selectedMsg.AutoSubmit, "should have auto submit false")
		}
	})
}
