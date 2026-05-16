package completions

import (
	"context"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

type Completion interface {
	Trigger() string
	Items() []completion.Item
	RequiresEmptyEditor() bool
	// MatchMode returns how items should be filtered (fuzzy or prefix)
	MatchMode() completion.MatchMode
}

// AsyncLoader is an optional interface for completions that support async loading.
// This allows the editor to load items in the background without blocking the UI.
type AsyncLoader interface {
	// LoadInitialItemsAsync loads a shallow set of items quickly (e.g., 2 levels deep, ~100 files).
	// Returns a channel that receives initial items for immediate display.
	LoadInitialItemsAsync(ctx context.Context) <-chan []completion.Item

	// LoadItemsAsync loads all items in a background goroutine with context support.
	// It returns a channel that receives the items when loading is complete.
	LoadItemsAsync(ctx context.Context) <-chan []completion.Item
}
