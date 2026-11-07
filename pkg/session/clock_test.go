package session

import (
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
)

// fixedClock returns a clock func that always reports t. Used to make
// CreatedAt assertions deterministic without touching any global state, so
// these tests are safe to run in parallel.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// stubIDs returns an ID generator that yields the supplied IDs in order and
// fails the test if exhausted.
func stubIDs(t *testing.T, ids ...string) func() string {
	t.Helper()
	i := 0
	return func() string {
		if i >= len(ids) {
			t.Fatalf("stubIDs: ran out of IDs after %d calls", i)
		}
		id := ids[i]
		i++
		return id
	}
}

func TestNew_UsesInjectedClockAndID(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	s := New(withClock(fixedClock(fixed)), withIDGen(stubIDs(t, "test-session-1")))

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
	t.Parallel()
	s := New(withIDGen(stubIDs(t)), WithID("explicit-id"))

	if s.ID != "explicit-id" {
		t.Errorf("ID = %q, want %q", s.ID, "explicit-id")
	}
}

func TestUserMessageAt_UsesProvidedClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	msg := UserMessageAt(fixed, "hello")

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

func TestSystemMessageAt_UsesProvidedClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	msg := SystemMessageAt(fixed, "you are a helpful assistant")

	if msg.Message.Role != chat.MessageRoleSystem {
		t.Errorf("Role = %v, want system", msg.Message.Role)
	}
	if msg.Message.CreatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", msg.Message.CreatedAt, fixed.Format(time.RFC3339))
	}
}

func TestImplicitUserMessageAt_IsImplicitAndUsesClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	msg := ImplicitUserMessageAt(fixed, "delegated task")

	if !msg.Implicit {
		t.Error("Implicit = false, want true")
	}
	if msg.Message.CreatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", msg.Message.CreatedAt, fixed.Format(time.RFC3339))
	}
}

func TestWithMessageOptions_UseInjectedClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	want := fixed.Format(time.RFC3339)

	s := New(
		withClock(fixedClock(fixed)),
		WithUserMessage("user"),
		WithSystemMessage("system"),
		WithImplicitUserMessage("implicit"),
	)

	if len(s.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(s.Messages))
	}
	for i, item := range s.Messages {
		if item.Message.Message.CreatedAt != want {
			t.Errorf("Messages[%d].CreatedAt = %q, want %q", i, item.Message.Message.CreatedAt, want)
		}
	}
}

func TestDuration_DeterministicWithExplicitTimestamps(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	s := New()

	s.AddMessage(UserMessageAt(t0, "first"))
	s.AddMessage(UserMessageAt(t0.Add(5*time.Second), "second"))

	got := s.Duration()
	want := 5 * time.Second
	if got != want {
		t.Errorf("Duration() = %v, want %v", got, want)
	}
}

// withClock injects the time source used for the session's CreatedAt, for
// timestamping messages it generates (summaries, compaction input), and for
// messages added through WithUserMessage/WithSystemMessage/
// WithImplicitUserMessage. Because those message options read the clock when
// they run, withClock must precede them in the option list (the natural
// ordering). Primarily for tests that need a deterministic clock; production
// code leaves it unset and falls back to time.Now.
func withClock(now func() time.Time) Opt {
	return func(s *Session) {
		s.clock = now
	}
}

// withIDGen injects the generator used to mint the session ID when no explicit
// ID is provided. Primarily for tests that need deterministic IDs; production
// code leaves it unset and falls back to a random UUID.
func withIDGen(gen func() string) Opt {
	return func(s *Session) {
		s.idgen = gen
	}
}
