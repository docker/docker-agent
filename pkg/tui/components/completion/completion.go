package completion

import (
	"cmp"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const maxItems = 10

// MatchMode defines how completion items are filtered
type MatchMode int

const (
	// MatchFuzzy uses fuzzy matching (matches anywhere in label)
	MatchFuzzy MatchMode = iota
	// MatchPrefix requires the query to match the start of the label
	MatchPrefix
)

type Item struct {
	Label       string
	Description string
	Value       string
	Execute     func() tea.Cmd
	Pinned      bool // Pinned items always appear at the top, in original order
}

type OpenMsg struct {
	Items     []Item
	MatchMode MatchMode
}

type OpenedMsg struct{}

type CloseMsg struct{}

type ClosedMsg struct{}

type QueryMsg struct {
	Query string
}

type SelectedMsg struct {
	Value      string
	Execute    func() tea.Cmd
	AutoSubmit bool
}

// SelectionChangedMsg is sent when the selected item changes (for preview in editor)
type SelectionChangedMsg struct {
	Value string
}

// AppendItemsMsg appends items to the current completion list without closing the popup.
// Useful for async loading of completion items.
type AppendItemsMsg struct {
	Items []Item
}

// ReplaceItemsMsg replaces non-pinned items in the completion list.
// Pinned items (like "Browse files…") are preserved.
// Useful for full async load that supersedes initial results.
type ReplaceItemsMsg struct {
	Items []Item
}

// SetLoadingMsg sets the loading state for the completion popup.
type SetLoadingMsg struct {
	Loading bool
}

type matchResult struct {
	item  Item
	score int
}

type completionKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	Tab    key.Binding
	Escape key.Binding
}

// defaultCompletionKeyMap returns default key bindings
func defaultCompletionKeyMap() completionKeyMap {
	return completionKeyMap{
		Up: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("↑", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("↓", "down"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "autocomplete"),
		),
		Escape: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel"),
		),
	}
}

// Manager manages the dialog stack and rendering
type Manager interface {
	layout.Model

	GetLayers() []*lipgloss.Layer
	Open() bool
	// SetEditorBottom sets the height from the bottom of the screen where the editor ends.
	// This is used to position the completion popup above the editor.
	SetEditorBottom(height int)
}

// manager represents an item completion component that manages completion state and UI
type manager struct {
	keyMap        completionKeyMap
	width         int
	height        int
	editorBottom  int // height from screen bottom where editor ends (for popup positioning)
	items         []Item
	filteredItems []Item
	query         string
	selected      int
	scrollOffset  int
	visible       bool
	matchMode     MatchMode
	loading       bool // true when async loading is in progress
}

// New creates a new  completion component
func New() Manager {
	return &manager{
		keyMap: defaultCompletionKeyMap(),
	}
}

func (c *manager) Init() tea.Cmd {
	return nil
}

func (c *manager) Open() bool {
	return c.visible
}

func (c *manager) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width = msg.Width
		c.height = msg.Height
		return c, nil

	case QueryMsg:
		c.query = msg.Query
		c.filterItems(c.query)
		// Keep the popup visible even with no results - user can backspace to broaden the query
		cmd := c.notifySelectionChanged()
		return c, cmd

	case OpenMsg:
		c.items = msg.Items
		c.matchMode = msg.MatchMode
		c.selected = 0
		c.scrollOffset = 0
		c.query = "" // Reset query when opening new completion
		c.filterItems(c.query)
		c.visible = len(c.filteredItems) > 0
		if !c.visible {
			return c, nil
		}
		return c, tea.Batch(
			core.CmdHandler(OpenedMsg{}),
			c.notifySelectionChanged(),
		)

	case CloseMsg:
		c.visible = false
		c.loading = false
		return c, nil

	case SetLoadingMsg:
		c.loading = msg.Loading
		return c, nil

	case AppendItemsMsg:
		// Append new items to the existing list
		c.items = append(c.items, msg.Items...)
		// Re-filter with current query
		c.filterItems(c.query)
		// Make popup visible if we now have items
		if len(c.filteredItems) > 0 && !c.visible {
			c.visible = true
		}
		cmd := c.notifySelectionChanged()
		return c, cmd

	case ReplaceItemsMsg:
		// Keep pinned items, replace everything else
		var pinnedItems []Item
		for _, item := range c.items {
			if item.Pinned {
				pinnedItems = append(pinnedItems, item)
			}
		}
		// Combine pinned items with new items
		c.items = append(pinnedItems, msg.Items...)
		// Re-filter with current query
		c.filterItems(c.query)
		// Make popup visible if we have items
		if len(c.filteredItems) > 0 && !c.visible {
			c.visible = true
		}
		cmd := c.notifySelectionChanged()
		return c, cmd

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, c.keyMap.Up):
			if c.selected > 0 {
				c.selected--
			}
			if c.selected < c.scrollOffset {
				c.scrollOffset = c.selected
			}
			cmd := c.notifySelectionChanged()
			return c, cmd

		case key.Matches(msg, c.keyMap.Down):
			if c.selected < len(c.filteredItems)-1 {
				c.selected++
			}
			if c.selected >= c.scrollOffset+maxItems {
				c.scrollOffset = c.selected - maxItems + 1
			}
			cmd := c.notifySelectionChanged()
			return c, cmd

		case key.Matches(msg, c.keyMap.Enter):
			c.visible = false
			if len(c.filteredItems) == 0 || c.selected >= len(c.filteredItems) {
				return c, core.CmdHandler(ClosedMsg{})
			}
			selectedItem := c.filteredItems[c.selected]
			return c, tea.Sequence(
				core.CmdHandler(SelectedMsg{
					Value:      selectedItem.Value,
					Execute:    selectedItem.Execute,
					AutoSubmit: true,
				}),
				core.CmdHandler(ClosedMsg{}),
			)
		case key.Matches(msg, c.keyMap.Tab):
			c.visible = false
			if len(c.filteredItems) == 0 || c.selected >= len(c.filteredItems) {
				return c, core.CmdHandler(ClosedMsg{})
			}
			selectedItem := c.filteredItems[c.selected]
			return c, tea.Sequence(
				core.CmdHandler(SelectedMsg{
					Value:      selectedItem.Value,
					Execute:    selectedItem.Execute,
					AutoSubmit: false,
				}),
				core.CmdHandler(ClosedMsg{}),
			)
		case key.Matches(msg, c.keyMap.Escape):
			c.visible = false
			return c, core.CmdHandler(ClosedMsg{})
		}
	}

	return c, nil
}

func (c *manager) SetSize(width, height int) tea.Cmd {
	c.width = width
	c.height = height
	return nil
}

func (c *manager) SetEditorBottom(height int) {
	c.editorBottom = height
}

func (c *manager) View() string {
	if !c.visible {
		return ""
	}

	var lines []string

	if len(c.filteredItems) == 0 {
		if c.loading {
			lines = append(lines, styles.CompletionNoResultsStyle.Render("Loading…"))
		} else {
			lines = append(lines, styles.CompletionNoResultsStyle.Render("No results"))
		}
	} else {
		visibleStart := c.scrollOffset
		visibleEnd := min(c.scrollOffset+maxItems, len(c.filteredItems))

		maxLabelLen := 0
		for i := visibleStart; i < visibleEnd; i++ {
			labelLen := lipgloss.Width(c.filteredItems[i].Label)
			if labelLen > maxLabelLen {
				maxLabelLen = labelLen
			}
		}

		for i := visibleStart; i < visibleEnd; i++ {
			item := c.filteredItems[i]
			isSelected := i == c.selected

			itemStyle := styles.CompletionNormalStyle
			descStyle := styles.CompletionDescStyle
			if isSelected {
				itemStyle = styles.CompletionSelectedStyle
				descStyle = styles.CompletionSelectedDescStyle
			}

			// Pad label to maxLabelLen so descriptions align
			paddedLabel := item.Label + strings.Repeat(" ", maxLabelLen+1-lipgloss.Width(item.Label))
			text := paddedLabel
			if item.Description != "" {
				text += " " + descStyle.Render(item.Description)
			}

			lines = append(lines, itemStyle.Width(c.width-6).Render(text))
		}
	}

	content := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return styles.CompletionBoxStyle.Render(content)
}

func (c *manager) GetLayers() []*lipgloss.Layer {
	if !c.visible {
		return nil
	}

	view := c.View()
	viewHeight := lipgloss.Height(view)

	// Use actual editor height if set, otherwise fall back to reasonable default
	editorHeight := cmp.Or(c.editorBottom, 4)
	yPos := max(c.height-viewHeight-editorHeight-1, 0)

	return []*lipgloss.Layer{
		lipgloss.NewLayer(view).X(styles.AppPadding).Y(yPos),
	}
}

// notifySelectionChanged sends a SelectionChangedMsg with the currently selected item's value
func (c *manager) notifySelectionChanged() tea.Cmd {
	if len(c.filteredItems) == 0 || c.selected >= len(c.filteredItems) {
		return core.CmdHandler(SelectionChangedMsg{Value: ""})
	}
	return core.CmdHandler(SelectionChangedMsg{Value: c.filteredItems[c.selected].Value})
}

func (c *manager) filterItems(query string) {
	// Pinned items are always shown at the top, in their original order.
	var pinnedItems []Item
	for _, item := range c.items {
		if item.Pinned {
			pinnedItems = append(pinnedItems, item)
		}
	}

	if query == "" {
		// Preserve original order for non-pinned items.
		c.filteredItems = make([]Item, 0, len(c.items))
		c.filteredItems = append(c.filteredItems, pinnedItems...)
		for _, item := range c.items {
			if !item.Pinned {
				c.filteredItems = append(c.filteredItems, item)
			}
		}
		// Reset selection when clearing the query
		if c.selected >= len(c.filteredItems) {
			c.selected = max(0, len(c.filteredItems)-1)
		}
		return
	}

	lowerQuery := strings.ToLower(query)
	var matches []matchResult

	for _, item := range c.items {
		if item.Pinned {
			continue
		}
		var matched bool
		var score int

		if c.matchMode == MatchPrefix {
			// Prefix matching: label must start with query (case-insensitive)
			if strings.HasPrefix(strings.ToLower(item.Label), lowerQuery) {
				matched = true
				score = 1000 - len(item.Label) // Shorter labels rank higher
			}
		} else {
			// Fuzzy matching
			pattern := []rune(lowerQuery)
			chars := util.ToChars([]byte(item.Label))
			result, _ := algo.FuzzyMatchV1(
				false, // caseSensitive
				false, // normalize
				true,  // forward
				&chars,
				pattern,
				true, // withPos
				nil,  // slab
			)
			if result.Start >= 0 {
				matched = true
				score = result.Score
			}
		}

		if matched {
			matches = append(matches, matchResult{
				item:  item,
				score: score,
			})
		}
	}

	slices.SortFunc(matches, func(a, b matchResult) int {
		return cmp.Compare(b.score, a.score)
	})

	// Build result: pinned items first, then sorted matches
	c.filteredItems = make([]Item, 0, len(pinnedItems)+len(matches))
	c.filteredItems = append(c.filteredItems, pinnedItems...)
	for _, match := range matches {
		c.filteredItems = append(c.filteredItems, match.item)
	}

	// Adjust selection if it's beyond the filtered list
	if c.selected >= len(c.filteredItems) {
		c.selected = max(0, len(c.filteredItems)-1)
	}

	// Adjust scroll offset to ensure selected item is visible
	if c.selected < c.scrollOffset {
		c.scrollOffset = c.selected
	} else if c.selected >= c.scrollOffset+maxItems {
		c.scrollOffset = max(0, c.selected-maxItems+1)
	}
}
