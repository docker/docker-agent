package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker-agent/pkg/audio/transcribe"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// fakeTranscriber is a controllable implementation of the Transcriber
// interface used to exercise the speech-to-text handlers without touching
// audio hardware or network. It is intentionally lock-free: the TUI runs the
// transcriber on the single Bubble Tea event-loop goroutine, so concurrent
// access from production code is not a concern, and tests interact with it
// strictly after each handler call returns.
type fakeTranscriber struct {
	supported  bool
	startErr   error
	running    bool
	startCalls int
	stopCalls  int
}

func (f *fakeTranscriber) IsSupported() bool { return f.supported }
func (f *fakeTranscriber) IsRunning() bool   { return f.running }

func (f *fakeTranscriber) Start(_ context.Context, _ transcribe.TranscriptHandler) error {
	f.startCalls++
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}

func (f *fakeTranscriber) Stop() {
	f.stopCalls++
	f.running = false
}

// newSpeakTestModel builds an appModel wired with a fakeTranscriber so that the
// speech-to-text handlers can be tested in isolation. It leverages the same
// minimal scaffolding used by the exit tests.
func newSpeakTestModel(ft *fakeTranscriber) *appModel {
	page := &mockChatPage{}
	ed := &mockEditor{}

	return &appModel{
		chatPages:               map[string]chat.Page{"test": page},
		sessionStates:           map[string]*service.SessionState{},
		editors:                 map[string]editor.Editor{"test": ed},
		pendingRestores:         map[string]string{},
		pendingSidebarCollapsed: map[string]bool{},
		chatPage:                page,
		editor:                  ed,
		transcriber:             ft,
		dialogMgr:               dialog.New(),
	}
}

func TestHandleStartSpeak_NoOpIfAlreadyRunning(t *testing.T) {
	ft := &fakeTranscriber{running: true}
	m := newSpeakTestModel(ft)

	_, cmd := m.handleStartSpeak()
	if cmd != nil {
		t.Errorf("expected nil cmd when already running, got %T", cmd)
	}
	if ft.startCalls != 0 {
		t.Errorf("Start should not be called when already running; got %d calls", ft.startCalls)
	}
}

func TestHandleStartSpeak_ReturnsErrorNotificationOnStartFailure(t *testing.T) {
	ft := &fakeTranscriber{startErr: errors.New("boom")}
	m := newSpeakTestModel(ft)

	_, cmd := m.handleStartSpeak()
	if cmd == nil {
		t.Fatalf("expected an error notification cmd, got nil")
	}
	if ft.startCalls != 1 {
		t.Errorf("Start should be called exactly once; got %d", ft.startCalls)
	}
	if m.transcriptCh != nil {
		t.Error("transcriptCh should be cleared after a failed Start")
	}

	// The returned cmd should produce an error notification.ShowMsg.
	msg := cmd()
	show, ok := msg.(notification.ShowMsg)
	if !ok {
		t.Fatalf("expected notification.ShowMsg, got %T (%v)", msg, msg)
	}
	if show.Type != notification.TypeError {
		t.Errorf("expected notification of TypeError, got %v", show.Type)
	}
	if show.Text == "" {
		t.Error("error notification should carry a non-empty Text")
	}
}

func TestHandleStopSpeak_NoOpWhenNotRunning(t *testing.T) {
	ft := &fakeTranscriber{running: false}
	m := newSpeakTestModel(ft)

	_, cmd := m.handleStopSpeak()
	if cmd != nil {
		t.Errorf("expected nil cmd when not running, got %T", cmd)
	}
	if ft.stopCalls != 0 {
		t.Errorf("Stop should not be called when not running; got %d", ft.stopCalls)
	}
}

func TestHandleStopSpeak_StopsAndNotifies(t *testing.T) {
	ft := &fakeTranscriber{running: true}
	m := newSpeakTestModel(ft)
	// Pretend a previous start opened a channel.
	m.transcriptCh = make(chan string, 1)

	_, cmd := m.handleStopSpeak()
	if cmd == nil {
		t.Fatalf("expected a batch cmd, got nil")
	}
	if ft.stopCalls != 1 {
		t.Errorf("Stop should be called exactly once; got %d", ft.stopCalls)
	}
	if m.transcriptCh != nil {
		t.Error("transcriptCh should be cleared after Stop")
	}
}
