package completions

import (
	"context"
	"slices"
	"sync"

	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

// Initial loading limits for snappy UX
const (
	initialMaxFiles = 100
	initialMaxDepth = 2
)

type fileCompletion struct {
	mu     sync.Mutex
	items  []completion.Item
	loaded bool
}

func NewFileCompletion() Completion {
	return &fileCompletion{}
}

func (c *fileCompletion) AutoSubmit() bool {
	return false
}

func (c *fileCompletion) RequiresEmptyEditor() bool {
	return false
}

func (c *fileCompletion) Trigger() string {
	return "@"
}

func (c *fileCompletion) Items() []completion.Item {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return cached items if already loaded
	if c.loaded {
		return c.items
	}

	// Try to create VCS matcher for current directory
	vcsMatcher, _ := fsx.NewVCSMatcher(".")

	// Prepare shouldIgnore function
	var shouldIgnore func(string) bool
	if vcsMatcher != nil {
		shouldIgnore = vcsMatcher.ShouldIgnore
	}

	// Use bounded walker to avoid scanning huge directories
	files, err := fsx.WalkFiles(context.Background(), ".", fsx.WalkFilesOptions{
		ShouldIgnore: shouldIgnore,
	})
	if err != nil {
		// Do not mark as loaded on error, allow retry
		return nil
	}

	// Sort files by name
	slices.Sort(files)

	items := make([]completion.Item, len(files))
	for i, f := range files {
		items[i] = completion.Item{
			Label: f,
			Value: "@" + f,
		}
	}

	c.items = items
	c.loaded = true

	return c.items
}

// LoadInitialItemsAsync loads a shallow set of items quickly for immediate display.
// It scans 2 levels deep with a max of 100 files for a snappy initial UX.
func (c *fileCompletion) LoadInitialItemsAsync(ctx context.Context) <-chan []completion.Item {
	ch := make(chan []completion.Item, 1)

	go func() {
		defer close(ch)

		// Check if we already have full items cached
		c.mu.Lock()
		if c.loaded {
			items := c.items
			c.mu.Unlock()
			select {
			case ch <- items:
			case <-ctx.Done():
			}
			return
		}
		c.mu.Unlock()

		// Try to create VCS matcher for current directory
		vcsMatcher, _ := fsx.NewVCSMatcher(".")

		var shouldIgnore func(string) bool
		if vcsMatcher != nil {
			shouldIgnore = vcsMatcher.ShouldIgnore
		}

		// Shallow scan: 2 levels deep, max 100 files
		files, err := fsx.WalkFiles(ctx, ".", fsx.WalkFilesOptions{
			MaxFiles:     initialMaxFiles,
			MaxDepth:     initialMaxDepth,
			ShouldIgnore: shouldIgnore,
		})
		if err != nil || ctx.Err() != nil {
			select {
			case ch <- nil:
			case <-ctx.Done():
			}
			return
		}

		// Sort files by name
		slices.Sort(files)

		items := make([]completion.Item, len(files))
		for i, f := range files {
			items[i] = completion.Item{
				Label: f,
				Value: "@" + f,
			}
		}

		// Don't cache initial items - we'll cache full items later
		select {
		case ch <- items:
		case <-ctx.Done():
		}
	}()

	return ch
}

// LoadItemsAsync loads all file items in a background goroutine with context support.
// It returns a channel that receives the items when loading is complete.
func (c *fileCompletion) LoadItemsAsync(ctx context.Context) <-chan []completion.Item {
	ch := make(chan []completion.Item, 1)

	go func() {
		defer close(ch)

		c.mu.Lock()
		// Return cached items if already loaded
		if c.loaded {
			items := c.items
			c.mu.Unlock()
			select {
			case ch <- items:
			case <-ctx.Done():
			}
			return
		}
		c.mu.Unlock()

		// Try to create VCS matcher for current directory
		vcsMatcher, _ := fsx.NewVCSMatcher(".")

		// Prepare shouldIgnore function
		var shouldIgnore func(string) bool
		if vcsMatcher != nil {
			shouldIgnore = vcsMatcher.ShouldIgnore
		}

		// Full scan with default limits
		files, err := fsx.WalkFiles(ctx, ".", fsx.WalkFilesOptions{
			ShouldIgnore: shouldIgnore,
		})
		if err != nil || ctx.Err() != nil {
			// Return nil on error or cancellation
			select {
			case ch <- nil:
			case <-ctx.Done():
			}
			return
		}

		// Sort files by name
		slices.Sort(files)

		items := make([]completion.Item, len(files))
		for i, f := range files {
			items[i] = completion.Item{
				Label: f,
				Value: "@" + f,
			}
		}

		// Cache the results
		c.mu.Lock()
		c.items = items
		c.loaded = true
		c.mu.Unlock()

		select {
		case ch <- items:
		case <-ctx.Done():
		}
	}()

	return ch
}

func (c *fileCompletion) MatchMode() completion.MatchMode {
	return completion.MatchFuzzy
}
