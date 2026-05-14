package claude

import (
	"os"
	"strings"
	"testing"

	extharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/harness"
)

// translateFixture parses every NDJSON line in path through the stateful
// streaming translator (state non-nil) and returns the collected events.
func translateFixture(t *testing.T, path string) []harness.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	state := &translatorState{
		toolNames:      make(map[string]string),
		blockTypes:     make(map[int]string),
		blockToolID:    make(map[int]string),
		streamedBlocks: make(map[string]map[int]bool),
	}
	var out []harness.Event
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		out = append(out, parseStreamLine(line, state)...)
	}
	return out
}

func eventsOfType(events []harness.Event, t extharness.EventType) []harness.Event {
	var out []harness.Event
	for _, e := range events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func TestParseSimpleRun(t *testing.T) {
	events := translateFixture(t, "testdata/simple_run.ndjson")

	// Final assistant text.
	texts := eventsOfType(events, extharness.EventText)
	if len(texts) == 0 {
		t.Fatal("expected at least one EventText, got none")
	}
	var combined strings.Builder
	for _, e := range texts {
		combined.WriteString(e.Text)
	}
	if !strings.Contains(combined.String(), "I'll help you with that.") {
		t.Errorf("text = %q, want to contain assistant message", combined.String())
	}

	// Terminal result event with usage.
	results := eventsOfType(events, extharness.EventResult)
	if len(results) != 1 {
		t.Fatalf("expected 1 EventResult, got %d", len(results))
	}
	r := results[0]
	if r.Result != "I'll help you with that." {
		t.Errorf("Result = %q, want assistant final text", r.Result)
	}
	if r.Usage == nil {
		t.Fatal("Usage is nil")
	}
	if r.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", r.Usage.InputTokens)
	}
	if r.Usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", r.Usage.OutputTokens)
	}
}

func TestParseToolCallRun(t *testing.T) {
	events := translateFixture(t, "testdata/tool_call_run.ndjson")

	// Tool call events: the fixture uses "Read" but only Bash/WebSearch/
	// WebFetch/Agent are in ToolArgFields, so we expect NO tool_call event.
	// Verify the parser does not panic and still produces the text events.
	calls := eventsOfType(events, extharness.EventToolCall)
	if len(calls) != 0 {
		t.Errorf("expected 0 EventToolCall for 'Read' (not in ToolArgFields), got %d", len(calls))
	}

	texts := eventsOfType(events, extharness.EventText)
	if len(texts) == 0 {
		t.Fatal("expected text events for assistant messages")
	}
	var combined strings.Builder
	for _, e := range texts {
		combined.WriteString(e.Text)
	}
	if !strings.Contains(combined.String(), "Let me read that file.") {
		t.Errorf("missing first assistant turn text in %q", combined.String())
	}
	if !strings.Contains(combined.String(), "The file contains: hello world") {
		t.Errorf("missing second assistant turn text in %q", combined.String())
	}

	// Terminal result.
	results := eventsOfType(events, extharness.EventResult)
	if len(results) != 1 {
		t.Fatalf("expected 1 EventResult, got %d", len(results))
	}
}

func TestParseStreamPartialDedupe(t *testing.T) {
	events := translateFixture(t, "testdata/stream_partial.ndjson")

	// Streaming worked: at least one text event from the deltas.
	texts := eventsOfType(events, extharness.EventText)
	if len(texts) == 0 {
		t.Fatal("expected EventText from stream_event deltas, got none")
	}

	var combined strings.Builder
	for _, e := range texts {
		combined.WriteString(e.Text)
	}
	full := combined.String()
	if !strings.Contains(full, "hello") {
		t.Errorf("combined text = %q, want to contain %q", full, "hello")
	}
	// Dedupe: the assistant message replays the same "hello" block that
	// was already streamed. It must NOT appear twice.
	if strings.Count(full, "hello") != 1 {
		t.Errorf("expected %q exactly once after dedupe, got %d times: %q",
			"hello", strings.Count(full, "hello"), full)
	}

	// Terminal result event.
	results := eventsOfType(events, extharness.EventResult)
	if len(results) != 1 {
		t.Fatalf("expected 1 EventResult, got %d", len(results))
	}
}

func TestParseErrorMaxTurns(t *testing.T) {
	events := translateFixture(t, "testdata/error_max_turns.ndjson")

	// The wire format emits a single `result` event with subtype
	// "error_max_turns". The rumpl/harness Event vocabulary collapses all
	// terminal events into EventResult; callers inspect the original
	// subtype out-of-band. We just verify we don't crash and we surface a
	// result event.
	results := eventsOfType(events, extharness.EventResult)
	if len(results) != 1 {
		t.Fatalf("expected 1 EventResult, got %d", len(results))
	}
}

func TestAdapterName(t *testing.T) {
	a := New("claude-sonnet-4-5")
	if a.Name() != "claude-code" {
		t.Errorf("Name = %q, want claude-code", a.Name())
	}
}

func TestAdapterPrintCommand(t *testing.T) {
	a := New("claude-sonnet-4-5")
	cmd := a.PrintCommand("hello world")
	wantFragments := []string{
		"claude",
		"--print",
		"--verbose",
		"--dangerously-skip-permissions",
		"--output-format stream-json",
		"--include-partial-messages",
		"--model 'claude-sonnet-4-5'",
		"-p 'hello world'",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(cmd, frag) {
			t.Errorf("PrintCommand missing %q\ngot: %s", frag, cmd)
		}
	}
}

func TestAdapterPrintCommandWithEffort(t *testing.T) {
	a := New("claude-sonnet-4-5", WithEffort(EffortHigh))
	cmd := a.PrintCommand("hi")
	if !strings.Contains(cmd, "--effort high") {
		t.Errorf("expected --effort high, got: %s", cmd)
	}
}

func TestAdapterInteractiveArgs(t *testing.T) {
	a := New("claude-sonnet-4-5")
	args := a.InteractiveArgs("ignored")
	want := []string{"claude", "--dangerously-skip-permissions", "--model", "claude-sonnet-4-5"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}

	a2 := New("claude-sonnet-4-5", WithEffort(EffortMax))
	args2 := a2.InteractiveArgs("")
	if len(args2) < 6 || args2[4] != "--effort" || args2[5] != "max" {
		t.Errorf("expected --effort max in args, got %v", args2)
	}
}

func TestAdapterParseStreamLineStateless(t *testing.T) {
	a := New("claude-sonnet-4-5")
	// A result line should still parse via the stateless ParseStreamLine.
	line := `{"type":"result","subtype":"success","result":"ok","usage":{"input_tokens":1,"output_tokens":1}}`
	events := a.ParseStreamLine(line)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != extharness.EventResult {
		t.Errorf("Type = %q, want result", events[0].Type)
	}
	if events[0].Result != "ok" {
		t.Errorf("Result = %q, want ok", events[0].Result)
	}
}

func TestAdapterParseStreamLineStatelessDoesNotEmitStreamEvents(t *testing.T) {
	a := New("claude-sonnet-4-5")
	// Stream events are stateful only; stateless callers should not see them.
	line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}}`
	events := a.ParseStreamLine(line)
	if len(events) != 0 {
		t.Errorf("stateless ParseStreamLine emitted %d events for stream_event line, want 0", len(events))
	}
}

func TestAdapterImplementsProvider(t *testing.T) {
	var _ extharness.Provider = New("claude-sonnet-4-5")
}

func TestRegistryContainsClaude(t *testing.T) {
	p, err := harness.Lookup("claude-code")
	if err != nil {
		t.Fatalf("Lookup claude-code: %v", err)
	}
	if p.Name() != "claude-code" {
		t.Errorf("Name = %q, want claude-code", p.Name())
	}
}

func TestBuildEnvAllowlist(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret-anthropic")
	t.Setenv("SHOULD_BE_DROPPED", "leak-me")
	req := harness.SubSessionRequest{
		Env: map[string]string{"CUSTOM_KEY": "custom-value"},
	}
	env := buildEnv(req)

	hasAnthropic := false
	hasCustom := false
	for _, kv := range env {
		if kv == "ANTHROPIC_API_KEY=secret-anthropic" {
			hasAnthropic = true
		}
		if kv == "CUSTOM_KEY=custom-value" {
			hasCustom = true
		}
		if strings.HasPrefix(kv, "SHOULD_BE_DROPPED=") {
			t.Errorf("buildEnv leaked SHOULD_BE_DROPPED through allowlist")
		}
	}
	if !hasAnthropic {
		t.Error("ANTHROPIC_API_KEY not in env")
	}
	if !hasCustom {
		t.Error("CUSTOM_KEY (from req.Env) not in env")
	}
}

func TestWriteTempPrompt(t *testing.T) {
	path, err := writeTempPrompt("you are a helpful assistant")
	if err != nil {
		t.Fatalf("writeTempPrompt: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp prompt: %v", err)
	}
	if string(data) != "you are a helpful assistant" {
		t.Errorf("prompt file contents = %q, want %q", string(data), "you are a helpful assistant")
	}
}

func TestExtractSessionID(t *testing.T) {
	got, ok := extractSessionID(`{"type":"system","session_id":"sess-xyz","model":"claude"}`)
	if !ok || got != "sess-xyz" {
		t.Errorf("extractSessionID = %q, ok=%v, want sess-xyz, true", got, ok)
	}

	_, ok = extractSessionID(`{"type":"system","model":"claude"}`)
	if ok {
		t.Error("extractSessionID succeeded on line without session_id")
	}
}
