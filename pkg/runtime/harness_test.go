package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/codingharness"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// harnessBinDir holds shim executables (codex, claude) shared by every
// harness test. The shims are written exactly once (see TestMain) and
// each cats a per-harness ".out" data file that individual tests
// overwrite. Because the executable bytes never change, the OS
// validates each shim only on first exec instead of paying that cost
// for a brand-new file in every test (~0.25s per first-time exec on
// macOS).
var harnessBinDir string

func TestMain(m *testing.M) {
	// The production JS-command wiring lives in pkg/runtime/jscommands,
	// which in-package tests cannot import (cycle); register the same
	// evaluator directly for the command-resolution tests.
	RegisterCommandEvaluator(func(agentTools []tools.Tool) CommandEvaluator {
		return js.NewEvaluator(agentTools)
	})
	RegisterHarness(codingharness.Factory)

	//nolint:forbidigo // TestMain has no *testing.T, so t.TempDir is unavailable.
	dir, err := os.MkdirTemp("", "harness-shim")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"codex", "claude"} {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat %q\nif [ -f %q ]; then exit \"$(cat %q)\"; fi\n",
			filepath.Join(dir, name+".args"), filepath.Join(dir, name+".out"),
			filepath.Join(dir, name+".exit"), filepath.Join(dir, name+".exit"))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			panic(err)
		}
	}
	harnessBinDir = dir

	// The package's user-config reads (cache_stable_prompts, cache_miss_warnings)
	// must see library defaults, not a developer's real ~/.config/cagent/config.yaml.
	// Set once before m.Run and never mutated, so it stays safe for parallel tests.
	//nolint:forbidigo // TestMain has no *testing.T, so t.TempDir is unavailable.
	configDir, err := os.MkdirTemp("", "runtime-test-config-*")
	if err != nil {
		panic(err)
	}
	paths.SetConfigDir(configDir)

	code := m.Run()

	paths.SetConfigDir("")
	_ = os.RemoveAll(configDir)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useHarnessShim points PATH at the shared shim directory and sets the
// stdout the named harness program ("codex" or "claude") emits when the
// runtime invokes it. PATH is prepended, not replaced, so the shim can
// still resolve "cat".
func useHarnessShim(t *testing.T, name, out string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(harnessBinDir, name+".out"), []byte(out), 0o600))
	require.NoError(t, os.RemoveAll(filepath.Join(harnessBinDir, name+".args")))
	require.NoError(t, os.RemoveAll(filepath.Join(harnessBinDir, name+".exit")))
	t.Setenv("PATH", harnessBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func useFailingHarnessShim(t *testing.T, name, out string) {
	t.Helper()
	useHarnessShim(t, name, out)
	require.NoError(t, os.WriteFile(filepath.Join(harnessBinDir, name+".exit"), []byte("1"), 0o600))
}

func harnessShimArgs(t *testing.T) string {
	t.Helper()
	args, err := os.ReadFile(filepath.Join(harnessBinDir, "codex.args"))
	require.NoError(t, err)
	return string(args)
}

func TestHarnessAgentRunStream(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"harness done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)

	rt := newHarnessRuntime(t, "codex")
	sess := session.New(session.WithUserMessage("do the task"))
	events := collectRuntimeEvents(t, rt, sess)

	assert.True(t, hasEventType(t, events, &AgentChoiceEvent{}))
	assert.Equal(t, "harness done", sess.GetLastAssistantMessageContent())

	var sawHarnessModel bool
	for _, ev := range events {
		if info, ok := ev.(*AgentInfoEvent); ok && info.Model == "codex" {
			sawHarnessModel = true
		}
	}
	assert.True(t, sawHarnessModel, "expected AgentInfo event with codex harness label")

	args := harnessShimArgs(t)
	assert.Contains(t, args, "do the task")
	assert.NotContains(t, args, "You are an external coder.")
	assert.NotContains(t, args, "<user>")
}

func TestHarnessAgentResumesPersistedSession(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"thread.started","thread_id":"thread-123"}
{"type":"item.completed","item":{"type":"agent_message","text":"first answer"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)

	store := session.NewInMemorySessionStore()
	rt := newHarnessRuntimeWithStore(t, "codex", store)
	sess := session.New(session.WithUserMessage("first question"))
	collectRuntimeEvents(t, rt, sess)

	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "thread-123", harnessSessionIDFor(loaded, rt.CurrentAgent()))

	useHarnessShim(t, "codex", `{"type":"thread.started","thread_id":"thread-123"}
{"type":"item.completed","item":{"type":"agent_message","text":"second answer"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)
	loaded.AddMessage(session.UserMessage("follow up only"))
	rt = newHarnessRuntimeWithStore(t, "codex", store)
	collectRuntimeEvents(t, rt, loaded)

	args := harnessShimArgs(t)
	assert.Contains(t, args, "exec\nresume\nthread-123\n")
	assert.Contains(t, args, "follow up only")
	assert.NotContains(t, args, "first question")
	assert.NotContains(t, args, "first answer")
	assert.Equal(t, "second answer", loaded.GetLastAssistantMessageContent())

	useFailingHarnessShim(t, "codex", `{"type":"thread.started","thread_id":"failed-thread"}
`)
	loaded.AddMessage(session.UserMessage("third question"))
	events := collectRuntimeEvents(t, rt, loaded)
	assert.True(t, hasEventType(t, events, &ErrorEvent{}))
	assert.Equal(t, "thread-123", harnessSessionIDFor(loaded, rt.CurrentAgent()))
}

func TestHarnessFirstFailedTurnCanResume(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}
	useHarnessShim(t, "codex", `{"type":"thread.started","thread_id":"retry-thread"}
{"type":"turn.failed","error":{"message":"quota exceeded"}}
`)
	store := session.NewInMemorySessionStore()
	rt := newHarnessRuntimeWithStore(t, "codex", store)
	sess := session.New(session.WithUserMessage("first question"))
	events := collectRuntimeEvents(t, rt, sess)
	assert.True(t, hasEventType(t, events, &ErrorEvent{}))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "retry-thread", harnessSessionIDFor(loaded, rt.CurrentAgent()))
	useHarnessShim(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"recovered"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)
	loaded.AddMessage(session.UserMessage("try again"))
	rt = newHarnessRuntimeWithStore(t, "codex", store)
	events = collectRuntimeEvents(t, rt, loaded)
	assert.False(t, hasEventType(t, events, &ErrorEvent{}))
	assert.Equal(t, "recovered", loaded.GetLastAssistantMessageContent())
	assert.Contains(t, harnessShimArgs(t), "exec\nresume\nretry-thread\n")
}

// TestHarnessRejectsImplicitOrMissingUserPrompt checks the genuine empty-prompt
// case: an implicit "Please proceed." with no task/system message is
// meaningless to the harness, so the run must be rejected before launch.
// Contrast with TestHarnessDelegatedTaskWithImplicitUserMessage below, which
// verifies that a delegation carrying a real task (system message + implicit
// user) succeeds.
func TestHarnessRejectsImplicitOrMissingUserPrompt(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"unexpected"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)
	rt := newHarnessRuntime(t, "codex")
	sess := session.New(session.WithImplicitUserMessage("Please proceed."))
	events := collectRuntimeEvents(t, rt, sess)

	assert.True(t, hasEventType(t, events, &ErrorEvent{}))
	_, err := os.Stat(filepath.Join(harnessBinDir, "codex.args"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestHarnessDelegatedTaskWithImplicitUserMessage is a regression test for
// https://github.com/docker/docker-agent/issues/4011. A transfer_task
// delegation builds a sub-session where the task text lives in the system
// message and the only user message is the implicit "Please proceed."
// filler injected by newSubSession. Before the fix this was rejected with
// "cannot run external harness without a user prompt"; now the full
// delegated context (system/task + implicit user) must be forwarded as the
// harness prompt.
func TestHarnessDelegatedTaskWithImplicitUserMessage(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"delegation done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)
	rt := newHarnessRuntime(t, "codex")
	// Mirror exactly what newSubSession builds for a transfer_task delegation:
	// the task goes into a system message and the user message is implicit.
	task := "implement the widget"
	sess := session.New(
		session.WithSystemMessage(buildTaskSystemMessage(task, "a working widget", nil)),
		session.WithImplicitUserMessage("Please proceed."),
	)
	events := collectRuntimeEvents(t, rt, sess)

	// Must launch without error.
	assert.False(t, hasEventType(t, events, &ErrorEvent{}))
	assert.Equal(t, "delegation done", sess.GetLastAssistantMessageContent())

	// Harness args must carry the task text so the harness can act on it.
	args := harnessShimArgs(t)
	assert.Contains(t, args, task)
	assert.Contains(t, args, "<task>")
}

func TestHarnessSubSessionIDSurvivesReconstruction(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"thread.started","thread_id":"child-thread"}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	parent := session.New(session.WithID("parent"))
	require.NoError(t, store.AddSession(t.Context(), parent))

	rt := newHarnessRuntimeWithStore(t, "codex", store)
	sess := session.New(session.WithParentID(parent.ID), session.WithUserMessage("child task"))
	collectRuntimeEvents(t, rt, sess)
	newPersistenceObserver(store).OnEvent(t.Context(), parent, SubSessionCompleted(parent.ID, sess, "root"))

	reconstructed, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "child-thread", harnessSessionIDFor(reconstructed, rt.CurrentAgent()))
}

func TestHarnessToolCallCompletes(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"item.started","item":{"type":"command_execution","command":"npm test"}}
{"type":"item.completed","item":{"type":"agent_message","text":"tests passed"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)

	rt := newHarnessRuntime(t, "codex")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("run tests")))

	var toolCall *ToolCallEvent
	var toolResponse *ToolCallResponseEvent
	for _, ev := range events {
		switch ev := ev.(type) {
		case *ToolCallEvent:
			toolCall = ev
		case *ToolCallResponseEvent:
			toolResponse = ev
		}
	}
	require.NotNil(t, toolCall)
	require.NotNil(t, toolResponse)
	assert.Equal(t, toolCall.ToolCall.ID, toolResponse.ToolCallID)
	require.NotNil(t, toolResponse.Result)
	assert.False(t, toolResponse.Result.IsError)
}

func TestHarnessShowsClaudeCodeToolCallAlongsideText(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "claude", `{"type":"assistant","message":{"content":[{"type":"text","text":"I will create the file."},{"type":"tool_use","id":"toolu_write","name":"Write","input":{"file_path":"/tmp/poem.md","content":"roses"}}]}}
{"type":"result","result":"created"}
`)

	rt := newHarnessRuntime(t, "claude-code")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("write poem")))

	var sawText bool
	var toolCall *ToolCallEvent
	for _, ev := range events {
		switch ev := ev.(type) {
		case *AgentChoiceEvent:
			if strings.Contains(ev.Content, "I will create the file") {
				sawText = true
			}
		case *ToolCallEvent:
			toolCall = ev
		}
	}
	assert.True(t, sawText)
	require.NotNil(t, toolCall)
	assert.Equal(t, "Write", toolCall.ToolCall.Function.Name)
	assert.Contains(t, toolCall.ToolCall.Function.Arguments, "/tmp/poem.md")
}

func TestHarnessSuppressesDuplicateClaudeCodeToolCall(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "claude", `{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash"}}}
{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"uname -a\"}"}}}
{"type":"stream_event","event":{"type":"content_block_stop","index":1}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"uname -a"}}]}}
{"type":"result","result":"done"}
`)

	rt := newHarnessRuntime(t, "claude-code")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("run uname")))

	var toolCalls []ToolCallEvent
	var partialArgs strings.Builder
	for _, ev := range events {
		switch ev := ev.(type) {
		case *ToolCallEvent:
			toolCalls = append(toolCalls, *ev)
		case *PartialToolCallEvent:
			partialArgs.WriteString(ev.ToolCall.Function.Arguments)
		}
	}
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "Bash", toolCalls[0].ToolCall.Function.Name)
	assert.Contains(t, partialArgs.String(), "uname -a")
}

func TestHarnessSuppressesReplayedClaudeCodeFinalText(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "claude", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}
{"type":"result","result":"Hello world"}
`)

	rt := newHarnessRuntime(t, "claude-code")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("say hello")))

	var chunks []string
	for _, ev := range events {
		if choice, ok := ev.(*AgentChoiceEvent); ok {
			chunks = append(chunks, choice.Content)
		}
	}
	assert.Equal(t, []string{"Hello", " world"}, chunks)
}

// A harness agent has no models to title its session with: Claude Code's
// driver answers the title prompt itself.
func TestHarnessAgentTitleGeneratedThroughClaudeCode(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "claude", `{"type":"result","subtype":"success","is_error":false,"result":"Fix the login bug"}`)

	gen := newHarnessRuntime(t, "claude-code").TitleGenerator(t.Context())
	require.NotNil(t, gen)
	title, err := gen.Generate(t.Context(), "sess-1", []string{"the login form rejects valid passwords"})
	require.NoError(t, err)
	assert.Equal(t, "Fix the login bug", title)

	args, err := os.ReadFile(filepath.Join(harnessBinDir, "claude.args"))
	require.NoError(t, err)
	assert.Contains(t, string(args), "--no-session-persistence")
	assert.NotContains(t, string(args), "--dangerously-skip-permissions")

	assert.Nil(t, newHarnessRuntime(t, "codex").TitleGenerator(t.Context()), "the codex driver cannot answer one-shot prompts")
}

func TestHarnessAgentTurnUsesEffortOverride(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "claude", `{"type":"result","result":"done"}`+"\n")
	root := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code", Effort: "low"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}),
		WithModelSwitcherConfig(&modelcatalog.Config{}))
	require.NoError(t, err)

	_, err = rt.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
	require.NoError(t, err)
	collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("do the task")))

	args, err := os.ReadFile(filepath.Join(harnessBinDir, "claude.args"))
	require.NoError(t, err)
	assert.Contains(t, string(args), "--effort\nmax\n")
	assert.NotContains(t, string(args), "--effort\nlow\n")
}

func newHarnessRuntime(t *testing.T, harnessType string) *LocalRuntime {
	t.Helper()
	return newHarnessRuntimeWithStore(t, harnessType, session.NewInMemorySessionStore())
}

func newHarnessRuntimeWithStore(t *testing.T, harnessType string, store session.Store) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "You are an external coder.", agent.WithHarness(&latest.HarnessConfig{Type: harnessType}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)
	return rt
}

func collectRuntimeEvents(t *testing.T, rt *LocalRuntime, sess *session.Session) []Event {
	t.Helper()
	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}
	return events
}

func TestHarnessCanceledFirstTurnRetainsThread(t *testing.T) {
	t.Parallel()
	store := session.NewInMemorySessionStore()
	root := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store),
		WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) {
			return canceledHarnessProvider{cancel: cancel}, nil
		}))
	require.NoError(t, err)
	sess := session.New(session.WithUserMessage("first question"))
	for range rt.RunStream(ctx, sess) {
	}
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "canceled-thread", harnessSessionIDFor(loaded, root))
}

func TestHarnessQuestionsReachTheUser(t *testing.T) {
	t.Parallel()
	provider := &elicitingHarnessProvider{}
	rt := newElicitingHarnessRuntime(t, provider)
	var request *ElicitationRequestEvent
	rt.OnElicitationRequest(func(event Event) {
		request = event.(*ElicitationRequestEvent)
		require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, map[string]any{"q1": "Blue"}, request.ElicitationID))
	})

	sess := session.New(session.WithUserMessage("ask me"))
	for range rt.RunStream(t.Context(), sess) {
	}

	require.NotNil(t, request)
	assert.Equal(t, "Which color?", request.Message)
	assert.Equal(t, map[string]any{"cagent/title": "Question"}, request.Meta)
	assert.Equal(t, sess.ID, request.SessionID)
	assert.Equal(t, "root", request.AgentName)
	assert.Equal(t, tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"q1": "Blue"}}, provider.result)
	assert.Equal(t, "Blue", sess.GetLastAssistantMessageContent())
}

func TestHarnessQuestionsStayInTextWithoutAUser(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		opts []Opt
		sess []session.Opt
		ctx  func(context.Context) context.Context
	}{
		{name: "non-interactive runtime", opts: []Opt{WithNonInteractive(true)}},
		{name: "non-interactive session", sess: []session.Opt{session.WithNonInteractive(true)}},
		{name: "background without a sink", ctx: tools.WithoutInteractivePrompts},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := &elicitingHarnessProvider{}
			rt := newElicitingHarnessRuntime(t, provider, tt.opts...)
			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(ctx)
			}
			sess := session.New(append(tt.sess, session.WithUserMessage("ask me"))...)
			for range rt.RunStream(ctx, sess) {
			}
			assert.False(t, provider.elicited, "the harness must not get an elicitation handler")
			assert.Equal(t, "no one to ask", sess.GetLastAssistantMessageContent())
		})
	}
}

func newElicitingHarnessRuntime(t *testing.T, provider *elicitingHarnessProvider, opts ...Opt) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code"}))
	opts = append([]Opt{
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
		WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) { return provider, nil }),
	}, opts...)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), opts...)
	require.NoError(t, err)
	return rt
}

// elicitingHarnessProvider asks one question when it gets an elicitation
// handler and answers with the user's choice.
type elicitingHarnessProvider struct {
	elicit   tools.ElicitationHandler
	elicited bool
	result   tools.ElicitationResult
}

func (p *elicitingHarnessProvider) Name() string { return "claude-code" }

func (p *elicitingHarnessProvider) WithElicitation(elicit tools.ElicitationHandler) harness.Provider {
	p.elicit = elicit
	p.elicited = true
	return p
}

func (p *elicitingHarnessProvider) Run(ctx context.Context, _ string, handle func(harness.Event)) error {
	if p.elicit == nil {
		handle(harness.Event{Type: harness.EventResult, Result: "no one to ask"})
		return nil
	}
	result, err := p.elicit(ctx, &mcp.ElicitParams{Message: "Which color?", Meta: mcp.Meta{"cagent/title": "Question"}})
	if err != nil {
		return err
	}
	p.result = result
	answer, _ := result.Content["q1"].(string)
	handle(harness.Event{Type: harness.EventResult, Result: answer})
	return nil
}

func (p *elicitingHarnessProvider) Resume(ctx context.Context, _, prompt string, handle func(harness.Event)) error {
	return p.Run(ctx, prompt, handle)
}

type canceledHarnessProvider struct{ cancel context.CancelFunc }

func (p canceledHarnessProvider) Name() string { return "codex" }
func (p canceledHarnessProvider) Run(ctx context.Context, _ string, handle func(harness.Event)) error {
	handle(harness.Event{Type: harness.EventSessionID, SessionID: "canceled-thread"})
	p.cancel()
	return ctx.Err()
}

func (p canceledHarnessProvider) Resume(ctx context.Context, _, prompt string, handle func(harness.Event)) error {
	return p.Run(ctx, prompt, handle)
}
