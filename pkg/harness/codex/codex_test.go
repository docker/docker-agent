package codex

import (
	"os"
	"strings"
	"testing"

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
		case harness.ReasoningStart:
			if t == "ReasoningStart" {
				out = append(out, e)
			}
		case harness.ReasoningDelta:
			if t == "ReasoningDelta" {
				out = append(out, e)
			}
		case harness.ReasoningEnd:
			if t == "ReasoningEnd" {
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
	}
	translateStream(f, state, sink)
	return sink
}

func TestTranslateSimpleRun(t *testing.T) {
	sink := translateFixture(t, "testdata/simple_run.ndjson")

	// Must start with RunStart carrying the thread_id.
	starts := sink.ofType("RunStart")
	if len(starts) != 1 {
		t.Fatalf("expected 1 RunStart, got %d", len(starts))
	}
	rs := starts[0].(harness.RunStart)
	if rs.HarnessRunID != "thread-abc123" {
		t.Errorf("HarnessRunID = %q, want thread-abc123", rs.HarnessRunID)
	}
	if rs.ThreadID != "thread-abc123" {
		t.Errorf("ThreadID = %q, want thread-abc123", rs.ThreadID)
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
	if !strings.Contains(text.String(), "I'll help you.") {
		t.Errorf("text = %q, want to contain assistant message", text.String())
	}

	// TextStart + TextEnd pair.
	if len(sink.ofType("TextStart")) != 1 {
		t.Errorf("expected 1 TextStart, got %d", len(sink.ofType("TextStart")))
	}
	if len(sink.ofType("TextEnd")) != 1 {
		t.Errorf("expected 1 TextEnd, got %d", len(sink.ofType("TextEnd")))
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
	if re.HarnessRunID != "thread-abc123" {
		t.Errorf("RunEnd.HarnessRunID = %q, want thread-abc123 (for resume)", re.HarnessRunID)
	}
	if re.Usage == nil {
		t.Fatal("RunEnd.Usage is nil")
	}
	if re.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", re.Usage.InputTokens)
	}
	if re.Usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", re.Usage.OutputTokens)
	}
	if re.Usage.CostUSD != 0.001 {
		t.Errorf("CostUSD = %f, want 0.001", re.Usage.CostUSD)
	}
}

func TestTranslateToolCallRun(t *testing.T) {
	sink := translateFixture(t, "testdata/tool_call_run.ndjson")

	// Atomic tool call: ToolCallStart + ToolCallResult, NO ToolCallEnd.
	starts := sink.ofType("ToolCallStart")
	ends := sink.ofType("ToolCallEnd")
	results := sink.ofType("ToolCallResult")

	if len(starts) != 1 {
		t.Fatalf("expected 1 ToolCallStart, got %d", len(starts))
	}
	if len(ends) != 0 {
		t.Errorf("expected 0 ToolCallEnd (atomic harness), got %d", len(ends))
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 ToolCallResult, got %d", len(results))
	}

	ts := starts[0].(harness.ToolCallStart)
	if ts.ToolName != "shell" {
		t.Errorf("ToolName = %q, want shell", ts.ToolName)
	}
	if ts.ToolCallID != "item-001" {
		t.Errorf("ToolCallID = %q, want item-001", ts.ToolCallID)
	}

	tr := results[0].(harness.ToolCallResult)
	if tr.Result != "file.txt\n" {
		t.Errorf("Result = %q, want file.txt\\n", tr.Result)
	}
	if tr.IsError {
		t.Error("IsError = true, want false")
	}
	if tr.ToolCallID != ts.ToolCallID {
		t.Errorf("Result.ToolCallID = %q, want %q", tr.ToolCallID, ts.ToolCallID)
	}

	// Verify Start precedes Result with no intervening events of other kinds.
	var startIdx, resultIdx int = -1, -1
	for i, e := range sink.events {
		switch e.(type) {
		case harness.ToolCallStart:
			if startIdx < 0 {
				startIdx = i
			}
		case harness.ToolCallResult:
			if resultIdx < 0 {
				resultIdx = i
			}
		}
	}
	if startIdx < 0 || resultIdx < 0 {
		t.Fatal("missing ToolCallStart or ToolCallResult")
	}
	if resultIdx != startIdx+1 {
		t.Errorf("ToolCallResult should be adjacent to ToolCallStart (start=%d, result=%d)", startIdx, resultIdx)
	}

	// Also must have the message after the tool call.
	if len(sink.ofType("TextDelta")) == 0 {
		t.Error("expected TextDelta after tool call")
	}

	// Must end with RunEnd.
	if len(sink.ofType("RunEnd")) != 1 {
		t.Fatal("expected RunEnd")
	}
}

func TestTranslateErrorTurnFailed(t *testing.T) {
	sink := translateFixture(t, "testdata/error_turn_failed.ndjson")

	errors := sink.ofType("RunError")
	if len(errors) != 1 {
		t.Fatalf("expected 1 RunError, got %d", len(errors))
	}
	re := errors[0].(harness.RunError)
	if re.Code != harness.ErrCodeContextExhausted {
		t.Errorf("Code = %q, want context_exhausted", re.Code)
	}
	if !strings.Contains(re.Message, "context window") {
		t.Errorf("Message = %q, want to contain 'context window'", re.Message)
	}

	// Must NOT have RunEnd.
	if len(sink.ofType("RunEnd")) != 0 {
		t.Error("expected no RunEnd on error")
	}

	// Must have RunStart (thread.started came before turn.failed).
	if len(sink.ofType("RunStart")) != 1 {
		t.Error("expected RunStart before turn.failed")
	}
}

func TestMapErrorCode(t *testing.T) {
	cases := map[string]harness.ErrorCode{
		"context_window_exceeded": harness.ErrCodeContextExhausted,
		"rate_limit":              harness.ErrCodeRateLimited,
		"rate_limited":            harness.ErrCodeRateLimited,
		"authentication":          harness.ErrCodeAuthFailed,
		"auth_failed":             harness.ErrCodeAuthFailed,
		"unauthorized":            harness.ErrCodeAuthFailed,
		"something_else":          harness.ErrCodeUnknown,
		"":                        harness.ErrCodeUnknown,
	}
	for in, want := range cases {
		if got := mapErrorCode(in); got != want {
			t.Errorf("mapErrorCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamWithoutTurnEvent(t *testing.T) {
	// A stream that ends before any turn.completed or turn.failed must yield
	// a synthetic RunError(harness_crashed).
	input := strings.NewReader(`{"type":"thread.started","thread_id":"thread-xxx","model":"codex-mini"}` + "\n")
	sink := &collectSink{}
	state := &translatorState{runID: "test-run"}
	translateStream(input, state, sink)

	errors := sink.ofType("RunError")
	if len(errors) != 1 {
		t.Fatalf("expected synthetic RunError when stream ends abruptly, got %d", len(errors))
	}
	if errors[0].(harness.RunError).Code != harness.ErrCodeHarnessCrashed {
		t.Errorf("Code = %q, want harness_crashed", errors[0].(harness.RunError).Code)
	}
}

func TestBuildArgsFreshRun(t *testing.T) {
	req := harness.SubSessionRequest{
		Task:       "do a thing",
		WorkingDir: "/tmp/work",
	}
	args := buildArgs(req, nil)

	// Must include exec, --json, --sandbox workspace-write, --ask-for-approval never,
	// --skip-git-repo-check, --cd /tmp/work, --, prompt.
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"exec",
		"--json",
		"--sandbox workspace-write",
		"--ask-for-approval never",
		"--skip-git-repo-check",
		"--cd /tmp/work",
		"-- do a thing",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got: %s", want, joined)
		}
	}
	// Prompt is the last arg.
	if args[len(args)-1] != "do a thing" {
		t.Errorf("last arg = %q, want prompt", args[len(args)-1])
	}
}

func TestBuildArgsResume(t *testing.T) {
	req := harness.SubSessionRequest{
		Task:        "next message",
		ResumeToken: "thread-abc123",
		WorkingDir:  "/tmp/work",
	}
	args := buildArgs(req, nil)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "exec resume thread-abc123 --json") {
		t.Errorf("resume args wrong: %s", joined)
	}
	if !strings.Contains(joined, "-- next message") {
		t.Errorf("resume prompt missing: %s", joined)
	}
	// On resume, we should NOT pass --sandbox or --cd (the resumed thread has its own).
	if strings.Contains(joined, "--sandbox") {
		t.Errorf("resume should not include --sandbox: %s", joined)
	}
}

func TestBuildArgsSandboxOverride(t *testing.T) {
	req := harness.SubSessionRequest{Task: "x"}
	cfg := &Config{Sandbox: "read-only"}
	args := buildArgs(req, cfg)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--sandbox read-only") {
		t.Errorf("expected --sandbox read-only, got: %s", joined)
	}
}

func TestBuildArgsSystemPromptPrepended(t *testing.T) {
	req := harness.SubSessionRequest{
		Task:         "do the work",
		SystemPrompt: "you are a careful agent",
	}
	args := buildArgs(req, nil)

	prompt := args[len(args)-1]
	if !strings.Contains(prompt, "you are a careful agent") {
		t.Errorf("system prompt not prepended: %q", prompt)
	}
	if !strings.Contains(prompt, "do the work") {
		t.Errorf("task missing from prompt: %q", prompt)
	}
}

func TestAdapterCapabilities(t *testing.T) {
	a := &Adapter{}
	caps := a.Capabilities()
	if caps.Protocol != harness.ProtocolStream {
		t.Errorf("Protocol = %q, want stream", caps.Protocol)
	}
	if caps.Features.SystemPrompt {
		t.Error("expected SystemPrompt = false (codex exec has no flag)")
	}
	if !caps.Features.Reasoning {
		t.Error("expected Reasoning = true")
	}
	if caps.Features.TextDeltas {
		t.Error("expected TextDeltas = false")
	}
	if !caps.Features.MultiTurn {
		t.Error("expected MultiTurn = true")
	}
	if caps.Features.StreamingArgs {
		t.Error("expected StreamingArgs = false")
	}
	if caps.Requires.ToolExecutor {
		t.Error("expected ToolExecutor = false for stream adapter")
	}
	if len(caps.BuiltInTools) == 0 {
		t.Error("expected non-empty BuiltInTools")
	}
}

func TestAdapterName(t *testing.T) {
	a := &Adapter{}
	if a.Name() != "codex" {
		t.Errorf("Name = %q, want codex", a.Name())
	}
}

func TestRegistryContainsCodex(t *testing.T) {
	adapter, err := harness.Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup codex: %v", err)
	}
	if adapter.Name() != "codex" {
		t.Errorf("adapter.Name() = %q, want codex", adapter.Name())
	}
}

func TestParseConfig(t *testing.T) {
	raw := []byte(`{"command":"/usr/local/bin/codex","sandbox":"read-only","args":["--verbose"]}`)
	cfg := parseConfig(raw)
	if cfg == nil {
		t.Fatal("parseConfig returned nil")
	}
	if cfg.Command != "/usr/local/bin/codex" {
		t.Errorf("Command = %q", cfg.Command)
	}
	if cfg.Sandbox != "read-only" {
		t.Errorf("Sandbox = %q", cfg.Sandbox)
	}
	if len(cfg.Args) != 1 || cfg.Args[0] != "--verbose" {
		t.Errorf("Args = %v", cfg.Args)
	}

	if parseConfig(nil) != nil {
		t.Error("parseConfig(nil) should return nil")
	}
	if parseConfig([]byte("not json")) != nil {
		t.Error("parseConfig(invalid) should return nil")
	}
}
