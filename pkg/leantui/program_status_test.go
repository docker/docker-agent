package leantui

import (
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/programstatus"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func statusModel(t *testing.T) *model {
	t.Helper()
	m := bareModel(24)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, session.New(session.WithID("root")))
	m.programStatus = &programstatus.Reporter{}
	return m
}

func TestProgramStatusTurn(t *testing.T) {
	t.Parallel()
	m := statusModel(t)
	m.handleEvent(t.Context(), runtime.StreamStarted("root", "root"))
	assert.Equal(t, "working", m.statusSession.Status().State)
	m.handleEvent(t.Context(), runtime.StreamStarted("child", "child"))
	m.handleEvent(t.Context(), runtime.StreamStopped("child", "child", "normal"))
	assert.Equal(t, "working", m.statusSession.Status().State)
	prompt := &runtime.ToolCallConfirmationEvent{}
	m.handleEvent(t.Context(), prompt)
	assert.Equal(t, "permission", m.statusSession.Status().Kind)
	m.resolveConfirm(runtime.ResumeApprove())
	assert.Equal(t, "working", m.statusSession.Status().State)
	m.handleEvent(t.Context(), runtime.StreamStopped("root", "root", "normal"))
	assert.Equal(t, "done", m.statusSession.Status().State)
	m.resetConversation()
	assert.Equal(t, "idle", m.statusSession.Status().State)
}

func TestProgramStatusFork(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"normal", "error", "canceled"} {
		m := statusModel(t)
		m.statusSession.Start("root")
		m.handleEvent(t.Context(), runtime.StreamStarted("fork", "worker"))
		assert.Equal(t, "working", m.statusSession.Status().State)
		m.handleEvent(t.Context(), runtime.StreamStopped("fork", "worker", reason))
		want := "done"
		switch reason {
		case "error":
			want = "error"
		case "canceled":
			want = "idle"
		}
		assert.Equal(t, want, m.statusSession.Status().State)
	}
}

func TestProgramStatusSkippedCompaction(t *testing.T) {
	t.Parallel()
	m := statusModel(t)
	m.statusSession.Start("root")
	m.busy = true
	m.handleEvent(t.Context(), runtime.SessionCompactionCompleted("root", runtime.CompactionOutcomeSkipped, "root"))
	assert.False(t, m.busy)
	assert.Equal(t, "done", m.statusSession.Status().State)
}

func TestProgramStatusStaleEvent(t *testing.T) {
	t.Parallel()
	m := statusModel(t)
	m.handleEvent(t.Context(), leanEvent{generation: 1, inner: runtime.StreamStarted("root", "root")})
	assert.Equal(t, "idle", m.statusSession.Status().State)
}

func TestProgramStatusProbeEscapeHandoff(t *testing.T) {
	t.Parallel()
	keys := make(chan ui.Key, 4)
	done := make(chan struct{})
	defer close(done)
	readKeys(strings.NewReader("\x1b[A"), keys, done)
	assert.Equal(t, ui.KeyUp, (<-keys).Typ)
	assert.Empty(t, keys)
}

func TestProgramStatusEarlyForkFailure(t *testing.T) {
	t.Parallel()
	m := statusModel(t)
	m.busy = true
	m.statusSession.Start("root")
	m.handleEvent(t.Context(), &runtime.ErrorEvent{SessionID: "fork", Error: "failed to get tools"})
	m.handleEvent(t.Context(), runtime.StreamStopped("fork", "worker", ""))
	m.handleEvent(t.Context(), runtime.Error("Skill failed"))
	assert.False(t, m.busy)
	assert.Equal(t, "error", m.statusSession.Status().State)
}

func TestProgramStatusInterruptLateNormalStop(t *testing.T) {
	t.Parallel()
	m := statusModel(t)
	m.handleEvent(t.Context(), runtime.StreamStarted("root", "root"))
	m.handleInterrupt()
	m.handleEvent(t.Context(), runtime.StreamStopped("root", "root", "normal"))
	assert.Equal(t, "idle", m.statusSession.Status().State)
}

func TestReadKeysEscapeTimeoutAndRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		keys := make(chan ui.Key, 8)
		done := make(chan struct{})
		defer close(done)
		go readKeys(reader, keys, done)
		_, err := writer.Write([]byte("\x1b["))
		require.NoError(t, err)
		synctest.Wait()
		_, err = writer.Write([]byte{3})
		require.NoError(t, err)
		assert.Equal(t, ui.KeyCtrlC, (<-keys).Typ)
		_, err = writer.Write([]byte("\x1b["))
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // Advance synctest time past the escape timeout.
		_, err = writer.Write([]byte("h"))
		require.NoError(t, err)
		assert.Equal(t, []rune{'h'}, (<-keys).Runes)
	})
}
