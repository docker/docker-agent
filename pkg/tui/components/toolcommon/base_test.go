package toolcommon

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestBaseRunningTimeFooter(t *testing.T) {
	t.Parallel()

	started := time.Now().Add(-5100 * time.Millisecond)
	msg := &types.Message{ToolStatus: types.ToolStatusRunning, StartedAt: &started}
	render := func(*types.Message, spinner.Spinner, service.SessionStateReader, int, int) string {
		return "tool header\ntool output"
	}
	base := NewBaseWithCollapsed(animation.NewSnapshotRuntime(0), msg, nil, render, render)

	for _, view := range []func() string{base.View, base.ExpandedView, base.CollapsedView} {
		assert.Equal(t, "tool header\ntool output\n  Running for 5s", ansi.Strip(view()))
	}

	started = time.Now().Add(-90100 * time.Millisecond)
	assert.Contains(t, ansi.Strip(base.View()), "Running for 1m30s")

	msg.SetToolStatus(types.ToolStatusCompleted)
	assert.Equal(t, "tool header\ntool output\n  Completed in 1m30s", ansi.Strip(base.View()))
	msg.SetToolStatus(types.ToolStatusError)
	assert.Contains(t, ansi.Strip(base.CollapsedView()), "Failed after 1m30s")
	msg.ToolStatus = types.ToolStatusRunning
	msg.HideToolProgress = true
	assert.Equal(t, "tool header\ntool output", base.View())
}

func TestHiddenToolProgressUsesNeutralIcon(t *testing.T) {
	t.Parallel()

	for _, status := range []types.ToolStatus{types.ToolStatusPending, types.ToolStatusRunning} {
		msg := &types.Message{ToolStatus: status, HideToolProgress: true}
		icon := Icon(msg, spinner.New(animation.NewSnapshotRuntime(0), spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle))
		assert.Equal(t, styles.ToolMessageStyle.MarginLeft(2).Render("●"), icon)
		assert.Equal(t, "  ●", ansi.Strip(icon))
	}
}

func TestWithRunningTime(t *testing.T) {
	t.Parallel()

	started := time.Now()
	for _, status := range []types.ToolStatus{
		types.ToolStatusPending, types.ToolStatusConfirmation, types.ToolStatusCompleted, types.ToolStatusError,
	} {
		msg := &types.Message{ToolStatus: status, StartedAt: &started}
		assert.Equal(t, "tool", WithRunningTime("tool", msg, 80))
	}
	msg := &types.Message{ToolStatus: types.ToolStatusRunning}
	assert.Equal(t, "tool", WithRunningTime("tool", msg, 80))
	msg.StartedAt = &started
	assert.Empty(t, WithRunningTime("", msg, 80))
	assert.Contains(t, ansi.Strip(WithRunningTime("tool", msg, 80)), "Running for 0s")

	future := time.Now().Add(time.Minute)
	msg.StartedAt = &future
	assert.Contains(t, ansi.Strip(WithRunningTime("tool", msg, 80)), "Running for 0s")
	for _, width := range []int{1, 5, 10} {
		for line := range strings.SplitSeq(WithRunningTime("x", msg, width), "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), width)
		}
	}
}
