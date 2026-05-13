package opencode

import (
	"encoding/json"
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

	// Text region must be properly bracketed.
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
	if re.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", re.StopReason)
	}
	if re.HarnessRunID != "sess-abc" {
		t.Errorf("HarnessRunID = %q, want sess-abc", re.HarnessRunID)
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
		t.Errorf("CostUSD = %v, want 0.001", re.Usage.CostUSD)
	}
}

func TestTranslateToolCallRun(t *testing.T) {
	sink := translateFixture(t, "testdata/tool_call_run.ndjson")

	// Tool call must be atomic: ToolCallStart + ToolCallResult, no ToolCallEnd.
	starts := sink.ofType("ToolCallStart")
	ends := sink.ofType("ToolCallEnd")
	results := sink.ofType("ToolCallResult")

	if len(starts) != 1 {
		t.Fatalf("expected 1 ToolCallStart, got %d", len(starts))
	}
	if len(ends) != 0 {
		t.Fatalf("expected 0 ToolCallEnd (atomic harness), got %d", len(ends))
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 ToolCallResult, got %d", len(results))
	}

	ts := starts[0].(harness.ToolCallStart)
	if ts.ToolName != "bash" {
		t.Errorf("ToolName = %q, want bash", ts.ToolName)
	}
	if ts.ToolCallID != "call-001" {
		t.Errorf("ToolCallID = %q, want call-001", ts.ToolCallID)
	}

	tr := results[0].(harness.ToolCallResult)
	if tr.Result != "file.txt\n" {
		t.Errorf("Result = %q, want file.txt\\n", tr.Result)
	}
	if tr.IsError {
		t.Error("IsError = true, want false")
	}
	if tr.ToolName != "bash" {
		t.Errorf("ToolName = %q, want bash", tr.ToolName)
	}

	// Must end with RunEnd.
	if len(sink.ofType("RunEnd")) != 1 {
		t.Fatal("expected RunEnd")
	}

	// Must have text after the tool call.
	if len(sink.ofType("TextStart")) != 1 {
		t.Error("expected text region after tool call")
	}
}

func TestTranslateErrorContextLength(t *testing.T) {
	sink := translateFixture(t, "testdata/error_run.ndjson")

	errors := sink.ofType("RunError")
	if len(errors) != 1 {
		t.Fatalf("expected 1 RunError, got %d", len(errors))
	}
	re := errors[0].(harness.RunError)
	if re.Code != harness.ErrCodeContextExhausted {
		t.Errorf("Code = %q, want context_exhausted", re.Code)
	}
	if !strings.Contains(re.Message, "context window exceeded") {
		t.Errorf("Message = %q, want to contain 'context window exceeded'", re.Message)
	}

	// Must NOT have RunEnd.
	if len(sink.ofType("RunEnd")) != 0 {
		t.Error("expected no RunEnd on error")
	}
}

func TestTranslateAuthError(t *testing.T) {
	sink := &collectSink{}
	state := &translatorState{runID: "test-run"}
	line := []byte(`{"type":"error","error":{"type":"auth","message":"unauthorized"}}`)
	var ev opencodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range translateEvent(&ev, state) {
		sink.Emit(e)
	}
	errs := sink.ofType("RunError")
	if len(errs) != 1 {
		t.Fatalf("expected 1 RunError, got %d", len(errs))
	}
	if errs[0].(harness.RunError).Code != harness.ErrCodeAuthFailed {
		t.Errorf("Code = %q, want auth_failed", errs[0].(harness.RunError).Code)
	}
}

func TestTranslateUnknownError(t *testing.T) {
	sink := &collectSink{}
	state := &translatorState{runID: "test-run"}
	line := []byte(`{"type":"error","error":{"type":"weirdo","message":"something bad"}}`)
	var ev opencodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range translateEvent(&ev, state) {
		sink.Emit(e)
	}
	errs := sink.ofType("RunError")
	if len(errs) != 1 {
		t.Fatalf("expected 1 RunError, got %d", len(errs))
	}
	if errs[0].(harness.RunError).Code != harness.ErrCodeUnknown {
		t.Errorf("Code = %q, want unknown", errs[0].(harness.RunError).Code)
	}
}

func TestTranslateToolCallError(t *testing.T) {
	sink := &collectSink{}
	state := &translatorState{runID: "test-run"}
	line := []byte(`{"type":"tool_use","part":{"type":"tool","id":"t-1","tool":"bash","callID":"c-1","state":{"status":"error","input":{"command":"false"},"output":"","error":"command failed","time":{"start":1,"end":2}}}}`)
	var ev opencodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range translateEvent(&ev, state) {
		sink.Emit(e)
	}
	results := sink.ofType("ToolCallResult")
	if len(results) != 1 {
		t.Fatalf("expected 1 ToolCallResult, got %d", len(results))
	}
	tr := results[0].(harness.ToolCallResult)
	if !tr.IsError {
		t.Error("IsError = false, want true")
	}
	if tr.Result != "command failed" {
		t.Errorf("Result = %q, want 'command failed'", tr.Result)
	}
}

func TestTranslateReasoning(t *testing.T) {
	sink := &collectSink{}
	state := &translatorState{runID: "test-run"}
	line := []byte(`{"type":"reasoning","part":{"type":"reasoning","text":"thinking out loud","time":{"start":1,"end":2}}}`)
	var ev opencodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range translateEvent(&ev, state) {
		sink.Emit(e)
	}
	if len(sink.ofType("ReasoningStart")) != 1 {
		t.Error("expected ReasoningStart")
	}
	if len(sink.ofType("ReasoningDelta")) != 1 {
		t.Error("expected ReasoningDelta")
	}
	if len(sink.ofType("ReasoningEnd")) != 1 {
		t.Error("expected ReasoningEnd")
	}
	deltas := sink.ofType("ReasoningDelta")
	if deltas[0].(harness.ReasoningDelta).Delta != "thinking out loud" {
		t.Errorf("Delta = %q, want 'thinking out loud'", deltas[0].(harness.ReasoningDelta).Delta)
	}
}

func TestStepStartCapturesSessionID(t *testing.T) {
	sink := translateFixture(t, "testdata/simple_run.ndjson")
	// The RunEnd should carry the session ID captured from step_start.
	ends := sink.ofType("RunEnd")
	if len(ends) != 1 {
		t.Fatalf("expected 1 RunEnd, got %d", len(ends))
	}
	if ends[0].(harness.RunEnd).HarnessRunID != "sess-abc" {
		t.Errorf("HarnessRunID = %q, want sess-abc", ends[0].(harness.RunEnd).HarnessRunID)
	}
}

func TestAdapterCapabilities(t *testing.T) {
	a := &Adapter{}
	caps := a.Capabilities()
	if caps.Protocol != harness.ProtocolStream {
		t.Errorf("Protocol = %q, want stream", caps.Protocol)
	}
	if caps.Features.SystemPrompt {
		t.Error("expected SystemPrompt = false (known gap)")
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
		t.Error("expected BuiltInTools to be non-empty")
	}
}

func TestAdapterName(t *testing.T) {
	a := &Adapter{}
	if a.Name() != "opencode" {
		t.Errorf("Name = %q, want opencode", a.Name())
	}
}

func TestRegistryContainsOpencode(t *testing.T) {
	adapter, err := harness.Lookup("opencode")
	if err != nil {
		t.Fatalf("Lookup opencode: %v", err)
	}
	if adapter.Name() != "opencode" {
		t.Errorf("adapter.Name() = %q, want opencode", adapter.Name())
	}
}

func TestBuildArgsBasic(t *testing.T) {
	req := harness.SubSessionRequest{
		RunID: "r1",
		Task:  "hello world",
	}
	args := buildArgs(req, nil, req.Task)
	want := []string{"run", "--format", "json", "--dangerously-skip-permissions", "--", "hello world"}
	if !sliceEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBuildArgsWithConfig(t *testing.T) {
	req := harness.SubSessionRequest{
		RunID: "r1",
		Task:  "hello",
	}
	cfg := &Config{
		Model: "anthropic/claude-sonnet-4-5",
		Agent: "build",
		Args:  []string{"--extra", "flag"},
	}
	args := buildArgs(req, cfg, req.Task)
	// Must include --model, --agent, and the extra args before --.
	if !contains(args, "--model") || !contains(args, "anthropic/claude-sonnet-4-5") {
		t.Errorf("args missing --model: %v", args)
	}
	if !contains(args, "--agent") || !contains(args, "build") {
		t.Errorf("args missing --agent: %v", args)
	}
	if !contains(args, "--extra") {
		t.Errorf("args missing extra args: %v", args)
	}
	// "--" must appear right before the prompt.
	dashIdx := indexOf(args, "--")
	if dashIdx == -1 || dashIdx != len(args)-2 {
		t.Errorf("expected -- as second-to-last arg, got args = %v", args)
	}
	if args[len(args)-1] != "hello" {
		t.Errorf("last arg = %q, want 'hello'", args[len(args)-1])
	}
}

func TestBuildArgsWithResumeToken(t *testing.T) {
	req := harness.SubSessionRequest{
		RunID:       "r1",
		Task:        "continue",
		ResumeToken: "sess-xyz",
	}
	args := buildArgs(req, nil, req.Task)
	if !contains(args, "--session") || !contains(args, "sess-xyz") {
		t.Errorf("args missing --session sess-xyz: %v", args)
	}
}

func TestParseConfig(t *testing.T) {
	raw := json.RawMessage(`{"command":"opencode","model":"anthropic/claude-sonnet-4-5","agent":"build","args":["--verbose"]}`)
	cfg := parseConfig(raw)
	if cfg == nil {
		t.Fatal("parseConfig returned nil")
	}
	if cfg.Command != "opencode" {
		t.Errorf("Command = %q", cfg.Command)
	}
	if cfg.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.Agent != "build" {
		t.Errorf("Agent = %q", cfg.Agent)
	}
	if len(cfg.Args) != 1 || cfg.Args[0] != "--verbose" {
		t.Errorf("Args = %v", cfg.Args)
	}
}

func TestParseConfigEmpty(t *testing.T) {
	if parseConfig(nil) != nil {
		t.Error("expected nil for empty config")
	}
	if parseConfig([]byte{}) != nil {
		t.Error("expected nil for zero-length config")
	}
}

func TestHeartbeatEventTime(t *testing.T) {
	hb := harness.Heartbeat{At: time.Now()}
	if hb.EventTime().IsZero() {
		t.Error("Heartbeat.EventTime() is zero")
	}
}

// helpers

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
