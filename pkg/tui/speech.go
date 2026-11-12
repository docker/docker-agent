package tui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type speechRun struct {
	cancel context.CancelFunc
	finish chan struct{}
	done   chan struct{}
	wake   chan struct{}
	tab    *tabModel
	editor editor.Editor
	err    error // Read only after done closes.

	mu      sync.Mutex
	pending strings.Builder
	ready   bool

	// Only the UI loop requests completion and decides whether to send the draft.
	stopping bool
	send     bool
}

type speechEvent struct{ run *speechRun }

func (r *speechRun) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (m *appModel) isTranscribing() bool {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	return m.speech != nil
}

func (m *appModel) handleStartSpeak() (tea.Model, tea.Cmd) {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	if m.speech != nil || m.speechCtx == nil || m.speechCtx.Err() != nil {
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.speechCtx)
	run := &speechRun{
		cancel: cancel,
		finish: make(chan struct{}),
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
		tab:    m.activeTab,
		editor: m.activeTab.editor,
	}
	m.speech = run
	go func() {
		defer close(run.done)
		defer cancel()
		run.err = m.transcriber.Run(ctx, run.finish, func(_ context.Context, delta string) {
			run.mu.Lock()
			run.pending.WriteString(delta)
			run.mu.Unlock()
			run.notify()
		}, func(context.Context) {
			run.mu.Lock()
			run.ready = true
			run.mu.Unlock()
			run.notify()
		})
	}()
	return m, waitForSpeech(run)
}

func waitForSpeech(run *speechRun) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-run.wake:
		case <-run.done:
		}
		return speechEvent{run: run}
	}
}

func (m *appModel) handleSpeechEvent(event speechEvent) (tea.Model, tea.Cmd) {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	if m.speech != event.run {
		return m, nil
	}
	run := event.run
	var done bool
	select {
	case <-run.done:
		done = true
	default:
	}
	// Notifications carry no text: even a notification in flight cannot lose a delta.
	run.mu.Lock()
	text, ready := run.pending.String(), run.ready
	run.pending.Reset()
	run.ready = false
	run.mu.Unlock()
	if text != "" {
		run.editor.InsertText(text)
	}

	if done {
		m.speech = nil
		var result tea.Cmd
		switch {
		case run.err != nil:
			result = notification.ErrorCmd(fmt.Sprintf("Transcription failed: %v", run.err))
		case run.send && m.activeTab == run.tab && m.activeTab.editor == run.editor:
			result = m.sendSpeechDraft(run)
		case run.stopping:
			result = notification.SuccessCmd("Stopped listening")
		}
		return m, tea.Batch(run.editor.SetRecording(false), result)
	}

	var started tea.Cmd
	if ready && !run.stopping {
		started = tea.Batch(run.editor.SetRecording(true), notification.InfoCmd("🎤 Listening... (ENTER to send or ESC to cancel)"))
	}
	return m, tea.Batch(started, waitForSpeech(run))
}

// stopSpeech discards a run whose editor or application is being torn down.
func (m *appModel) stopSpeech() *speechRun {
	return m.stopSpeechForTab(nil)
}

func (m *appModel) stopSpeechForTab(tab *tabModel) *speechRun {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	run := m.speech
	if run != nil && tab != nil && run.tab != tab {
		return nil
	}
	if run != nil {
		run.cancel()
		<-run.done
		m.speech = nil
		if run.err != nil {
			slog.Warn("Failed to stop transcription", "error", run.err)
		}
	}
	return run
}

func (m *appModel) handleStopSpeak() (tea.Model, tea.Cmd) {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	if run := m.speech; run != nil {
		run.stopping, run.send = true, false
		run.cancel()
	}
	// The existing subscription applies pending text and reports the final outcome.
	return m, nil
}

func (m *appModel) handleFinishSpeak() (tea.Model, tea.Cmd) {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	if run := m.speech; run != nil && !run.stopping {
		run.stopping = true
		run.send = m.activeTab == run.tab
		close(run.finish)
		return m, notification.InfoCmd("Finishing transcription… (ESC to cancel)")
	}
	return m, nil
}

func (m *appModel) sendSpeechDraft(run *speechRun) tea.Cmd {
	cmd := run.editor.SendContent()
	if cmd == nil {
		return nil
	}
	var tabID string
	for id, tab := range m.tabs {
		if tab == run.tab {
			tabID = id
			break
		}
	}
	return func() tea.Msg {
		return messages.RoutedMsg{
			SessionID: tabID,
			Inner:     cmd(),
			Valid:     func() bool { return m.tabs[tabID] == run.tab && run.tab.editor == run.editor },
		}
	}
}

func (m *appModel) cancelSpeechSend(tab *tabModel) {
	m.speechMu.Lock()
	defer m.speechMu.Unlock()
	if run := m.speech; run != nil && run.tab == tab {
		run.send = false
	}
}
