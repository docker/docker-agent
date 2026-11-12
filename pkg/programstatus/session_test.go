package programstatus

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"", "normal", "continue", "steered", "canceled", "error", "budget_exceeded", "hook_blocked"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			s := &Session{}
			assert.Equal(t, "idle", s.Status().State)
			s.Apply("root", runtime.StreamStarted("root", "root"))
			s.Apply("root", runtime.StreamStarted("child", "child"))
			s.Apply("root", runtime.StreamStopped("child", "child", "error"))
			assert.Equal(t, "working", s.Status().State)
			s.Apply("root", runtime.StreamStopped("root", "root", reason))
			want := "done"
			switch reason {
			case "canceled":
				want = "idle"
			case "error", "budget_exceeded", "hook_blocked":
				want = "error"
			}
			assert.Equal(t, want, s.Status().State)
			s.Acknowledge()
			assert.Equal(t, "idle", s.Status().State)
		})
	}
}

func TestSessionPrompts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		event tea.Msg
		kind  string
	}{
		{&runtime.ToolCallConfirmationEvent{ToolCall: tools.ToolCall{ID: "tool"}}, "permission"},
		{&runtime.MaxIterationsReachedEvent{}, "question"},
		{&runtime.ElicitationRequestEvent{}, "question"},
		{&runtime.ElicitationRequestEvent{Meta: map[string]any{"docker-agent/type": "oauth_flow"}}, "auth"},
	} {
		s := &Session{}
		s.Start("root")
		s.Apply("root", tc.event)
		s.Apply("root", runtime.StreamStopped("child", "child", "normal"))
		s.Acknowledge()
		assert.Equal(t, "blocked", s.Status().State)
		assert.Equal(t, tc.kind, s.Status().Kind)
		assert.NotEmpty(t, s.Status().Msg)
		s.Resolve(tc.event)
		assert.Equal(t, "working", s.Status().State)
	}
}

func TestSessionDetachedPrompts(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.Start("root")
	foreground := &runtime.ElicitationRequestEvent{SessionID: "root"}
	detached := &runtime.ElicitationRequestEvent{SessionID: "job"}
	s.Apply("root", foreground)
	s.Apply("root", detached)
	s.Apply("root", runtime.StreamStopped("root", "root", "normal"))
	assert.Equal(t, "blocked", s.Status().State)
	s.Resolve(foreground)
	assert.Equal(t, "blocked", s.Status().State)
	s.Resolve(detached)
	assert.Equal(t, "done", s.Status().State)
}

func TestSessionErrorAndRecovery(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.Start("root")
	s.Apply("root", &runtime.ErrorEvent{SessionID: "root"})
	assert.Equal(t, "working", s.Status().State, "don't mark an active run finished")
	s.Apply("root", runtime.StreamStopped("root", "root", "normal"))
	assert.Equal(t, "error", s.Status().State)
	s.Apply("root", runtime.SessionRecovered("root"))
	assert.Equal(t, "idle", s.Status().State)
	s.Apply("root", runtime.StreamStarted("root", "root"))
	s.Apply("root", &runtime.ErrorEvent{SessionID: "child"})
	s.Apply("root", runtime.StreamStopped("root", "root", "normal"))
	assert.Equal(t, "done", s.Status().State)
}

func TestSessionCompaction(t *testing.T) {
	t.Parallel()
	for _, nested := range []bool{false, true} {
		s := &Session{}
		s.Start("root")
		if nested {
			s.Apply("root", runtime.StreamStarted("root", "root"))
		}
		s.Apply("root", &runtime.SessionCompactionEvent{SessionID: "root", Status: "started"})
		assert.Equal(t, "working", s.Status().State)
		s.Apply("root", &runtime.SessionCompactionEvent{SessionID: "root", Status: "completed"})
		want := "done"
		if nested {
			want = "working"
		}
		assert.Equal(t, want, s.Status().State)
	}
}

func TestSessionStandaloneFork(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"normal", "error", "canceled"} {
		s := &Session{}
		s.Apply("root", runtime.StreamStarted("fork", "worker"))
		assert.Equal(t, "working", s.Status().State)
		s.Apply("root", runtime.StreamStarted("nested", "child"))
		s.Apply("root", runtime.StreamStopped("nested", "child", "normal"))
		assert.Equal(t, "working", s.Status().State)
		s.Apply("root", runtime.StreamStopped("fork", "worker", reason))
		want := "done"
		switch reason {
		case "error":
			want = "error"
		case "canceled":
			want = "idle"
		}
		assert.Equal(t, want, s.Status().State)
	}
}

func TestSessionCompactionWithoutStart(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.Start("root")
	s.Apply("root", &runtime.SessionCompactionEvent{SessionID: "root", Status: "completed", Outcome: runtime.CompactionOutcomeSkipped})
	assert.Equal(t, "done", s.Status().State)
}

func TestSessionCancelsForkPrompts(t *testing.T) {
	t.Parallel()
	for _, direct := range []bool{false, true} {
		s := &Session{}
		s.Apply("root", runtime.StreamStarted("fork", "worker"))
		s.Apply("root", &runtime.ElicitationRequestEvent{SessionID: "fork"})
		assert.Equal(t, "blocked", s.Status().State)
		if direct {
			s.Cancel("root")
		} else {
			s.Apply("root", runtime.StreamStopped("fork", "worker", "canceled"))
		}
		assert.Equal(t, "idle", s.Status().State)
	}
}

func TestSessionCancellationRetiresNestedPrompts(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.Apply("root", runtime.StreamStarted("root", "root"))
	s.Apply("root", runtime.StreamStarted("child", "child"))
	s.Apply("root", &runtime.ElicitationRequestEvent{SessionID: "child"})
	s.Apply("root", runtime.StreamStopped("child", "child", "canceled"))
	s.Apply("root", runtime.StreamStopped("root", "root", "canceled"))
	assert.Equal(t, "idle", s.Status().State)
}

func TestSessionCanceledLateCompletion(t *testing.T) {
	t.Parallel()
	for _, compact := range []bool{false, true} {
		s := &Session{}
		s.Start("root")
		s.Cancel("root")
		if compact {
			s.Apply("root", runtime.SessionCompactionCompleted("root", runtime.CompactionOutcomeApplied, "root"))
		} else {
			s.Apply("root", runtime.StreamStopped("root", "root", "normal"))
		}
		assert.Equal(t, "idle", s.Status().State)
		s.Start("root")
		s.Apply("root", runtime.StreamStopped("root", "root", "normal"))
		assert.Equal(t, "done", s.Status().State)
	}
}

func TestSessionEarlyChildPromptAndFailure(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.Start("root")
	s.Apply("root", &runtime.ElicitationRequestEvent{SessionID: "child", ElicitationID: "oauth"})
	s.Apply("root", runtime.StreamStopped("child", "child", "canceled"))
	assert.Equal(t, "working", s.Status().State)
	s.Apply("root", runtime.StreamStopped("root", "root", "canceled"))
	assert.Equal(t, "idle", s.Status().State)

	s.Start("root")
	s.Apply("root", &runtime.ErrorEvent{SessionID: "fork", Error: "tool startup failed"})
	s.Finish("root", "")
	assert.Equal(t, "error", s.Status().State)
}

func TestSessionClosedDetachedPrompt(t *testing.T) {
	t.Parallel()
	s := &Session{}
	first := &runtime.ElicitationRequestEvent{SessionID: "job", ElicitationID: "first"}
	second := &runtime.ElicitationRequestEvent{SessionID: "job", ElicitationID: "second"}
	s.Apply("root", first)
	s.Apply("root", second)
	s.Apply("root", &runtime.ElicitationClosedEvent{ElicitationID: "first"})
	assert.Equal(t, "blocked", s.Status().State)
	s.Apply("root", &runtime.ElicitationClosedEvent{ElicitationID: "second"})
	assert.Equal(t, "idle", s.Status().State)
}
