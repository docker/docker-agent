package completion

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
)

func TestCircularNavigation(t *testing.T) {
	t.Parallel()

	t.Run("Up arrow at top wraps to bottom", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with multiple items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "item1", Value: "value1"},
				{Label: "item2", Value: "value2"},
				{Label: "item3", Value: "value3"},
			},
		})

		// Initially selected should be 0
		assert.Equal(t, 0, m.selected, "initial selection should be 0")

		// Press Up arrow at top
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})

		// Should wrap to bottom (selected=2, which is len(filteredItems)-1)
		assert.Equal(t, 2, m.selected, "up arrow at top should wrap to bottom")
	})

	t.Run("Down arrow at bottom wraps to top", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with multiple items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "item1", Value: "value1"},
				{Label: "item2", Value: "value2"},
				{Label: "item3", Value: "value3"},
			},
		})

		// Move to bottom (selected=2)
		m.selected = 2
		assert.Equal(t, 2, m.selected, "should be at bottom")

		// Press Down arrow at bottom
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})

		// Should wrap to top (selected=0) and reset scroll offset
		assert.Equal(t, 0, m.selected, "down arrow at bottom should wrap to top")
		assert.Equal(t, 0, m.scrollOffset, "scroll offset should reset when wrapping to top")
	})

	t.Run("scroll offset properly handled during wrapping", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Create more items than maxItems (10) to test scrolling
		items := make([]Item, 15)
		for i := range 15 {
			items[i] = Item{Label: fmt.Sprintf("item%d", i+1), Value: fmt.Sprintf("value%d", i+1)}
		}

		m.Update(OpenMsg{Items: items})

		// Move to bottom (selected=14, which is len(filteredItems)-1)
		m.selected = 14
		m.scrollOffset = 5 // Simulate scrolled state

		// Press Down arrow (should wrap to top)
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})

		assert.Equal(t, 0, m.selected, "should wrap to top")
		assert.Equal(t, 0, m.scrollOffset, "scroll offset should reset to 0 when wrapping to top")

		// Now at bottom, press Up arrow (should wrap to top)
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})

		assert.Equal(t, 14, m.selected, "should wrap to bottom (index 14)")
		// Scroll offset should be adjusted to show the bottom item
		expectedScrollOffset := max(0, 14-maxItems+1) // 14-10+1 = 5
		assert.Equal(t, expectedScrollOffset, m.scrollOffset, "scroll offset should be adjusted for bottom item")

		// Now set to actual bottom and test up wrap
		m.selected = 14
		m.scrollOffset = 5
	})

	t.Run("empty list navigation", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with empty items
		m.Update(OpenMsg{Items: []Item{}})

		// Should not crash on navigation
		originalSelected := m.selected
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assert.Equal(t, originalSelected, m.selected, "up arrow on empty list should not change selection")

		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Equal(t, originalSelected, m.selected, "down arrow on empty list should not change selection")
	})

	t.Run("single item navigation", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with single item
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "onlyItem", Value: "onlyValue"},
			},
		})

		assert.Equal(t, 0, m.selected, "initial selection should be 0")

		// Up arrow on single item (wrap to same item)
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assert.Equal(t, 0, m.selected, "up arrow with single item should stay at 0")
		// Down arrow on single item (wrap to same item)
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Equal(t, 0, m.selected, "down arrow with single item should stay at 0")
	})

	t.Run("normal navigation still works", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with multiple items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "item1", Value: "value1"},
				{Label: "item2", Value: "value2"},
				{Label: "item3", Value: "value3"},
			},
		})

		// Normal down navigation
		assert.Equal(t, 0, m.selected, "should start at 0")

		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Equal(t, 1, m.selected, "down arrow should move to 1")

		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Equal(t, 2, m.selected, "down arrow should move to 2")

		// Normal up navigation
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assert.Equal(t, 1, m.selected, "up arrow should move to 1")

		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assert.Equal(t, 0, m.selected, "up arrow should move to 0")
	})

	t.Run("wrapping with filtered items", func(t *testing.T) {
		t.Parallel()

		m := New().(*manager)

		// Open with multiple items
		m.Update(OpenMsg{
			Items: []Item{
				{Label: "apple", Value: "apple"},
				{Label: "banana", Value: "banana"},
				{Label: "apricot", Value: "apricot"},
				{Label: "berry", Value: "berry"},
			},
		})

		// Apply filter that reduces items
		m.Update(QueryMsg{Query: "ap"}) // Should match "apple" and "apricot"

		assert.Len(t, m.filteredItems, 2, "should have 2 filtered items")
		assert.Equal(t, 0, m.selected, "selection should be at 0 after filtering")

		// Test wrapping with filtered items
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assert.Equal(t, 1, m.selected, "up arrow should wrap to last filtered item (index 1)")

		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Equal(t, 0, m.selected, "down arrow should wrap to first filtered item (index 0)")
	})
}
