//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall/js"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

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
	var handler js.Func
	handler = js.FuncOf(func(_ js.Value, args []js.Value) any {
		// The executor runs synchronously inside the constructor and only
		// captures the resolve/reject values, so the one-shot handler can go.
		defer handler.Release()
		resolve, reject := args[0], args[1]
		executor(
			func(v any) { resolve.Invoke(v) },
			func(v any) { reject.Invoke(v) },
		)
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

// promise runs fn on a goroutine and settles the returned Promise with its
// result; errors become rejections.
func promise(fn func() (any, error)) js.Value {
	return newPromise(func(resolve, reject func(any)) {
		go func() {
			result, err := fn()
			if err != nil {
				reject(jsError(err))
				return
			}
			resolve(result)
		}()
	})
}

// emit invokes onEvent(payload) and returns what it returned. It is a no-op
// returning undefined when onEvent is not a function, and a throwing
// handler is logged rather than allowed to unwind the Go caller.
func emit(onEvent js.Value, payload any) (result js.Value) {
	result = js.Undefined()
	if onEvent.Type() != js.TypeFunction {
		return result
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("onEvent handler threw", "error", fmt.Sprint(r))
		}
	}()
	return onEvent.Invoke(js.ValueOf(payload))
}

// notify invokes onEvent(payload) for its side effects only. A returned
// promise is not awaited; its rejection is logged so it never surfaces as
// an unhandled rejection in the host.
func notify(onEvent js.Value, payload any) {
	if result := emit(onEvent, payload); isThenable(result) {
		js.Global().Get("Promise").Call("resolve", result).Call("then", js.Undefined(), warnRejected)
	}
}

// warnRejected lives for the whole program: it is shared by every notify.
// It runs as a JS callback, which must not block: under Node the log write
// is an async fs call that cannot complete until the callback returns.
var warnRejected = js.FuncOf(func(_ js.Value, args []js.Value) any {
	reason := describe(firstArg(args))
	go slog.Warn("onEvent handler rejected", "error", reason)
	return nil
})

func isThenable(v js.Value) bool {
	return v.Type() == js.TypeObject && v.Get("then").Type() == js.TypeFunction
}

// awaitGate settles a JS value into a Go callback exactly once, no matter
// how the value's `then` behaves, and returns a function that disarms the
// callback. Promise.resolve gives the once-only guarantee for well-behaved
// thenables; the armed flag covers the rest and the disarm path.
var awaitGate = sync.OnceValue(func() js.Value {
	return js.Global().Call("eval", `(function (value, settle) {
  let armed = true;
  const once = (rejected) => (v) => {
    if (!armed) return;
    armed = false;
    settle(rejected, v);
  };
  Promise.resolve(value).then(once(false), once(true));
  return () => { armed = false; };
})`)
})

// awaitJS resolves a value returned by a JS callback: thenables are awaited
// until they settle or ctx ends, anything else is returned as is. After ctx
// ends a late settlement is dropped on the JS side, so the Go callback can
// be released safely.
func awaitJS(ctx context.Context, v js.Value) (js.Value, error) {
	if !isThenable(v) {
		return v, nil
	}
	type settled struct {
		value js.Value
		err   error
	}
	done := make(chan settled, 1)
	settle := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if args[0].Bool() {
			done <- settled{err: fmt.Errorf("callback rejected: %s", describe(args[1]))}
		} else {
			done <- settled{value: args[1]}
		}
		return nil
	})
	defer settle.Release()
	disarm := awaitGate().Invoke(v, settle)
	select {
	case result := <-done:
		return result.value, result.err
	case <-ctx.Done():
		// Go and JS never run at once, so nothing can slip between the
		// disarm and the deferred release.
		disarm.Invoke()
		return js.Undefined(), ctx.Err()
	}
}

func firstArg(args []js.Value) js.Value {
	if len(args) == 0 {
		return js.Undefined()
	}
	return args[0]
}

// describe renders a JS value for an error message, preferring an Error's
// message.
func describe(v js.Value) string {
	if v.Type() == js.TypeObject && v.Get("message").Type() == js.TypeString {
		return v.Get("message").String()
	}
	return js.Global().Get("String").Invoke(v).String()
}

func stringField(v js.Value, name string) string {
	if v.Type() != js.TypeObject {
		return ""
	}
	if f := v.Get(name); f.Type() == js.TypeString {
		return f.String()
	}
	return ""
}

func boolField(v js.Value, name string) bool {
	return v.Type() == js.TypeObject && v.Get(name).Truthy()
}

// jsObjectToStringMap converts a flat JS object {k: "v", ...} into a Go
// map[string]string. Non-string values are stringified via Object.toString;
// null and undefined values are skipped. A null/undefined input yields a
// nil map.
func jsObjectToStringMap(v js.Value) map[string]string {
	if v.Type() != js.TypeObject {
		return nil
	}
	keys := js.Global().Get("Object").Call("keys", v)
	out := make(map[string]string, keys.Length())
	for i := range keys.Length() {
		k := keys.Index(i).String()
		switch val := v.Get(k); val.Type() {
		case js.TypeString:
			out[k] = val.String()
		case js.TypeNull, js.TypeUndefined:
		default:
			out[k] = val.Call("toString").String()
		}
	}
	return out
}

// stringifyJS is JSON.stringify with JS exceptions (cycles, BigInt)
// returned as errors instead of panics.
func stringifyJS(v js.Value) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("JSON.stringify: %v", r)
		}
	}()
	encoded := js.Global().Get("JSON").Call("stringify", v)
	if encoded.Type() != js.TypeString {
		return "", errors.New("JSON.stringify: value is not serializable")
	}
	return encoded.String(), nil
}

// jsToJSON round-trips a JS value through JSON into plain Go values
// (map[string]any, []any, float64, string, bool, nil).
func jsToJSON(v js.Value) any {
	encoded, err := stringifyJS(v)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal([]byte(encoded), &out)
	return out
}

// jsToMessages decodes a JS array of OpenAI-style message objects into
// chat.Message values. The JSON round trip carries tool_calls and
// tool_call_id through, so a host can replay a full tool-calling history.
func jsToMessages(v js.Value) ([]chat.Message, error) {
	if v.Type() != js.TypeObject || !js.Global().Get("Array").Call("isArray", v).Bool() {
		return nil, errors.New("messages must be an array")
	}
	encoded, err := stringifyJS(v)
	if err != nil {
		return nil, err
	}
	var msgs []chat.Message
	if err := json.Unmarshal([]byte(encoded), &msgs); err != nil {
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
