//go:build !windows

package tui

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestHandlePlanEditorClosed_NoInlineDraftRead proves Update never reads the
// draft on the event loop: the draft path is a FIFO with no writer, on which
// an inline read (a plain open without O_NONBLOCK) would block forever and
// hang this test. Update must return a command immediately; the command then
// opens the FIFO hang-safe, rejects it on the descriptor as non-regular, and
// leaves the path in place.
func TestHandlePlanEditorClosed_NoInlineDraftRead(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)

	fifo := filepath.Join(t.TempDir(), "draft.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this system: %v", err)
	}

	// A regression to an inline draft read would block this call forever
	// and fail the test by timeout.
	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: fifo})
	require.NotNil(t, cmd, "Update must defer the draft read to a command")

	result := cmd()
	writeResult, ok := result.(planWriteResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	require.Error(t, writeResult.readErr)
	assert.Contains(t, writeResult.readErr.Error(), "not a regular file")

	_, notifyCmd := m.Update(result)
	texts := notificationTexts(collectMsgs(notifyCmd))
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not a regular file")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "nothing may be written from an unreadable draft")
	_, err = os.Stat(fifo)
	require.NoError(t, err, "the draft path must be preserved on a read failure")
}

// TestShowPlanBrowser_FIFOPlanFileDoesNotHang proves the TUI's read timeout
// is not the only line of defense for the known FIFO trigger: a FIFO
// squatting on a stored plan file fails fast inside the real filesystem
// storage, so /plans completes promptly — browser opened, healthy plan
// listed, FIFO degraded to a warning — instead of the read command hanging
// (before the hang-safe open: forever, with the deadline unable to interrupt
// the blocked open).
func TestShowPlanBrowser_FIFOPlanFileDoesNotHang(t *testing.T) {
	t.Parallel()
	m, _ := newTestModel(t)
	sharedDir := t.TempDir()
	svc := plans.NewService(plan.NewFilesystemStorage(sharedDir))
	WithPlansService(svc)(m)
	sess := session.New()
	m.application = app.New(t.Context(), stubRuntime{}, sess)
	m.sessionState = service.NewSessionState(sess)

	mustCreatePlan(t, svc, "good", "content")
	if err := syscall.Mkfifo(filepath.Join(sharedDir, "wedged.json"), 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this system: %v", err)
	}

	_, cmd := m.Update(messages.ShowPlanBrowserMsg{})
	require.NotNil(t, cmd)

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	var result tea.Msg
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the /plans listing read blocked on a FIFO plan file; the storage open must not block")
	}

	loaded, ok := result.(planBrowserLoadedMsg)
	require.True(t, ok, "got %T", result)
	require.NoError(t, loaded.err, "the listing must complete for real, not through the read deadline")

	msgs := drainPlanFlow(t, m, func() tea.Msg { return result })
	openMsg, ok := firstOfType[dialog.OpenDialogMsg](msgs)
	require.True(t, ok, "the browser must open despite the FIFO plan file")
	openMsg.Model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	assert.Contains(t, openMsg.Model.View(), "good", "the healthy plan must be listed")

	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts, "the FIFO must surface as a warning")
	assert.Contains(t, texts[0], "wedged")
}
