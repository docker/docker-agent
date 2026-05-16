//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// ---------------------------------------------------------------------------
// JS <-> Go conversion helpers
// ---------------------------------------------------------------------------

// throwingError builds a JS object the JS-side wrapper recognises as an
// error to re-throw. We use a sentinel field so plain return values can never
// collide with it (real config output never includes a `__error` key).
func throwingError(msg string) any {
	return js.ValueOf(map[string]any{"__error": msg})
}

// jsError wraps a Go error in a JS Error so it can be passed to Promise.reject.
func jsError(err error) js.Value {
	return js.Global().Get("Error").New(err.Error())
}

// rejectedPromise returns a Promise that rejects immediately with msg.
// Used when we can detect a bad call before launching a goroutine.
func rejectedPromise(msg string) js.Value {
	return newPromise(func(_, reject func(any)) {
		reject(jsError(fmt.Errorf("%s", msg)))
	})
}

// newPromise builds a JavaScript Promise whose executor is a Go function.
// The returned js.Value is the Promise itself; resolve/reject are passed
// to executor and may be called from any goroutine.
func newPromise(executor func(resolve, reject func(any))) js.Value {
	promiseCtor := js.Global().Get("Promise")
	handler := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve := args[0]
		reject := args[1]
		executor(
			func(v any) { resolve.Invoke(v) },
			func(v any) { reject.Invoke(v) },
		)
		return nil
	})
	// Note: handler is a one-shot Func. The Promise executor is invoked
	// synchronously from the constructor, so it is safe to Release the
	// handler immediately afterwards. We don't, because the executor
	// captures goroutines that may continue to call resolve/reject — which
	// only invoke the captured `resolve`/`reject` js.Values, not the handler
	// itself, so leaking the handler is OK and avoids a subtle race.
	return promiseCtor.New(handler)
}

// emit invokes onEvent(payload) on the JS side, swallowing any error if the
// caller passed something other than a function (or nothing at all).
func emit(onEvent js.Value, payload any) {
	if onEvent.Type() != js.TypeFunction {
		return
	}
	onEvent.Invoke(js.ValueOf(payload))
}

// jsObjectToStringMap converts a flat JS object {k: "v", ...} into a Go
// map[string]string. Non-string values are stringified via Object.toString.
// A null/undefined input yields a nil map.
func jsObjectToStringMap(v js.Value) map[string]string {
	if v.Type() != js.TypeObject {
		return nil
	}
	keys := js.Global().Get("Object").Call("keys", v)
	out := make(map[string]string, keys.Length())
	for i := 0; i < keys.Length(); i++ {
		k := keys.Index(i).String()
		val := v.Get(k)
		if val.Type() == js.TypeString {
			out[k] = val.String()
		} else {
			out[k] = val.Call("toString").String()
		}
	}
	return out
}

// jsToMessages decodes a JS array of {role, content} objects into a slice of
// chat.Message. We round-trip through JSON so any extra fields the JS side
// supplies (e.g. tool_calls in a future iteration) flow through naturally.
func jsToMessages(v js.Value) ([]chat.Message, error) {
	if v.Type() != js.TypeObject {
		return nil, fmt.Errorf("messages must be an array")
	}
	jsonStr := js.Global().Get("JSON").Call("stringify", v).String()
	var msgs []chat.Message
	if err := json.Unmarshal([]byte(jsonStr), &msgs); err != nil {
		return nil, fmt.Errorf("decoding messages: %w", err)
	}
	return msgs, nil
}

// configToMap reduces a fully-parsed *latest.Config to the small JS-friendly
// shape returned by parseConfig. We deliberately omit fields that wouldn't
// mean anything in the browser (toolsets, hooks, sandbox).
func configToMap(cfg *latest.Config) map[string]any {
	agents := make([]any, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		agents = append(agents, map[string]any{
			"name":         a.Name,
			"description":  a.Description,
			"model":        a.Model,
			"instruction":  a.Instruction,
			"sub_agents":   stringsToAny(a.SubAgents),
			"handoffs":     stringsToAny(a.Handoffs),
			"add_date":     a.AddDate,
			"add_env_info": a.AddEnvironmentInfo,
		})
	}

	models := map[string]any{}
	for k, m := range cfg.Models {
		models[k] = map[string]any{
			"provider": m.Provider,
			"model":    m.Model,
			"base_url": m.BaseURL,
		}
	}

	return map[string]any{
		"version": cfg.Version,
		"agents":  agents,
		"models":  models,
	}
}

// stringsToAny widens a []string to []any so syscall/js.ValueOf accepts it.
func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
