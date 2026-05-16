package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
)

var categories = []commands.Category{
	{
		Name: "Session",
		Commands: []commands.Item{
			{
				ID:           "session.new",
				Label:        "New Session",
				SlashCommand: "/new",
				Description:  "Start a new conversation session",
				Category:     "Session",
				Execute:      func(string) tea.Cmd { return nil },
			},
			{
				ID:           "session.compact",
				Label:        "Compact Session",
				SlashCommand: "/compact",
				Description:  "Summarize and compact the current conversation",
				Category:     "Session",
				Execute:      func(string) tea.Cmd { return nil },
			},
		},
	},
}

// multiCategoryCommands has multiple categories for testing line mapping
var multiCategoryCommands = []commands.Category{
	{
		Name: "Session",
		Commands: []commands.Item{
			{ID: "session.new", Label: "New Session", Category: "Session"},
			{ID: "session.save", Label: "Save Session", Category: "Session"},
		},
	},
	{
		Name: "Model",
		Commands: []commands.Item{
			{ID: "model.select", Label: "Select Model", Category: "Model"},
		},
	},
	{
		Name: "Tools",
		Commands: []commands.Item{
			{ID: "tools.list", Label: "List Tools", Category: "Tools"},
			{ID: "tools.refresh", Label: "Refresh Tools", Category: "Tools"},
		},
	},
}

// TestCommandPaletteFilteringIgnoresCategory ensures that typing the name of
// a category does not match every command in that category. Regression test
// for the case where typing "session" would surface every Session-category
// command, defeating the purpose of filtering.
func TestCommandPaletteFilteringIgnoresCategory(t *testing.T) {
	cats := []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{ID: "session.attach", Label: "Attach", SlashCommand: "/attach", Description: "Attach a file to your message", Category: "Session"},
				{ID: "session.history", Label: "Sessions", SlashCommand: "/sessions", Description: "Browse and load past sessions", Category: "Session"},
			},
		},
	}
	dialog := NewCommandPaletteDialog(cats)
	d := dialog.(*commandPaletteDialog)

	d.textInput.SetValue("session")
	d.filterCommands()

	var ids []string
	for _, c := range d.filtered {
		ids = append(ids, c.ID)
	}
	require.Equal(t, []string{"session.history"}, ids,
		"typing 'session' must not surface unrelated commands like 'Attach' just because they share the Session category")
}

func TestCommandPaletteFiltering(t *testing.T) {
	dialog := NewCommandPaletteDialog(categories)
	d := dialog.(*commandPaletteDialog)

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "filter by new",
			input:    "new",
			expected: []string{"session.new"},
		},
		{
			name:     "filter by slash command",
			input:    "/new",
			expected: []string{"session.new"},
		},
		{
			name:     "filter by compact",
			input:    "compact",
			expected: []string{"session.compact"},
		},
		{
			name:     "filter by description",
			input:    "summarize",
			expected: []string{"session.compact"},
		},
		{
			name:     "no match",
			input:    "nonexistent",
			expected: []string{},
		},
		{
			name:     "empty search shows all",
			input:    "",
			expected: []string{"session.new", "session.compact"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.textInput.SetValue(tt.input)
			d.filterCommands()
			require.Len(t, d.filtered, len(tt.expected))

			for i, expectedID := range tt.expected {
				require.Equal(t, expectedID, d.filtered[i].ID)
			}
		})
	}
}

func TestCommandPaletteBuildLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		categories    []commands.Category
		expectedLines int
		expectedMap   []int // expected lineToCmd mapping
	}{
		{
			name:          "single category with 2 commands",
			categories:    categories, // Session: 2 commands → 1 header + 2 commands = 3 lines
			expectedLines: 3,
			expectedMap:   []int{-1, 0, 1}, // header, cmd0, cmd1
		},
		{
			name:          "multiple categories",
			categories:    multiCategoryCommands, // Session(2), Model(1), Tools(2) → 3 headers + 5 commands + 2 blank lines = 10 lines
			expectedLines: 10,
			// Line layout: header, cmd0, cmd1, blank, header, cmd2, blank, header, cmd3, cmd4
			expectedMap: []int{-1, 0, 1, -1, -1, 2, -1, -1, 3, 4},
		},
		{
			name:          "empty categories",
			categories:    []commands.Category{},
			expectedLines: 0,
			expectedMap:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dialog := NewCommandPaletteDialog(tt.categories)
			d := dialog.(*commandPaletteDialog)
			lines, lineToCmd := d.buildLines(0)
			assert.Len(t, lines, tt.expectedLines)
			assert.Equal(t, tt.expectedMap, lineToCmd)
		})
	}
}

func TestCommandPaletteFindSelectedLine(t *testing.T) {
	t.Parallel()

	dialog := NewCommandPaletteDialog(multiCategoryCommands)
	d := dialog.(*commandPaletteDialog)

	// Expected: selecting command index N should return the correct line
	// (accounting for blank lines before non-first categories)
	tests := []struct {
		selected     int
		expectedLine int
	}{
		{0, 1}, // session.new is at line 1 (after Session header)
		{1, 2}, // session.save is at line 2
		{2, 5}, // model.select is at line 5 (after blank + Model header)
		{3, 8}, // tools.list is at line 8 (after blank + Tools header)
		{4, 9}, // tools.refresh is at line 9
	}

	for _, tt := range tests {
		d.selected = tt.selected
		assert.Equal(t, tt.expectedLine, d.findSelectedLine(),
			"findSelectedLine() with selected=%d should return %d", tt.selected, tt.expectedLine)
	}
}

func TestCommandPaletteLineMappingWithFiltering(t *testing.T) {
	t.Parallel()

	dialog := NewCommandPaletteDialog(multiCategoryCommands)
	d := dialog.(*commandPaletteDialog)

	// Filter to show only "Session" category commands
	d.textInput.SetValue("session")
	d.filterCommands()

	// Should have 2 commands from Session category
	require.Len(t, d.filtered, 2)

	// Line layout after filtering:
	// Line 0: "Session" header
	// Line 1: session.new (index 0)
	// Line 2: session.save (index 1)
	lines, lineToCmd := d.buildLines(0)
	assert.Len(t, lines, 3)
	assert.Equal(t, []int{-1, 0, 1}, lineToCmd)

	// Test findSelectedLine
	d.selected = 0
	assert.Equal(t, 1, d.findSelectedLine())
	d.selected = 1
	assert.Equal(t, 2, d.findSelectedLine())
}
