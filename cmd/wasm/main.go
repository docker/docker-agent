//go:build js && wasm

// Package main is a js/wasm entry point that exposes docker-agent's agent
// runtime to the browser. It runs the same pkg/runtime the CLI uses,
// assembled through pkg/embeddedchat with browser-safe registries:
//
//   - Config parsing (any version → latest via pkg/config).
//   - Agent enumeration.
//   - Long-lived sessions with the full agentic loop: streaming, tool
//     calling with approval, multi-agent handoffs and transfers, fallback
//     models, builtin hooks, remote MCP servers with OAuth elicitation, the
//     portable builtin toolsets (todo, plan, memory, fetch, api, ...),
//     JavaScript ${...} expansion, code mode, TOON and deferred tools.
//   - A stateless chat call kept for the original demo page.
//
// The binary is built with:
//
//	GOOS=js GOARCH=wasm go build -o web/docker-agent.wasm ./cmd/wasm
//
// and loaded by web/index.html via wasm_exec.js.
//
// What this entry point does NOT do, by construction: run processes, read
// or write files, listen on sockets, persist sessions. Configs that need
// any of that are rejected when the session is created.
//
// The JS API (registered on `globalThis.dockerAgent`):
//
//	dockerAgent.parseConfig(yamlString) -> {version, agents, models}
//	dockerAgent.listAgents(yamlString) -> [{name, model, description, instruction}]
//	dockerAgent.createSession(options, onEvent) -> Promise<session>
//	dockerAgent.chat({...options, messages}, onEvent) -> Promise<{message, usage?}>
//	dockerAgent.abort() -> void   // cancels the in-flight chat() call
//
// options: {yaml, agentName?, env?, toolProxy?, oauthRedirectURI?, autoApprove?, documents?}
//
// documents is a {"logical/path.md": "content", ...} object: the documents
// `type: rag` toolsets index for the session (their `docs` select among
// these paths). At most maxDocuments entries and maxDocumentBytes in total.
//
// A session handle offers:
//
//	session.send(prompt) -> Promise<{message, usage?}>
//	session.confirm(decision | {decision, toolName?, reason?}, reason?) -> Promise<void>
//	session.respondToElicitation({id, action, content?}) -> Promise<void>
//	session.restart() -> Promise<void>
//	session.abort() -> void
//	session.close() -> Promise<void>
//
// confirm() rejects when no tool_confirmation is pending. Without an
// onEvent handler nobody can answer, so a session declines every
// tool_confirmation and elicitation.
//
// `onEvent` is called with one of:
//
//	{type: "delta", content?: string, reasoning?: string}
//	{type: "tool_call_delta", id, name, arguments}
//	{type: "tool_call", id, name, args}
//	{type: "tool_confirmation", id, name, args}   // answer with confirm()
//	{type: "tool_output", id, name, output}
//	{type: "tool_result", id, name, output, is_error}
//	{type: "tool_blocked", id, name, reason}
//	{type: "elicitation", id, message, mode, url, schema, meta}  // answer with respondToElicitation()
//	{type: "handoff", from, to}
//	{type: "fallback", from, to, attempt, reason}
//	{type: "usage", input_tokens, output_tokens, context_length, context_limit, cost}
//	{type: "warning", message}
//	{type: "error", message}
//	{type: "finish", reason}
//
// With chat(), the handler's return value answers tool_confirmation
// (true, a decision string or {decision, toolName?, reason?}) and
// elicitation ({action, content?}) events; a Promise is awaited until it
// settles or the call is aborted. Anything else, including a missing
// handler, declines the request. Return values for other events are
// ignored.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall/js"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/rag"
)

// compatState tracks the chat() call in flight so abort() can cancel it.
// Only one chat() runs at a time; starting another cancels the previous.
var compatState struct {
	mu     sync.Mutex
	active *compatRun
}

type compatRun struct{ cancel context.CancelFunc }

func main() {
	api := js.Global().Get("Object").New()
	api.Set("_parseConfig", js.FuncOf(parseConfigJS))
	api.Set("_listAgents", js.FuncOf(listAgentsJS))
	api.Set("chat", js.FuncOf(chatJS))
	api.Set("abort", js.FuncOf(abortJS))
	api.Set("createSession", js.FuncOf(createSessionJS))
	js.Global().Set("dockerAgent", api)

	// Wrap _parseConfig and _listAgents with JS shims that detect the
	// {__error: "..."} sentinel and throw a real Error.
	js.Global().Call("eval", `
(function () {
  function wrapSync(name) {
    const raw = globalThis.dockerAgent["_" + name];
    globalThis.dockerAgent[name] = function () {
      const r = raw.apply(null, arguments);
      if (r && typeof r === "object" && typeof r.__error === "string") {
        throw new Error(r.__error);
      }
      return r;
    };
    delete globalThis.dockerAgent["_" + name];
  }
  wrapSync("parseConfig");
  wrapSync("listAgents");
})();
	`)

	// Block forever: the runtime needs the Go scheduler alive so callbacks
	// keep working.
	select {}
}

func parseConfigJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return throwingError("parseConfig: expected yaml string argument")
	}
	cfg, err := config.Load(context.Background(), config.NewBytesSource("config.yaml", []byte(args[0].String())))
	if err != nil {
		return throwingError(fmt.Sprintf("parseConfig: %v", err))
	}
	return js.ValueOf(configToMap(cfg))
}

func listAgentsJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return throwingError("listAgents: expected yaml string argument")
	}
	cfg, err := config.Load(context.Background(), config.NewBytesSource("config.yaml", []byte(args[0].String())))
	if err != nil {
		return throwingError(fmt.Sprintf("listAgents: %v", err))
	}
	agents := make([]any, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		agents = append(agents, map[string]any{
			"name":        a.Name,
			"model":       a.Model,
			"description": a.Description,
			"instruction": a.Instruction,
		})
	}
	return js.ValueOf(agents)
}

// parseSessionOptions reads the options object shared by chat() and
// createSession().
func parseSessionOptions(opts js.Value) (sessionOptions, error) {
	if opts.Type() != js.TypeObject {
		return sessionOptions{}, errors.New("expected an options object")
	}
	if opts.Get("yaml").Type() != js.TypeString {
		return sessionOptions{}, errors.New("options.yaml must be a string")
	}
	documents, err := jsDocuments(opts.Get("documents"))
	if err != nil {
		return sessionOptions{}, fmt.Errorf("options.documents: %w", err)
	}
	return sessionOptions{
		YAML:             opts.Get("yaml").String(),
		AgentName:        stringField(opts, "agentName"),
		Env:              jsObjectToStringMap(opts.Get("env")),
		ToolProxy:        stringField(opts, "toolProxy"),
		OAuthRedirectURI: stringField(opts, "oauthRedirectURI"),
		AutoApprove:      boolField(opts, "autoApprove"),
		Documents:        documents,
	}, nil
}

// Caps on the documents one session may carry: the index lives in memory
// and embedding strategies send every chunk to a model.
const (
	maxDocuments     = 1000
	maxDocumentBytes = 16 << 20
)

// jsDocuments copies a {path: content} object into the session's documents.
// Values must be strings: there is no fallback that could quietly index a
// stringified object.
func jsDocuments(v js.Value) (rag.Documents, error) {
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return nil, nil
	case js.TypeObject:
	default:
		return nil, errors.New("must be an object of path to content")
	}
	keys := js.Global().Get("Object").Call("keys", v)
	if keys.Length() > maxDocuments {
		return nil, fmt.Errorf("%d documents exceed the limit of %d", keys.Length(), maxDocuments)
	}
	documents := make(rag.Documents, keys.Length())
	total := 0
	for i := range keys.Length() {
		path := keys.Index(i).String()
		content := v.Get(path)
		if strings.TrimSpace(path) == "" {
			return nil, errors.New("document paths must not be empty")
		}
		if content.Type() != js.TypeString {
			return nil, fmt.Errorf("document %q must be a string", path)
		}
		text := content.String()
		if total += len(text); total > maxDocumentBytes {
			return nil, fmt.Errorf("documents exceed the limit of %d bytes in total", maxDocumentBytes)
		}
		documents[path] = []byte(text)
	}
	return documents, nil
}

func onEventArg(args []js.Value) js.Value {
	if len(args) >= 2 && args[1].Type() == js.TypeFunction {
		return args[1]
	}
	return js.Undefined()
}

func createSessionJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return rejectedPromise("createSession: expected an options object")
	}
	opts, err := parseSessionOptions(args[0])
	if err != nil {
		return rejectedPromise("createSession: " + err.Error())
	}
	onEvent := onEventArg(args)
	return promise(func() (any, error) {
		session, err := browserHost.openSession(context.Background(), opts)
		if err != nil {
			return nil, fmt.Errorf("createSession: %w", err)
		}
		return newSessionHandle(session, onEvent), nil
	})
}

func abortJS(js.Value, []js.Value) any {
	compatState.mu.Lock()
	defer compatState.mu.Unlock()
	if compatState.active != nil {
		compatState.active.cancel()
	}
	return js.Undefined()
}

// chatJS is the stateless API: one session per call, seeded with the
// caller's message history, closed when the turn ends.
func chatJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return rejectedPromise("chat: expected at least one argument (options object)")
	}
	opts, err := parseSessionOptions(args[0])
	if err != nil {
		return rejectedPromise("chat: " + err.Error())
	}
	messages, err := jsToMessages(args[0].Get("messages"))
	if err != nil {
		return rejectedPromise(fmt.Sprintf("chat: parsing messages: %v", err))
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != chat.MessageRoleUser {
		return rejectedPromise("chat: the last message must be from the user")
	}
	prompt := messages[len(messages)-1].Content
	opts.History = messages[:len(messages)-1]
	onEvent := onEventArg(args)

	// Register the cancellation before anything runs so an abort() issued
	// right after chat() returns is not lost.
	ctx, cancel := context.WithCancel(context.Background())
	run := &compatRun{cancel: cancel}
	compatState.mu.Lock()
	if compatState.active != nil {
		compatState.active.cancel()
	}
	compatState.active = run
	compatState.mu.Unlock()

	return promise(func() (any, error) {
		defer func() {
			cancel()
			compatState.mu.Lock()
			defer compatState.mu.Unlock()
			if compatState.active == run {
				compatState.active = nil
			}
		}()
		result, err := runCompatChat(ctx, browserHost, opts, prompt, onEvent)
		if err != nil {
			return nil, err
		}
		return js.ValueOf(result), nil
	})
}

func runCompatChat(ctx context.Context, h host, opts sessionOptions, prompt string, onEvent js.Value) (map[string]any, error) {
	session, err := h.openSession(ctx, opts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errAborted
		}
		return nil, err
	}
	defer session.close()

	return session.send(prompt, func(turnCtx context.Context, event map[string]any) {
		switch event["type"] {
		case "tool_confirmation", "elicitation":
			// Waiting is bounded by the turn: an abort declines what the
			// host has not answered yet instead of hanging the call.
			reply, err := awaitJS(turnCtx, emit(onEvent, event))
			if err != nil {
				reply = js.Undefined()
			}
			answer(session, event, reply)
		default:
			notify(onEvent, event)
		}
	})
}
