//go:build js && wasm

// Package main is a js/wasm entry point that exposes docker-agent's agentic
// loop to the browser. It provides:
//
//   - Config parsing (any version → latest via pkg/config).
//   - Agent enumeration.
//   - A full agentic loop: streaming chat with tool calling, multi-agent
//     handoffs, fallback models, hooks, and remote MCP tool support.
//   - Request cancellation via abort().
//
// The binary is built with:
//
//	GOOS=js GOARCH=wasm go build -o web/docker-agent.wasm ./cmd/wasm
//
// and loaded by web/index.html via wasm_exec.js.
//
// What this entry point does NOT do:
//
//   - Run shell commands (no os/exec under js/wasm).
//   - Open files or listen on sockets.
//   - Persist sessions to sqlite.
//   - Render a TUI.
//
// What it DOES do:
//
//   - Full tool-calling loop (model requests tools → execute → return results).
//   - Multi-agent handoffs (transfer_task, handoff).
//   - Remote MCP tool support (HTTP/SSE transport, NOT stdio).
//   - Hooks (session_start, turn_start, pre_tool_use, post_tool_use).
//   - Fallback models with retry + backoff.
//   - Request cancellation via abort().
//   - Token usage reporting.
//
// The JS API (registered on `globalThis.dockerAgent`):
//
//	dockerAgent.parseConfig(yamlString) -> {version, agents, models}
//	dockerAgent.listAgents(yamlString) -> [{name, model, description, instruction}]
//	dockerAgent.chat({yaml, agentName?, env?, messages}, onEvent) -> Promise<{message, usage?}>
//	dockerAgent.abort() -> void   // cancels any in-flight chat request
//
// `onEvent` is called with one of:
//
//	{type: "delta",  content?: string, reasoning?: string}
//	{type: "tool_call", id: string, name: string, args: string}
//	{type: "tool_call_delta", id: string, name: string, arguments: string}
//	{type: "tool_result", id: string, name: string, output: string}
//	{type: "tool_blocked", id: string, name: string, reason: string}
//	{type: "handoff", from: string, to: string}
//	{type: "fallback", from: string, to: string, attempt: number, reason: string}
//	{type: "finish", reason: string}
//	{type: "usage",  input_tokens: number, output_tokens: number}
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall/js"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// abortState holds the cancellation machinery for the active chat request.
// Only one chat can be in flight at a time; calling abort() cancels it.
var abortState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func main() {
	api := js.Global().Get("Object").New()
	api.Set("_parseConfig", js.FuncOf(parseConfigJS))
	api.Set("_listAgents", js.FuncOf(listAgentsJS))
	api.Set("chat", js.FuncOf(chatJS))
	api.Set("abort", js.FuncOf(abortJS))
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

// ---------------------------------------------------------------------------
// parseConfig
// ---------------------------------------------------------------------------

func parseConfigJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return throwingError("parseConfig: expected yaml string argument")
	}
	yamlStr := args[0].String()

	cfg, err := config.Load(context.Background(), config.NewBytesSource("config.yaml", []byte(yamlStr)))
	if err != nil {
		return throwingError(fmt.Sprintf("parseConfig: %v", err))
	}

	return js.ValueOf(configToMap(cfg))
}

// ---------------------------------------------------------------------------
// listAgents
// ---------------------------------------------------------------------------

func listAgentsJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return throwingError("listAgents: expected yaml string argument")
	}
	yamlStr := args[0].String()

	cfg, err := config.Load(context.Background(), config.NewBytesSource("config.yaml", []byte(yamlStr)))
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

// ---------------------------------------------------------------------------
// abort
// ---------------------------------------------------------------------------

func abortJS(_ js.Value, _ []js.Value) any {
	abortState.mu.Lock()
	defer abortState.mu.Unlock()
	if abortState.cancel != nil {
		abortState.cancel()
		abortState.cancel = nil
	}
	return js.Undefined()
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

func chatJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return rejectedPromise("chat: expected at least one argument (options object)")
	}
	opts := args[0]
	var onEvent js.Value
	if len(args) >= 2 && args[1].Type() == js.TypeFunction {
		onEvent = args[1]
	}

	yamlStr := opts.Get("yaml").String()
	agentName := ""
	if v := opts.Get("agentName"); v.Type() == js.TypeString {
		agentName = v.String()
	}
	envMap := jsObjectToStringMap(opts.Get("env"))
	messages, err := jsToMessages(opts.Get("messages"))
	if err != nil {
		return rejectedPromise(fmt.Sprintf("chat: parsing messages: %v", err))
	}

	// Set up cancellable context.
	ctx, cancel := context.WithCancel(context.Background())
	abortState.mu.Lock()
	if abortState.cancel != nil {
		abortState.cancel()
	}
	abortState.cancel = cancel
	abortState.mu.Unlock()

	return newPromise(func(resolve, reject func(any)) {
		go func() {
			defer func() {
				abortState.mu.Lock()
				cancel()
				abortState.mu.Unlock()
			}()

			result, err := runChat(ctx, yamlStr, agentName, envMap, messages, onEvent)
			if err != nil {
				if ctx.Err() != nil {
					reject(jsError(fmt.Errorf("chat aborted")))
				} else {
					reject(jsError(err))
				}
				return
			}
			resolve(result)
		}()
	})
}

// runChat loads the config, builds the runtime, and runs the agentic loop.
func runChat(ctx context.Context, yamlStr, agentName string, envMap map[string]string, messages []chat.Message, onEvent js.Value) (any, error) {
	cfg, err := config.Load(ctx, config.NewBytesSource("config.yaml", []byte(yamlStr)))
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// Mirror the JS-supplied env map into the Go runtime's process env so any
	// provider that bypasses environment.Provider still finds its key.
	for k, v := range envMap {
		_ = os.Setenv(k, v)
	}

	env := environment.NewMapEnvProvider(envMap)

	// Build the full runtime with agents, tools, hooks, fallbacks.
	rt, err := buildRuntime(ctx, cfg, env, onEvent)
	if err != nil {
		return nil, fmt.Errorf("building runtime: %w", err)
	}

	// Run the agentic loop.
	result, err := rt.runAgentLoop(ctx, agentName, messages)
	if err != nil {
		return nil, err
	}

	return js.ValueOf(result), nil
}

// resolveModel resolves the agent's model field, which is either a key into
// cfg.Models or an inline "provider/model" reference.
func resolveModel(cfg *latest.Config, modelName string) (latest.ModelConfig, error) {
	if mc, ok := cfg.Models[modelName]; ok {
		return mc, nil
	}
	mc, err := latest.ParseModelRef(modelName)
	if err != nil {
		return latest.ModelConfig{}, fmt.Errorf("model %q: %w", modelName, err)
	}
	return mc, nil
}
