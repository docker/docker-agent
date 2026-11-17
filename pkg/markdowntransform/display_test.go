package markdowntransform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitChanged(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("transform did not complete")
	}
}

func TestApplyKeepsPrecedingMarkdownOnFailure(t *testing.T) {
	t.Parallel()
	out := apply(t.Context(), []Transformer{
		func(_ context.Context, in Input) (string, error) { return in.Markdown + " first", nil },
		func(context.Context, Input) (string, error) { return "discarded", errors.New("failed") },
		func(_ context.Context, in Input) (string, error) { return in.Markdown + " last", nil },
	}, Input{Markdown: "source"})
	assert.Equal(t, "source first last", out)
}

func TestDisplayShowsLatestResultWithoutBlocking(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	calls := make(chan Input, 10)
	display := NewDisplay(func(ctx context.Context, in Input) (string, error) {
		calls <- in
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "T:" + in.Markdown, nil
	})
	display.Start(t.Context())
	var surface Surface
	changed := display.Changed()
	assert.Equal(t, "a", display.Markdown(&surface, Input{Markdown: "a"}), "source is shown until a result exists")
	<-calls
	for _, markdown := range []string{"ab", "abc", "abcd"} {
		assert.Equal(t, markdown, display.Markdown(&surface, Input{Markdown: markdown}))
	}
	release <- struct{}{}
	waitChanged(t, changed)
	changed = display.Changed()
	assert.Equal(t, "T:a", display.Markdown(&surface, Input{Markdown: "abcd"}), "the previous result avoids flicker")
	assert.Equal(t, "abcd", (<-calls).Markdown, "only the latest pending snapshot is transformed")
	close(release)
	waitChanged(t, changed)
	assert.Equal(t, "T:abcd", display.Markdown(&surface, Input{Markdown: "abcd"}))
	for range 10 {
		display.Markdown(&surface, Input{Markdown: "abcd"})
	}
	assert.Empty(t, calls, "repeated renders reuse the result")
}

func TestDisplayTransformsNewestRequestsFirst(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	calls := make(chan string, 10)
	display := NewDisplay(func(_ context.Context, in Input) (string, error) {
		calls <- in.Markdown
		<-release
		return in.Markdown, nil
	})
	display.Start(t.Context())
	var busy, old, live Surface
	display.Markdown(&busy, Input{Markdown: "busy"})
	<-calls
	display.Markdown(&old, Input{Markdown: "history"})
	display.Markdown(&live, Input{Markdown: "live", IsStreaming: true})
	close(release)
	assert.Equal(t, "live", <-calls)
	assert.Equal(t, "history", <-calls)
}

func TestDisplayPromotesQueuedSurfaceOnRender(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	calls := make(chan string, 10)
	display := NewDisplay(func(_ context.Context, in Input) (string, error) {
		calls <- in.Markdown
		<-release
		return in.Markdown, nil
	})
	display.Start(t.Context())
	var busy, live, older Surface
	display.Markdown(&busy, Input{Markdown: "busy"})
	<-calls
	display.Markdown(&live, Input{Markdown: "live"})
	// A resize renders history first and the live reply last.
	display.Markdown(&older, Input{Markdown: "history"})
	display.Markdown(&live, Input{Markdown: "live"})
	close(release)
	assert.Equal(t, "live", <-calls)
	assert.Equal(t, "history", <-calls)
}

func TestDisplayRevertingInputKeepsLatestSnapshot(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	calls := make(chan Input, 10)
	display := NewDisplay(func(_ context.Context, in Input) (string, error) {
		calls <- in
		<-release
		return fmt.Sprintf("T%d", in.AvailableWidth), nil
	})
	display.Start(t.Context())
	wide, narrow := Input{Markdown: "x", AvailableWidth: 80}, Input{Markdown: "x", AvailableWidth: 40}

	// A in flight, B wanted, back to A: A's result is current, with no empty or
	// redundant request.
	var s Surface
	display.Markdown(&s, wide)
	assert.Equal(t, wide, <-calls)
	display.Markdown(&s, narrow)
	display.Markdown(&s, wide)
	release <- struct{}{}
	require.Eventually(t, func() bool { return display.Current(&s, wide) }, 5*time.Second, time.Millisecond)
	assert.Empty(t, calls)

	// A done, B in flight, back to A: B's result must not stay current.
	display.Markdown(&s, narrow)
	assert.Equal(t, narrow, <-calls)
	assert.Equal(t, "T80", display.Markdown(&s, wide))
	release <- struct{}{}
	assert.Equal(t, wide, <-calls, "returning to A re-requests it after B")
	close(release)
	require.Eventually(t, func() bool { return display.Markdown(&s, wide) == "T80" && display.Current(&s, wide) }, 5*time.Second, time.Millisecond)
}

func TestDisplayCurrent(t *testing.T) {
	t.Parallel()
	display := NewDisplay(func(_ context.Context, in Input) (string, error) { return "T:" + in.Markdown, nil })
	display.Start(t.Context())
	var s Surface
	changed := display.Changed()
	display.Markdown(&s, Input{Markdown: "a"})
	waitChanged(t, changed)
	assert.True(t, display.Current(&s, Input{Markdown: "a"}))
	assert.False(t, display.Current(&s, Input{Markdown: "ab"}))
	assert.True(t, (*Display)(nil).Current(&s, Input{Markdown: "ab"}))
}

func TestDisplayVersionChangesOnlyWithOutput(t *testing.T) {
	t.Parallel()
	display := NewDisplay(func(context.Context, Input) (string, error) { return "same", nil })
	display.Start(t.Context())
	var surface Surface
	assert.Zero(t, display.Version(&surface))
	for _, width := range []int{80, 40} {
		changed := display.Changed()
		display.Markdown(&surface, Input{Markdown: "x", AvailableWidth: width})
		waitChanged(t, changed)
	}
	assert.Equal(t, uint64(1), display.Version(&surface))
}

func TestDisplayNotifiesEveryWaiter(t *testing.T) {
	t.Parallel()
	display := NewDisplay(func(_ context.Context, in Input) (string, error) { return in.Markdown, nil })
	display.Start(t.Context())
	first, second := display.Changed(), display.Changed()
	var surface Surface
	display.Markdown(&surface, Input{Markdown: "x"})
	waitChanged(t, first)
	waitChanged(t, second)
}

func TestNilDisplay(t *testing.T) {
	t.Parallel()
	var display *Display
	var surface Surface
	assert.Equal(t, "source", display.Markdown(&surface, Input{Markdown: "source"}))
	assert.Zero(t, display.Version(&surface))
	require.Nil(t, display.Changed())
}

func TestDisplayThrottlesStreamingSurfaces(t *testing.T) {
	t.Parallel()
	calls := make(chan string, 10)
	display := NewDisplay(func(_ context.Context, in Input) (string, error) {
		calls <- in.Markdown
		return "T:" + in.Markdown, nil
	})
	display.streamInterval = time.Hour
	display.Start(t.Context())
	var live, history Surface
	changed := display.Changed()
	display.Markdown(&live, Input{Markdown: "a", IsStreaming: true})
	waitChanged(t, changed)
	assert.Equal(t, "a", <-calls, "a reply's first result is not delayed")

	changed = display.Changed()
	display.Markdown(&history, Input{Markdown: "old"})
	display.Markdown(&live, Input{Markdown: "ab", IsStreaming: true})
	display.Markdown(&live, Input{Markdown: "abc", IsStreaming: true})
	waitChanged(t, changed)
	assert.Equal(t, "old", <-calls, "a throttled reply does not hold up other surfaces")
	assert.Equal(t, "T:a", display.Markdown(&live, Input{Markdown: "abc", IsStreaming: true}))

	changed = display.Changed()
	display.Markdown(&live, Input{Markdown: "abc"})
	waitChanged(t, changed)
	assert.Equal(t, "abc", <-calls, "the completed snapshot is not delayed")
	assert.Empty(t, calls, "intermediate snapshots are skipped")
}

func TestDisplayTransformsLatestStreamingSnapshotWhenDue(t *testing.T) {
	t.Parallel()
	display := NewDisplay(func(_ context.Context, in Input) (string, error) { return "T:" + in.Markdown, nil })
	display.streamInterval = 20 * time.Millisecond
	display.Start(t.Context())
	var live Surface
	changed := display.Changed()
	display.Markdown(&live, Input{Markdown: "a", IsStreaming: true})
	waitChanged(t, changed)
	changed = display.Changed()
	display.Markdown(&live, Input{Markdown: "ab", IsStreaming: true})
	waitChanged(t, changed)
	assert.Equal(t, "T:ab", display.Markdown(&live, Input{Markdown: "ab", IsStreaming: true}), "no further render is needed to pick up the snapshot")
}
