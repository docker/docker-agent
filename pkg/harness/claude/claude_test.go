package claude

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

// collectSink collects all emitted events for test assertions.
type collectSink struct {
	events []harness.Event
}

func (c *collectSink) Emit(e harness.Event) {
	c.events = append(c.events, e)
}

func (c *collectSink) ofType(t string) []harness.Event {
	var out []harness.Event
	for _, e := range c.events {
		switch e.(type) {
		case harness.RunStart:
			if t == "RunStart" {
				out = append(out, e)
			}
		case harness.TextStart:
			if t == "TextStart" {
				out = append(out, e)
			}
		case harness.TextDelta:
			if t == "TextDelta" {
				out = append(out, e)
			}
		case harness.TextEnd:
			if t == "TextEnd" {
				out = append(out, e)
			}
		case harness.ToolCallStart:
			if t == "ToolCallStart" {
				out = append(out, e)
			}
		case harness.ToolCallEnd:
			if t == "ToolCallEnd" {
				out = append(out, e)
			}
		case harness.ToolCallResult:
			if t == "ToolCallResult" {
				out = append(out, e)
			}
		case harness.RunEnd:
			if t == "RunEnd" {
				out = append(out, e)
			}
		case harness.RunError:
			if t == "RunError" {
				out = append(out, e)
			}
		}
	}
	return out
}

func translateFixture(t *testing.T, path string) *collectSink {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture %s: %v", path, err)
	}
	defer f.Close()

	sink := &collectSink{}
	state := &translatorState{
		runID:     "test-run",
		agentName: "test-agent",
		toolNames: make(map[string]string),
	}
	translateStream(f, state, sink)
	return sink
}

func TestTranslateSimpleRun(t *testing.T) {
	sink := translateFixture(t, "testdata/simple_run.ndjson")

	// Must start with RunStart.
	starts := sink.ofType("RunStart")
	if len(starts) != 1 {
		t.Fatalf("expected 1 RunStart, got %d", len(starts))
	}
	rs := starts[0].(harness.RunStart)
	if rs.HarnessRunID != "sess-abc123" {
		t.Errorf("HarnessRunID = %q, want sess-abc123", rs.HarnessRunID)
	}

	// Must have text content.
	deltas := sink.ofType("TextDelta")
	if len(deltas) == 0 {
		t.Fatal("expected TextDelta events, got none")
	}
	var text strings.Builder
	for _, d := range deltas {
		text.WriteString(d.(harness.TextDelta).Delta)
	}
	if !strings.Contains(text.String(), "I'll help you with that.") {
		t.Errorf("text = %q, want to contain assistant message", text.String())
	}

	// Must end with RunEnd (not RunError).
	ends := sink.ofType("RunEnd")
	if len(ends) != 1 {
		t.Fatalf("expected 1 RunEnd, got %d; errors: %v", len(ends), sink.ofType("RunError"))
	}
	re := ends[0].(harness.RunEnd)
	if re.StopReason != "success" {
		t.Errorf("StopReason = %q, want success", re.StopReason)
	}
	if re.Usage == nil {
		t.Fatal("RunEnd.Usage is nil")
	}
	if re.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", re.Usage.InputTokens)
	}
}

func TestTranslateToolCallRun(t *testing.T) {
	sink := translateFixture(t, "testdata/tool_call_run.ndjson")

	// Tool call start and end.
	starts := sink.ofType("ToolCallStart")
	ends := sink.ofType("ToolCallEnd")
	results := sink.ofType("ToolCallResult")

	if len(starts) != 1 {
		t.Fatalf("expected 1 ToolCallStart, got %d", len(starts))
	}
	if len(ends) != 1 {
		t.Fatalf("expected 1 ToolCallEnd, got %d", len(ends))
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 ToolCallResult, got %d", len(results))
	}

	ts := starts[0].(harness.ToolCallStart)
	if ts.ToolName != "Read" {
		t.Errorf("ToolName = %q, want Read", ts.ToolName)
	}
	if ts.ToolCallID != "toolu_01" {
		t.Errorf("ToolCallID = %q, want toolu_01", ts.ToolCallID)
	}

	tr := results[0].(harness.ToolCallResult)
	if tr.Result != "hello world" {
		t.Errorf("Result = %q, want hello world", tr.Result)
	}
	if tr.IsError {
		t.Error("IsError = true, want false")
	}

	// Must end with RunEnd.
	if len(sink.ofType("RunEnd")) != 1 {
		t.Fatal("expected RunEnd")
	}
}

func TestTranslateErrorMaxTurns(t *testing.T) {
	sink := translateFixture(t, "testdata/error_max_turns.ndjson")

	errors := sink.ofType("RunError")
	if len(errors) != 1 {
		t.Fatalf("expected 1 RunError, got %d", len(errors))
	}
	re := errors[0].(harness.RunError)
	if re.Code != harness.ErrCodeContextExhausted {
		t.Errorf("Code = %q, want context_exhausted", re.Code)
	}

	// Must NOT have RunEnd.
	if len(sink.ofType("RunEnd")) != 0 {
		t.Error("expected no RunEnd on error")
	}
}

func TestAdapterCapabilities(t *testing.T) {
	a := &Adapter{}
	caps := a.Capabilities()
	if caps.Protocol != harness.ProtocolStream {
		t.Errorf("Protocol = %q, want stream", caps.Protocol)
	}
	if !caps.Features.SystemPrompt {
		t.Error("expected SystemPrompt = true")
	}
	if !caps.Features.Reasoning {
		t.Error("expected Reasoning = true")
	}
	if !caps.Features.MultiTurn {
		t.Error("expected MultiTurn = true")
	}
	if caps.Requires.ToolExecutor {
		t.Error("expected ToolExecutor = false for stream adapter")
	}
}

func TestAdapterName(t *testing.T) {
	a := &Adapter{}
	if a.Name() != "claude-code" {
		t.Errorf("Name = %q, want claude-code", a.Name())
	}
}

func TestRegistryContainsClaude(t *testing.T) {
	adapter, err := harness.Lookup("claude-code")
	if err != nil {
		t.Fatalf("Lookup claude-code: %v", err)
	}
	if adapter.Name() != "claude-code" {
		t.Errorf("adapter.Name() = %q, want claude-code", adapter.Name())
	}
}

func TestHeartbeatEventTime(t *testing.T) {
	hb := harness.Heartbeat{At: time.Now()}
	if hb.EventTime().IsZero() {
		t.Error("Heartbeat.EventTime() is zero")
	}
}
