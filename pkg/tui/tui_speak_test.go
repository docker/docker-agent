package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/audio/transcribe"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type fakeTranscriber struct {
	supported bool
	err       error
	calls     atomic.Int32
	cleaned   atomic.Bool
	handler   func(context.Context, <-chan struct{}, transcribe.TranscriptHandler, func(context.Context)) error
}

func (f *fakeTranscriber) IsSupported() bool { return f.supported }
func (f *fakeTranscriber) Run(ctx context.Context, finish <-chan struct{}, handler transcribe.TranscriptHandler, ready func(context.Context)) error {
	f.calls.Add(1)
	defer f.cleaned.Store(true)
	if f.handler != nil {
		return f.handler(ctx, finish, handler, ready)
	}
	if f.err != nil {
		return f.err
	}
	ready(ctx)
	select {
	case <-ctx.Done():
	case <-finish:
	}
	return nil
}

func newSpeakTestModel(t *testing.T, ft *fakeTranscriber) *appModel {
	t.Helper()
	m, _ := newTestModel(t)
	m.transcriber = ft
	m.speechCtx, m.cancelSpeech = context.WithCancel(t.Context())
	t.Cleanup(m.cleanupManagedResources)
	return m
}

func nextSpeechEvent(t *testing.T, cmd tea.Cmd) speechEvent {
	t.Helper()
	require.NotNil(t, cmd)
	event, ok := cmd().(speechEvent)
	require.True(t, ok)
	return event
}

func waitSpeechDone(t *testing.T, run *speechRun) {
	t.Helper()
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("transcription did not finish")
	}
}

func requireSpeechError(t *testing.T, cmd tea.Cmd, text string) {
	t.Helper()
	msgs := collectMsgs(cmd)
	for _, msg := range msgs {
		if show, ok := msg.(notification.ShowMsg); ok {
			require.Equal(t, notification.TypeError, show.Type)
			require.Contains(t, show.Text, text)
			return
		}
	}
	t.Fatal("missing error notification")
}

func TestHandleStartSpeak_NoOpIfAlreadyRunning(t *testing.T) {
	t.Parallel()
	ft := &fakeTranscriber{}
	m := newSpeakTestModel(t, ft)
	_, cmd := m.handleStartSpeak()
	nextSpeechEvent(t, cmd)
	_, cmd = m.handleStartSpeak()
	assert.Nil(t, cmd)
	assert.EqualValues(t, 1, ft.calls.Load())
	m.stopSpeech()
	assert.True(t, ft.cleaned.Load())
}

func TestSpeech_StopPreservesStartupError(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{err: errors.New("missing API key")})
	_, cmd := m.handleStartSpeak()
	run := m.speech
	waitSpeechDone(t, run)
	terminal := nextSpeechEvent(t, cmd)
	m.handleStopSpeak()
	_, cmd = m.handleSpeechEvent(terminal)
	assert.False(t, m.isTranscribing())
	requireSpeechError(t, cmd, "missing API key")
}

func TestHandleStopSpeak_NoOpWhenNotRunning(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{})
	_, cmd := m.handleStopSpeak()
	assert.Nil(t, cmd)
}

func TestSpeech_StopRetainsInflightText(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, _ <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		handler(ctx, " final words")
		<-ctx.Done()
		return nil
	}})
	ed := &speechEditor{text: "first words"}
	m.activeTab.editor = ed
	_, cmd := m.handleStartSpeak()
	run := m.speech
	inflight := nextSpeechEvent(t, cmd)
	m.handleStopSpeak()
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(inflight)
	assert.Equal(t, "first words final words", ed.text)
	assert.False(t, m.isTranscribing())
	_, cmd = m.handleSpeechEvent(inflight)
	assert.Nil(t, cmd)
	assert.Equal(t, "first words final words", ed.text)
}

func TestSpeech_IgnoresDiscardedRun(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{})
	_, _ = m.handleStartSpeak()
	previous := m.stopSpeech()
	_, _ = m.handleStartSpeak()
	_, cmd := m.handleSpeechEvent(speechEvent{run: previous})
	assert.Nil(t, cmd)
	assert.True(t, m.isTranscribing())
}

func TestSpeech_ShutdownJoinsCapture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		ft := &fakeTranscriber{handler: func(ctx context.Context, _ <-chan struct{}, _ transcribe.TranscriptHandler, ready func(context.Context)) error {
			ready(ctx)
			<-ctx.Done()
			<-release
			return nil
		}}
		m := newSpeakTestModel(t, ft)
		_, cmd := m.handleStartSpeak()
		nextSpeechEvent(t, cmd)
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.cleanupManagedResources()
		}()
		synctest.Wait()
		assert.False(t, ft.cleaned.Load())
		select {
		case <-done:
			t.Fatal("shutdown returned before capture cleanup")
		default:
		}
		close(release)
		<-done
		assert.True(t, ft.cleaned.Load())
		_, cmd = m.handleStartSpeak()
		assert.Nil(t, cmd)
	})
}

func TestSpeech_StopDoesNotBlockUI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, _ <-chan struct{}, _ transcribe.TranscriptHandler, ready func(context.Context)) error {
			ready(ctx)
			<-ctx.Done()
			<-release
			return nil
		}})
		_, cmd := m.handleStartSpeak()
		run := m.speech
		nextSpeechEvent(t, cmd)
		m.handleStopSpeak()
		synctest.Wait()
		assert.True(t, m.isTranscribing(), "keep the run until cleanup completes")
		_, cmd = m.handleStartSpeak()
		assert.Nil(t, cmd, "do not overlap capture during cleanup")
		close(release)
		waitSpeechDone(t, run)
		_, _ = m.handleSpeechEvent(speechEvent{run: run})
		assert.False(t, m.isTranscribing())
	})
}

func TestSpeech_ParentCancellation(t *testing.T) {
	t.Parallel()
	ft := &fakeTranscriber{}
	m := newSpeakTestModel(t, ft)
	_, _ = m.handleStartSpeak()
	run := m.speech
	m.cancelSpeech()
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(speechEvent{run: run})
	assert.False(t, m.isTranscribing())
	assert.True(t, ft.cleaned.Load())
}

type speechEditor struct {
	mockEditor

	text      string
	recording bool
	sent      []string
}

func (e *speechEditor) InsertText(text string) { e.text += text }
func (e *speechEditor) SetRecording(recording bool) tea.Cmd {
	e.recording = recording
	return nil
}

func (e *speechEditor) SendContent() tea.Cmd {
	e.sent = append(e.sent, e.text)
	return nil
}

func TestSpeech_FinishSendsAllTextOnce(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, finish <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		handler(ctx, "first")
		<-finish
		handler(ctx, " last")
		return nil
	}})
	ed := &speechEditor{}
	m.activeTab.editor = ed
	_, cmd := m.handleStartSpeak()
	run := m.speech
	inflight := nextSpeechEvent(t, cmd)
	m.handleFinishSpeak()
	m.handleFinishSpeak()
	assert.Empty(t, ed.sent)
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(inflight)
	assert.Equal(t, []string{"first last"}, ed.sent)
	_, _ = m.handleSpeechEvent(inflight)
	assert.Len(t, ed.sent, 1)
}

func TestSpeech_FinishFailureKeepsDraft(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, finish <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		handler(ctx, "keep this")
		<-finish
		return errors.New("final transcription timed out")
	}})
	ed := &speechEditor{}
	m.activeTab.editor = ed
	_, _ = m.handleStartSpeak()
	run := m.speech
	m.handleFinishSpeak()
	waitSpeechDone(t, run)
	_, cmd := m.handleSpeechEvent(speechEvent{run: run})
	requireSpeechError(t, cmd, "timed out")
	assert.Equal(t, "keep this", ed.text)
	assert.Empty(t, ed.sent)
}

func TestSpeech_SwitchTabWhileFinishingKeepsOriginDraft(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, finish <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		<-finish
		handler(ctx, "origin")
		return nil
	}})
	origin := &speechEditor{}
	m.activeTab.editor = origin
	_, _ = m.handleStartSpeak()
	run := m.speech
	m.handleFinishSpeak()
	other := &speechEditor{}
	m.activeTab = &tabModel{editor: other}
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(speechEvent{run: run})
	assert.Equal(t, "origin", origin.text)
	assert.Empty(t, origin.sent)
	assert.Empty(t, other.text)
	assert.Empty(t, other.sent)
}

func TestSpeech_EscapeCancelsPendingSend(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, finish <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		<-finish
		handler(ctx, "draft")
		<-ctx.Done()
		return nil
	}})
	ed := &speechEditor{}
	m.activeTab.editor = ed
	_, _ = m.handleStartSpeak()
	run := m.speech
	m.handleFinishSpeak()
	m.handleStopSpeak()
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(speechEvent{run: run})
	assert.Equal(t, "draft", ed.text)
	assert.Empty(t, ed.sent)
}

func TestSpeech_ExitSafetyNetCoversBlockedCleanup(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ft := &fakeTranscriber{handler: func(ctx context.Context, _ <-chan struct{}, _ transcribe.TranscriptHandler, ready func(context.Context)) error {
		ready(ctx)
		<-ctx.Done()
		<-release
		return nil
	}}
	m := newSpeakTestModel(t, ft)
	t.Cleanup(func() { close(release) })
	_, cmd := m.handleStartSpeak()
	nextSpeechEvent(t, cmd)
	m.program = tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard))
	defer m.program.Kill()
	exited := make(chan struct{}, 1)
	m.exitFunc = func(int) { exited <- struct{}{} }
	m.shutdownTimeout = 10 * time.Millisecond
	m.cleanupAll()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("speech cleanup blocked the exit safety net")
	}
}

func TestSpeech_StopPreservesCleanupError(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, _ <-chan struct{}, _ transcribe.TranscriptHandler, ready func(context.Context)) error {
		ready(ctx)
		<-ctx.Done()
		return errors.New("dispose failed")
	}})
	_, cmd := m.handleStartSpeak()
	run := m.speech
	nextSpeechEvent(t, cmd)
	m.handleStopSpeak()
	waitSpeechDone(t, run)
	_, cmd = m.handleSpeechEvent(speechEvent{run: run})
	requireSpeechError(t, cmd, "dispose failed")
}

type routedSpeechEditor struct{ speechEditor }

func (e *routedSpeechEditor) SendContent() tea.Cmd {
	content := e.text
	return func() tea.Msg { return messages.SendMsg{Content: content} }
}

func TestSpeech_SendStaysScopedUntilConsumption(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{})
	ed := &routedSpeechEditor{}
	ed.text = "dictation"
	m.activeTab.editor = ed
	run := &speechRun{tab: m.activeTab, editor: ed}
	cmd := m.sendSpeechDraft(run)
	origin := m.activeTab
	m.activeTab = &tabModel{editor: &speechEditor{}}
	routed, ok := cmd().(messages.RoutedMsg)
	require.True(t, ok)
	assert.Equal(t, "test", routed.SessionID)
	assert.Equal(t, messages.SendMsg{Content: "dictation"}, routed.Inner)
	assert.True(t, routed.Valid())
	origin.editor = &speechEditor{}
	assert.False(t, routed.Valid(), "a replacement editor must invalidate the queued send")
}

func TestSpeech_CoalescedNotificationsLoseNoText(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, _ <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		for range 100 {
			handler(ctx, "x")
		}
		return nil
	}})
	ed := &speechEditor{}
	m.activeTab.editor = ed
	_, cmd := m.handleStartSpeak()
	run := m.speech
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(nextSpeechEvent(t, cmd))
	assert.Equal(t, strings.Repeat("x", 100), ed.text)
	assert.False(t, m.isTranscribing())
}

func TestSpeech_LeavingAndReturningRevokesSend(t *testing.T) {
	t.Parallel()
	m := newSpeakTestModel(t, &fakeTranscriber{handler: func(ctx context.Context, finish <-chan struct{}, handler transcribe.TranscriptHandler, _ func(context.Context)) error {
		<-finish
		handler(ctx, "keep draft")
		return nil
	}})
	ed := &speechEditor{}
	m.activeTab.editor = ed
	_, _ = m.handleStartSpeak()
	run := m.speech
	m.handleFinishSpeak()
	m.cancelSpeechSend(m.activeTab)
	waitSpeechDone(t, run)
	_, _ = m.handleSpeechEvent(speechEvent{run: run})
	assert.Equal(t, "keep draft", ed.text)
	assert.Empty(t, ed.sent)
}

func TestNew_KeepsInjectedTranscriber(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ft := &fakeTranscriber{supported: true}

	a := app.New(t.Context(), stubRuntime{}, &session.Session{ID: "speak", Title: "speak"})
	m := New(t.Context(), nil, a, "", func() {}, WithTranscriber(ft)).(*appModel)
	t.Cleanup(m.cleanupManagedResources)

	assert.Same(t, ft, m.transcriber)
}
