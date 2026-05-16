package session

import (
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
)

// setNowForTest installs a fixed clock for the duration of t and restores the
// previous clock on cleanup. Tests use this to assert on CreatedAt fields
// without flakiness.
//
// The clock is a package-level variable, so tests using this helper MUST NOT
// call t.Parallel(): two parallel tests would race on nowFn.
func setNowForTest(t *testing.T, fixed time.Time) {
	t.Helper()
	prev := nowFn
	nowFn = func() time.Time { return fixed }
	t.Cleanup(func() { nowFn = prev })
}

// setIDForTest installs a deterministic ID generator for the duration of t and
// restores the previous generator on cleanup. The supplied IDs are returned
// in order; running out triggers t.Fatalf.
//
// Like setNowForTest, this helper mutates a package-level variable and is not
// safe to use with t.Parallel(). Additionally, the installed generator calls
// t.Fatalf if exhausted, which is only valid on the test goroutine — do not
// use this helper if production code under test may call New() from a
// background goroutine.
func setIDForTest(t *testing.T, ids ...string) {
	t.Helper()
	prev := newIDFn
	i := 0
	newIDFn = func() string {
		if i >= len(ids) {
			t.Fatalf("setIDForTest: ran out of IDs after %d calls", i)
		}
		id := ids[i]
		i++
		return id
	}
	t.Cleanup(func() { newIDFn = prev })
}

func TestNew_UsesInjectedClockAndID(t *testing.T) {
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	setNowForTest(t, fixed)
	setIDForTest(t, "test-session-1")

	s := New()

	if s.ID != "test-session-1" {
		t.Errorf("ID = %q, want %q", s.ID, "test-session-1")
	}
	if !s.CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want %v", s.CreatedAt, fixed)
	}
	if !s.SendUserMessage {
		t.Error("SendUserMessage should default to true")
	}
}

func TestNew_WithIDOverridesGenerator(t *testing.T) {
	setIDForTest(t, "should-not-be-used")

	s := New(WithID("explicit-id"))

	if s.ID != "explicit-id" {
		t.Errorf("ID = %q, want %q", s.ID, "explicit-id")
	}
}

func TestUserMessage_UsesInjectedClock(t *testing.T) {
	fixed := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	setNowForTest(t, fixed)

	msg := UserMessage("hello")

	if msg.Message.Role != chat.MessageRoleUser {
		t.Errorf("Role = %v, want user", msg.Message.Role)
	}
	if msg.Message.CreatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", msg.Message.CreatedAt, fixed.Format(time.RFC3339))
	}
	if msg.Message.Content != "hello" {
		t.Errorf("Content = %q, want %q", msg.Message.Content, "hello")
	}
}

func TestSystemMessage_UsesInjectedClock(t *testing.T) {
	fixed := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	setNowForTest(t, fixed)

	msg := SystemMessage("you are a helpful assistant")

	if msg.Message.Role != chat.MessageRoleSystem {
		t.Errorf("Role = %v, want system", msg.Message.Role)
	}
	if msg.Message.CreatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", msg.Message.CreatedAt, fixed.Format(time.RFC3339))
	}
}

func TestImplicitUserMessage_IsImplicitAndUsesClock(t *testing.T) {
	fixed := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	setNowForTest(t, fixed)

	msg := ImplicitUserMessage("delegated task")

	if !msg.Implicit {
		t.Error("Implicit = false, want true")
	}
	if msg.Message.CreatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", msg.Message.CreatedAt, fixed.Format(time.RFC3339))
	}
}

func TestDuration_DeterministicWithInjectedClock(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	s := New()

	setNowForTest(t, t0)
	s.AddMessage(UserMessage("first"))

	setNowForTest(t, t0.Add(5*time.Second))
	s.AddMessage(UserMessage("second"))

	got := s.Duration()
	want := 5 * time.Second
	if got != want {
		t.Errorf("Duration() = %v, want %v", got, want)
	}
}
