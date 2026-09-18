//go:build js && wasm

package main

import (
	"context"
	"syscall/js"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/rag"
)

func TestCompatChatDeclinesConfirmationWithoutHandler(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("could not echo"))
	h := testHost(echo, map[string]provider.Provider{"root": model})

	result, err := runCompatChat(t.Context(), h, sessionOptions{YAML: echoAgentYAML}, "echo ping", js.Undefined())
	require.NoError(t, err, "an unanswerable confirmation must be declined, not awaited")
	assert.Equal(t, "could not echo", result["message"].(map[string]any)["content"])
	assert.Equal(t, 0, echo.callCount())
}

func TestCompatChatHandlerAnswersConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		calls  int
	}{
		{"boolean", "() => true", 1},
		{"decision string", "() => 'approve'", 1},
		{"decision object", "() => ({decision: 'approve_tool', toolName: 'echo'})", 1},
		{"promise", "() => Promise.resolve(true)", 1},
		{"rejected promise", "() => Promise.reject(new Error('nope'))", 0},
		{"throwing handler", "() => { throw new Error('nope') }", 0},
		{"no answer", "() => {}", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			echo := &echoToolSet{}
			model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
			h := testHost(echo, map[string]provider.Provider{"root": model})
			onEvent := js.Global().Call("eval", "(ev) => ev.type === 'tool_confirmation' ? ("+tc.answer+")() : undefined")

			_, err := runCompatChat(t.Context(), h, sessionOptions{YAML: echoAgentYAML}, "echo ping", onEvent)
			require.NoError(t, err)
			assert.Equal(t, tc.calls, echo.callCount())
		})
	}
}

func TestCompatChatSeedsHistoryAndReturnsUsage(t *testing.T) {
	model := newScriptedModel("mock/root", textTurn("pong again"))
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": model})
	var events []map[string]any
	onEvent := js.FuncOf(func(_ js.Value, args []js.Value) any {
		events = append(events, jsToJSON(args[0]).(map[string]any))
		return nil
	})
	defer onEvent.Release()

	history := []chat.Message{{Role: chat.MessageRoleUser, Content: "ping"}, {Role: chat.MessageRoleAssistant, Content: "pong"}}
	result, err := runCompatChat(t.Context(), h, sessionOptions{YAML: echoAgentYAML, History: history}, "again?", onEvent.Value)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{"role": "assistant", "content": "pong again"}, result["message"])
	assert.Equal(t, map[string]any{"input_tokens": int64(10), "output_tokens": int64(5)}, result["usage"])
	require.Len(t, model.lastCall(), 4)
	assert.Equal(t, "pong", model.lastCall()[2].Content)
	var types []string
	for _, e := range events {
		types = append(types, e["type"].(string))
	}
	assert.Equal(t, []string{"delta", "usage", "finish"}, types)
	assert.InDelta(t, 10, events[1]["input_tokens"], 0)
}

func TestCompatChatAbortedWhileConfirmationUnanswered(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("never"))
	h := testHost(echo, map[string]provider.Provider{"root": model})

	ctx, cancel := context.WithCancel(t.Context())
	onEvent := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if args[0].Get("type").String() != "tool_confirmation" {
			return nil
		}
		cancel()
		return js.Global().Call("eval", "new Promise(() => {})")
	})
	defer onEvent.Release()

	done := make(chan error, 1)
	go func() {
		_, err := runCompatChat(ctx, h, sessionOptions{YAML: echoAgentYAML}, "echo ping", onEvent.Value)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, errAborted)
	case <-time.After(5 * time.Second):
		t.Fatal("chat hung on a confirmation promise that never settles")
	}
	assert.Equal(t, 0, echo.callCount())
}

func TestCompatChatOnlyAwaitsAnswers(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
	h := testHost(echo, map[string]provider.Provider{"root": model})
	// Every other event gets a promise that never settles: awaiting it
	// would hang the call.
	onEvent := js.Global().Call("eval", `(ev) => ev.type === 'tool_confirmation' ? Promise.resolve(true) : new Promise(() => {})`)

	result, err := runCompatChat(t.Context(), h, sessionOptions{YAML: echoAgentYAML}, "echo ping", onEvent)
	require.NoError(t, err)
	assert.Equal(t, "done", result["message"].(map[string]any)["content"])
	assert.Equal(t, 1, echo.callCount())
}

func TestCompatChatAbortedBeforeItStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root", textTurn("never"))})
	_, err := runCompatChat(ctx, h, sessionOptions{YAML: echoAgentYAML}, "hi", js.Undefined())
	require.ErrorIs(t, err, errAborted)
}

func TestParseDecisionAndElicitationAnswer(t *testing.T) {
	eval := func(src string) js.Value { return js.Global().Call("eval", "("+src+")") }

	decision, toolName, reason := parseDecision(eval("true"))
	assert.Equal(t, []string{"approve", "", ""}, []string{decision, toolName, reason})
	decision, _, _ = parseDecision(eval("false"))
	assert.Equal(t, "reject", decision)
	decision, toolName, reason = parseDecision(eval("({decision: 'approve_tool', toolName: 'echo', reason: 'r'})"))
	assert.Equal(t, []string{"approve_tool", "echo", "r"}, []string{decision, toolName, reason})
	decision, _, _ = parseDecision(eval("undefined"))
	assert.Equal(t, "reject", decision)
	decision, _, _ = parseDecision(eval("42"))
	assert.Equal(t, "reject", decision)

	id, action, content := parseElicitationAnswer(eval("({id: 'e1', action: 'accept', content: {token: 't', n: 1}})"))
	assert.Equal(t, "e1", id)
	assert.Equal(t, "accept", action)
	assert.Equal(t, map[string]any{"token": "t", "n": float64(1)}, content)
	_, action, content = parseElicitationAnswer(eval("true"))
	assert.Equal(t, "accept", action)
	assert.Nil(t, content)
	_, action, _ = parseElicitationAnswer(eval("'cancel'"))
	assert.Equal(t, "cancel", action)
	_, action, _ = parseElicitationAnswer(eval("undefined"))
	assert.Equal(t, "decline", action)
}

func TestJSDocuments(t *testing.T) {
	eval := func(src string) js.Value { return js.Global().Call("eval", "("+src+")") }

	docs, err := jsDocuments(eval("undefined"))
	require.NoError(t, err)
	assert.Nil(t, docs, "no documents means the rag toolset has nothing to select from")

	docs, err = jsDocuments(eval("({'guide.md': 'hello', 'notes/todo.md': ''})"))
	require.NoError(t, err)
	assert.Equal(t, rag.Documents{"guide.md": []byte("hello"), "notes/todo.md": []byte{}}, docs)

	for name, tc := range map[string]struct{ src, want string }{
		"not an object": {"'guide.md'", "must be an object"},
		"non-string":    {"({'guide.md': 42})", `"guide.md" must be a string`},
		"empty path":    {"({'  ': 'x'})", "must not be empty"},
		"too many":      {"Object.fromEntries(Array.from({length: 1001}, (_, i) => ['d' + i, 'x']))", "exceed the limit of 1000"},
		"too large":     {"({a: 'x'.repeat(9 << 20), b: 'y'.repeat(8 << 20)})", "bytes in total"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := jsDocuments(eval(tc.src))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestAwaitJS(t *testing.T) {
	eval := func(src string) js.Value { return js.Global().Call("eval", "("+src+")") }

	v, err := awaitJS(t.Context(), eval("'plain'"))
	require.NoError(t, err)
	assert.Equal(t, "plain", v.String())

	v, err = awaitJS(t.Context(), eval("Promise.resolve(7)"))
	require.NoError(t, err)
	assert.Equal(t, 7, v.Int())

	_, err = awaitJS(t.Context(), eval("Promise.reject(new Error('boom'))"))
	require.ErrorContains(t, err, "boom")
}

func TestAwaitJSMisbehavingThenables(t *testing.T) {
	eval := func(src string) js.Value { return js.Global().Call("eval", "("+src+")") }

	// Settling more than once must neither block nor be reported twice.
	v, err := awaitJS(t.Context(), eval(`({then(res, rej) { res('first'); res('second'); rej(new Error('late')); }})`))
	require.NoError(t, err)
	assert.Equal(t, "first", v.String())

	_, err = awaitJS(t.Context(), eval(`({then() { throw new Error('bad then'); }})`))
	require.ErrorContains(t, err, "bad then")
}

func TestAwaitJSSettledAfterCancel(t *testing.T) {
	errs := captureConsoleErrors(t)
	deferred := js.Global().Call("eval", `(() => { const d = {}; d.promise = new Promise((res) => { d.resolve = res; }); return d; })()`)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := awaitJS(ctx, deferred.Get("promise"))
	require.ErrorIs(t, err, context.Canceled)

	// The late settlement runs before this await's own microtask; it must
	// not reach the released Go callback.
	deferred.Call("resolve", 1)
	_, err = awaitJS(t.Context(), js.Global().Call("eval", "Promise.resolve(0)"))
	require.NoError(t, err)
	assert.Empty(t, errs())
}

// captureConsoleErrors records console.error output for the test's lifetime.
func captureConsoleErrors(t *testing.T) func() []any {
	t.Helper()
	js.Global().Call("eval", `globalThis.__consoleErrors = []; globalThis.__consoleError = console.error;
console.error = (...args) => globalThis.__consoleErrors.push(args.map(String).join(' '));`)
	t.Cleanup(func() { js.Global().Call("eval", `console.error = globalThis.__consoleError`) })
	return func() []any {
		out, _ := jsToJSON(js.Global().Get("__consoleErrors")).([]any)
		return out
	}
}

func TestNotifyIgnoresHandlerResult(t *testing.T) {
	for _, src := range []string{
		"() => Promise.reject(new Error('boom'))",
		"() => ({then() { throw new Error('bad then'); }})",
		"() => 42",
		"() => { throw new Error('boom') }",
	} {
		notify(js.Global().Call("eval", "("+src+")"), map[string]any{"type": "delta"})
	}
	notify(js.Undefined(), map[string]any{"type": "delta"})
	// Let the rejection handlers run so a bug would surface here, not later.
	// A handler that blocks the event loop (logging from the callback under
	// Node did) would hang this await.
	done := make(chan error, 1)
	go func() {
		_, err := awaitJS(t.Context(), js.Global().Call("eval", "Promise.resolve(0)"))
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("rejection handler blocked the event loop")
	}
}

func TestEmitSurvivesThrowingHandler(t *testing.T) {
	result := emit(js.Global().Call("eval", "(() => { throw new Error('boom') })"), map[string]any{"type": "delta"})
	assert.Equal(t, js.TypeUndefined, result.Type())
	assert.Equal(t, js.TypeUndefined, emit(js.Undefined(), map[string]any{"type": "delta"}).Type())
}
