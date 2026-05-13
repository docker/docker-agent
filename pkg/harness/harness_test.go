package harness_test

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

// --- Registry tests ---

type stubAdapter struct{ name string }

func (s *stubAdapter) Name() string                              { return s.name }
func (s *stubAdapter) Capabilities() harness.AdapterCapabilities { return harness.AdapterCapabilities{} }
func (s *stubAdapter) Run(_ context.Context, _ harness.SubSessionRequest) {}

func TestRegistryLookupMissing(t *testing.T) {
	_, err := harness.Lookup("nonexistent-harness-xyz")
	if err == nil {
		t.Fatal("expected error for unknown harness type, got nil")
	}
}

// --- Token ownership tests ---

func TestAcquireReleaseToken(t *testing.T) {
	token := "test-token-" + t.Name()

	// First acquire succeeds.
	if err := harness.AcquireToken(token); err != nil {
		t.Fatalf("first AcquireToken: %v", err)
	}

	// Second acquire fails.
	if err := harness.AcquireToken(token); err == nil {
		t.Fatal("second AcquireToken should fail, got nil")
	}

	// After release, acquire succeeds again.
	harness.ReleaseToken(token)
	if err := harness.AcquireToken(token); err != nil {
		t.Fatalf("AcquireToken after release: %v", err)
	}
	harness.ReleaseToken(token)
}

func TestAcquireEmptyToken(t *testing.T) {
	// Empty token is always allowed (no-op).
	if err := harness.AcquireToken(""); err != nil {
		t.Fatalf("AcquireToken empty: %v", err)
	}
	if err := harness.AcquireToken(""); err != nil {
		t.Fatalf("second AcquireToken empty: %v", err)
	}
	harness.ReleaseToken("")
}

// --- Event type tests ---

func TestEventTypes(t *testing.T) {
	now := time.Now()

	events := []harness.Event{
		harness.RunStart{RunID: "r1", HarnessRunID: "h1", At: now},
		harness.TextStart{MessageID: "m1", Role: "assistant", At: now},
		harness.TextDelta{MessageID: "m1", Delta: "hello", At: now},
		harness.TextEnd{MessageID: "m1", At: now},
		harness.ReasoningStart{MessageID: "m2", At: now},
		harness.ReasoningDelta{MessageID: "m2", Delta: "thinking...", At: now},
		harness.ReasoningEnd{MessageID: "m2", At: now},
		harness.ToolCallStart{ToolCallID: "tc1", ToolName: "Bash", At: now},
		harness.ToolCallArgsDelta{ToolCallID: "tc1", Delta: `{"cmd":"ls"}`, At: now},
		harness.ToolCallEnd{ToolCallID: "tc1", At: now},
		harness.ToolCallResult{ToolCallID: "tc1", ToolName: "Bash", Result: "file.txt", At: now},
		harness.PermissionPending{RequestID: "p1", ToolCallID: "tc1", At: now},
		harness.PermissionResolved{RequestID: "p1", Allowed: true, Source: "user", At: now},
		harness.Heartbeat{At: now},
		harness.RunEnd{RunID: "r1", StopReason: "success", At: now},
		harness.RunError{RunID: "r1", Code: harness.ErrCodeHarnessCrashed, Message: "oops", At: now},
	}

	for _, e := range events {
		if e.EventTime().IsZero() {
			t.Errorf("EventTime() is zero for %T", e)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	codes := []harness.ErrorCode{
		harness.ErrCodeContextExhausted,
		harness.ErrCodeRateLimited,
		harness.ErrCodeAuthFailed,
		harness.ErrCodeHarnessCrashed,
		harness.ErrCodeHarnessTimeout,
		harness.ErrCodeUserCanceled,
		harness.ErrCodeCapabilityMismatch,
		harness.ErrCodeUnknown,
	}
	for _, c := range codes {
		if c == "" {
			t.Errorf("empty error code in list")
		}
	}
}

func TestProtocolClasses(t *testing.T) {
	if harness.ProtocolStream == "" {
		t.Error("ProtocolStream is empty")
	}
	if harness.ProtocolACP == "" {
		t.Error("ProtocolACP is empty")
	}
	if harness.ProtocolStream == harness.ProtocolACP {
		t.Error("ProtocolStream == ProtocolACP")
	}
}
