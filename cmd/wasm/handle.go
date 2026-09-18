//go:build js && wasm

package main

import (
	"context"
	"sync"
	"syscall/js"
)

// sessionHandle is the JS object returned by createSession. Its methods are
// js.Funcs that must be released exactly once, after close, so the handle
// tracks them itself.
type sessionHandle struct {
	session *chatSession
	onEvent js.Value
	obj     js.Value

	release sync.Once
	funcs   map[string]js.Func
}

// closedStubs replace a closed handle's methods so late calls fail with a
// clear error instead of "call to released function"; close stays
// idempotent and abort stays a no-op.
var closedStubs = sync.OnceValue(func() js.Value {
	return js.Global().Call("eval", `({
  send: () => Promise.reject(new Error("session is closed")),
  confirm: () => Promise.reject(new Error("session is closed")),
  respondToElicitation: () => Promise.reject(new Error("session is closed")),
  restart: () => Promise.reject(new Error("session is closed")),
  abort: () => {},
  close: () => Promise.resolve(),
})`)
})

// newSessionHandle exposes session to JS. Every method returns a Promise
// except abort, which is synchronous and never fails.
func newSessionHandle(session *chatSession, onEvent js.Value) js.Value {
	h := &sessionHandle{session: session, onEvent: onEvent, obj: js.Global().Get("Object").New(), funcs: map[string]js.Func{}}
	h.method("send", h.send)
	h.method("confirm", h.confirm)
	h.method("respondToElicitation", h.respondToElicitation)
	h.method("restart", h.restart)
	h.method("abort", func(js.Value, []js.Value) any {
		session.abort()
		return js.Undefined()
	})
	h.method("close", h.close)
	return h.obj
}

func (h *sessionHandle) method(name string, fn func(js.Value, []js.Value) any) {
	f := js.FuncOf(fn)
	h.funcs[name] = f
	h.obj.Set(name, f)
}

func (h *sessionHandle) send(_ js.Value, args []js.Value) any {
	if len(args) < 1 || args[0].Type() != js.TypeString {
		return rejectedPromise("send: expected a prompt string")
	}
	// The turn is reserved before the promise is handed back so
	// `session.send(p); session.abort()` cancels it.
	t, err := h.session.start(args[0].String())
	if err != nil {
		return rejectedPromise(err.Error())
	}
	return promise(func() (any, error) {
		result, err := h.session.run(t, h.emit)
		if err != nil {
			return nil, err
		}
		return js.ValueOf(result), nil
	})
}

// emit hands an event to the host. Without a handler nobody can answer a
// tool_confirmation or elicitation, so they are declined instead of
// stalling the turn.
func (h *sessionHandle) emit(_ context.Context, event map[string]any) {
	if h.onEvent.Type() != js.TypeFunction {
		answer(h.session, event, js.Undefined())
		return
	}
	notify(h.onEvent, event)
}

// confirm accepts confirm(decision, reason?) or
// confirm({decision, toolName?, reason?}).
func (h *sessionHandle) confirm(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return rejectedPromise("confirm: expected a decision")
	}
	decision, toolName, reason := parseDecision(args[0])
	if len(args) >= 2 && args[1].Type() == js.TypeString {
		reason = args[1].String()
	}
	return promise(func() (any, error) {
		return js.Undefined(), h.session.confirm(decision, toolName, reason)
	})
}

// respondToElicitation accepts respondToElicitation({id, action, content?}).
func (h *sessionHandle) respondToElicitation(_ js.Value, args []js.Value) any {
	if len(args) < 1 || args[0].Type() != js.TypeObject {
		return rejectedPromise("respondToElicitation: expected {id, action, content?}")
	}
	id, action, content := parseElicitationAnswer(args[0])
	if id == "" {
		return rejectedPromise("respondToElicitation: id is required")
	}
	return promise(func() (any, error) {
		return js.Undefined(), h.session.respondToElicitation(id, action, content)
	})
}

func (h *sessionHandle) restart(js.Value, []js.Value) any {
	return promise(func() (any, error) {
		return js.Undefined(), h.session.restart()
	})
}

func (h *sessionHandle) close(js.Value, []js.Value) any {
	return promise(func() (any, error) {
		err := h.session.close()
		h.release.Do(func() {
			stubs := closedStubs()
			for name, f := range h.funcs {
				h.obj.Set(name, stubs.Get(name))
				f.Release()
			}
		})
		return js.Undefined(), err
	})
}

// answer settles a tool_confirmation or elicitation event with the host's
// reply; anything unusable, undefined included, declines it. Other events
// need no answer.
func answer(session *chatSession, event map[string]any, reply js.Value) {
	switch event["type"] {
	case "tool_confirmation":
		decision, toolName, reason := parseDecision(reply)
		if err := session.confirm(decision, toolName, reason); err != nil {
			_ = session.confirm("reject", "", err.Error())
		}
	case "elicitation":
		id, _ := event["id"].(string)
		_, action, content := parseElicitationAnswer(reply)
		if err := session.respondToElicitation(id, action, content); err != nil {
			_ = session.respondToElicitation(id, "decline", nil)
		}
	}
}

// parseDecision reads a confirmation decision given as a string, a boolean
// (true approves) or a {decision, toolName?, reason?} object. Anything else
// rejects the tool call.
func parseDecision(v js.Value) (decision, toolName, reason string) {
	switch v.Type() {
	case js.TypeString:
		return v.String(), "", ""
	case js.TypeBoolean:
		if v.Bool() {
			return "approve", "", ""
		}
		return "reject", "", ""
	case js.TypeObject:
		return stringField(v, "decision"), stringField(v, "toolName"), stringField(v, "reason")
	}
	return "reject", "", ""
}

// parseElicitationAnswer reads an elicitation answer given as an
// {id?, action, content?} object, an action string or a boolean (true
// accepts). Anything else declines the request.
func parseElicitationAnswer(v js.Value) (id, action string, content map[string]any) {
	switch v.Type() {
	case js.TypeString:
		return "", v.String(), nil
	case js.TypeBoolean:
		if v.Bool() {
			return "", "accept", nil
		}
	case js.TypeObject:
		if c := v.Get("content"); c.Type() == js.TypeObject {
			content, _ = jsToJSON(c).(map[string]any)
		}
		return stringField(v, "id"), stringField(v, "action"), content
	}
	return "", "decline", nil
}
