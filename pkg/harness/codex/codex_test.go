package codex

import (
	"os"
	"strings"
	"testing"

	extharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/harness"
)

// parseFixture parses every JSONL line in path through the stateless stream
// parser and returns the collected events.
func parseFixture(t *testing.T, path string) []harness.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out []harness.Event
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		out = append(out, parseStreamLine(line)...)
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
	events := parseFixture(t, "testdata/simple_run.ndjson")

	// agent_message yields EventText + EventResult; turn.completed yields a
	// second EventResult carrying usage.
	texts := eventsOfType(events, extharness.EventText)
	if len(texts) != 1 {
		t.Fatalf("expected 1 EventText, got %d", len(texts))
	}
	if texts[0].Text != "I'll help you." {
		t.Errorf("Text = %q, want assistant message", texts[0].Text)
	}

	results := eventsOfType(events, extharness.EventResult)
	if len(results) != 2 {
		t.Fatalf("expected 2 EventResult (agent_message + turn.completed), got %d", len(results))
	}
	// First result mirrors the agent message text.
	if results[0].Result != "I'll help you." {
		t.Errorf("results[0].Result = %q, want assistant message", results[0].Result)
	}
	// Second result carries usage.
	if results[1].Usage == nil {
		t.Fatal("results[1].Usage is nil; expected usage from turn.completed")
	}
	if results[1].Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", results[1].Usage.InputTokens)
	}
	if results[1].Usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", results[1].Usage.OutputTokens)
	}
}

func TestParseToolCallRun(t *testing.T) {
	events := parseFixture(t, "testdata/tool_call_run.ndjson")

	calls := eventsOfType(events, extharness.EventToolCall)
	if len(calls) != 1 {
		t.Fatalf("expected 1 EventToolCall, got %d", len(calls))
	}
	if calls[0].ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", calls[0].ToolName)
	}
	if calls[0].ToolArgs != "ls /tmp" {
		t.Errorf("ToolArgs = %q, want %q", calls[0].ToolArgs, "ls /tmp")
	}

	// item.completed for command_execution is ignored by the parser;
	// only agent_message produces EventText. So we should see exactly one
	// EventText (the assistant message after the tool call).
	texts := eventsOfType(events, extharness.EventText)
	if len(texts) != 1 {
		t.Fatalf("expected 1 EventText, got %d", len(texts))
	}
	if !strings.Contains(texts[0].Text, "The directory contains") {
		t.Errorf("Text = %q, want assistant message", texts[0].Text)
	}
}

func TestParseStreamLineThreadStartedIsNoEvent(t *testing.T) {
	events := parseStreamLine(`{"type":"thread.started","thread_id":"thread-xxx","model":"codex-mini"}`)
	if len(events) != 0 {
		t.Errorf("thread.started produced %d events, want 0 (stateless parser)", len(events))
	}
}

func TestParseStreamLineErrorIsNoEvent(t *testing.T) {
	// The error event is captured in RunStreaming and threaded into
	// RunResult.Err; ParseStreamLine does not surface it as an event.
	events := parseStreamLine(`{"type":"error","code":"unauthorized","message":"401 Unauthorized"}`)
	if len(events) != 0 {
		t.Errorf("error produced %d events from stateless parser, want 0", len(events))
	}
}

func TestParseStreamLineBadJSON(t *testing.T) {
	if events := parseStreamLine("not json"); events != nil {
		t.Errorf("non-JSON input produced %v, want nil", events)
	}
	if events := parseStreamLine(""); events != nil {
		t.Errorf("empty input produced %v, want nil", events)
	}
}

func TestAdapterName(t *testing.T) {
	a := New()
	if a.Name() != "codex" {
		t.Errorf("Name = %q, want codex", a.Name())
	}
}

func TestAdapterPrintCommand(t *testing.T) {
	a := New()
	got := a.PrintCommand("hello world")
	want := "codex exec --json --dangerously-bypass-approvals-and-sandbox -- 'hello world'"
	if got != want {
		t.Errorf("PrintCommand =\n  %s\nwant:\n  %s", got, want)
	}
}

func TestAdapterPrintCommandEscapesQuotes(t *testing.T) {
	a := New()
	got := a.PrintCommand("it's complicated")
	// Single quote must be escaped as '\''.
	if !strings.Contains(got, `'it'\''s complicated'`) {
		t.Errorf("PrintCommand did not shell-escape single quote: %s", got)
	}
}

func TestAdapterInteractiveArgs(t *testing.T) {
	a := New()
	args := a.InteractiveArgs("ignored")
	want := []string{"codex"}
	if len(args) != len(want) || args[0] != want[0] {
		t.Errorf("InteractiveArgs = %v, want %v", args, want)
	}
}

func TestAdapterParseStreamLineStateless(t *testing.T) {
	a := New()
	line := `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`
	events := a.ParseStreamLine(line)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (text + result), got %d", len(events))
	}
	if events[0].Type != extharness.EventText || events[0].Text != "ok" {
		t.Errorf("events[0] = %+v, want EventText('ok')", events[0])
	}
	if events[1].Type != extharness.EventResult || events[1].Result != "ok" {
		t.Errorf("events[1] = %+v, want EventResult('ok')", events[1])
	}
}

func TestAdapterImplementsProvider(t *testing.T) {
	var _ extharness.Provider = New()
}

func TestRegistryContainsCodex(t *testing.T) {
	p, err := harness.Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup codex: %v", err)
	}
	if p.Name() != "codex" {
		t.Errorf("Name = %q, want codex", p.Name())
	}
}

func TestBuildRunArgsFreshRun(t *testing.T) {
	req := harness.SubSessionRequest{
		Task:       "do a thing",
		WorkingDir: "/tmp/work",
	}
	args := buildRunArgs(req)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"-C /tmp/work",
		"-- do a thing",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got: %s", want, joined)
		}
	}
	if args[len(args)-1] != "do a thing" {
		t.Errorf("last arg = %q, want prompt", args[len(args)-1])
	}
}

func TestBuildRunArgsResume(t *testing.T) {
	req := harness.SubSessionRequest{
		Task:        "next message",
		ResumeToken: "thread-abc123",
		WorkingDir:  "/tmp/work",
	}
	args := buildRunArgs(req)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "exec resume thread-abc123 --json") {
		t.Errorf("resume args wrong: %s", joined)
	}
	if !strings.Contains(joined, "-- next message") {
		t.Errorf("resume prompt missing: %s", joined)
	}
	if strings.Contains(joined, "--dangerously-bypass") {
		t.Errorf("resume should not include --dangerously-bypass: %s", joined)
	}
	if strings.Contains(joined, "-C ") {
		t.Errorf("resume should not include -C: %s", joined)
	}
}

func TestBuildRunArgsSystemPromptPrepended(t *testing.T) {
	req := harness.SubSessionRequest{
		Task:         "do the work",
		SystemPrompt: "you are a careful agent",
	}
	args := buildRunArgs(req)
	prompt := args[len(args)-1]
	if !strings.Contains(prompt, "you are a careful agent") {
		t.Errorf("system prompt not prepended: %q", prompt)
	}
	if !strings.Contains(prompt, "do the work") {
		t.Errorf("task missing from prompt: %q", prompt)
	}
}

func TestBuildRunArgsResumeIgnoresSystemPrompt(t *testing.T) {
	// On resume the thread already carries instructions; we should not
	// prepend the system prompt to the user message.
	req := harness.SubSessionRequest{
		Task:         "continue",
		SystemPrompt: "you are a careful agent",
		ResumeToken:  "thread-xyz",
	}
	args := buildRunArgs(req)
	prompt := args[len(args)-1]
	if strings.Contains(prompt, "careful agent") {
		t.Errorf("resume should not prepend system prompt, got: %q", prompt)
	}
	if prompt != "continue" {
		t.Errorf("prompt = %q, want 'continue'", prompt)
	}
}

func TestClassifyErrorMessage(t *testing.T) {
	cases := map[string]harness.ErrorCode{
		"401 Unauthorized":                   harness.ErrCodeAuthFailed,
		"authentication failed":              harness.ErrCodeAuthFailed,
		"auth_failed: bad key":               harness.ErrCodeAuthFailed,
		"429 Too Many Requests":              harness.ErrCodeRateLimited,
		"rate limit exceeded":                harness.ErrCodeRateLimited,
		"context_window_exceeded: too long": harness.ErrCodeContextExhausted,
		"the context window was exceeded":    harness.ErrCodeContextExhausted,
		"something else went wrong":          harness.ErrCodeUnknown,
		"":                                   harness.ErrCodeUnknown,
	}
	for in, want := range cases {
		if got := classifyErrorMessage(in); got != want {
			t.Errorf("classifyErrorMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildEnvAllowlist(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret-openai")
	t.Setenv("SHOULD_BE_DROPPED", "leak-me")
	req := harness.SubSessionRequest{
		Env: map[string]string{"CUSTOM_KEY": "custom-value"},
	}
	env := buildEnv(req)

	hasOpenAI := false
	hasCustom := false
	for _, kv := range env {
		if kv == "OPENAI_API_KEY=secret-openai" {
			hasOpenAI = true
		}
		if kv == "CUSTOM_KEY=custom-value" {
			hasCustom = true
		}
		if strings.HasPrefix(kv, "SHOULD_BE_DROPPED=") {
			t.Errorf("buildEnv leaked SHOULD_BE_DROPPED through allowlist")
		}
	}
	if !hasOpenAI {
		t.Error("OPENAI_API_KEY not in env")
	}
	if !hasCustom {
		t.Error("CUSTOM_KEY (from req.Env) not in env")
	}
}
