package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// trueHook is a minimal hook reused as the body of every per-event
// fixture below — the hook's identity doesn't matter to Has, only its
// presence does.
var trueHook = []Hook{{Type: HookTypeCommand, Command: "true"}}

// matcherWildcard wraps trueHook in the structure tool-scoped events
// (PreToolUse, PostToolUse) require.
var matcherWildcard = []MatcherConfig{{Matcher: "*", Hooks: trueHook}}

// onlyHooks maps each known event to a Config that lights up exactly
// that event. The cross-product test below iterates this map (once for
// the empty check, once for the per-event check), so adding a new
// event is a one-line update here — the same one-line update
// compileEvents needs.
var onlyHooks = map[EventType]*Config{
	EventPreToolUse:       {PreToolUse: matcherWildcard},
	EventPostToolUse:      {PostToolUse: matcherWildcard},
	EventSessionStart:     {SessionStart: trueHook},
	EventTurnStart:        {TurnStart: trueHook},
	EventTurnEnd:          {TurnEnd: trueHook},
	EventBeforeLLMCall:    {BeforeLLMCall: trueHook},
	EventAfterLLMCall:     {AfterLLMCall: trueHook},
	EventSessionEnd:       {SessionEnd: trueHook},
	EventOnUserInput:      {OnUserInput: trueHook},
	EventStop:             {Stop: trueHook},
	EventNotification:     {Notification: trueHook},
	EventOnError:          {OnError: trueHook},
	EventOnMaxIterations:  {OnMaxIterations: trueHook},
	EventBeforeCompaction: {BeforeCompaction: trueHook},
	EventAfterCompaction:  {AfterCompaction: trueHook},
}

// TestExecutorHasIsGeneric exercises the generic Has API across every
// event kind to ensure compileEvents is wired correctly. It guards
// against regressions where adding a new event to the Config struct
// silently fails to surface in Has/Dispatch.
func TestExecutorHasIsGeneric(t *testing.T) {
	t.Parallel()

	// Empty config: no event has hooks.
	empty := NewExecutor(nil, "/tmp", nil)
	for ev := range onlyHooks {
		assert.Falsef(t, empty.Has(ev), "empty executor must not report Has(%s)", ev)
	}

	// Cross-product: configuring event ev must light up Has(ev) and
	// only Has(ev).
	for ev, cfg := range onlyHooks {
		exec := NewExecutor(cfg, "/tmp", nil)
		for other := range onlyHooks {
			if other == ev {
				assert.Truef(t, exec.Has(other), "configuring %s must light up Has(%s)", ev, other)
			} else {
				assert.Falsef(t, exec.Has(other), "configuring %s must NOT light up Has(%s)", ev, other)
			}
		}
	}

	// Unknown events fall through cleanly rather than panicking.
	assert.False(t, empty.Has(EventType("does-not-exist")))
}
