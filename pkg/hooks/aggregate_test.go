package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAggregateTracksMostRestrictiveDecision pins the new
// Result.Decision contract: when multiple pre_tool_use hooks fire on a
// single tool call, the aggregated verdict is the most-restrictive
// (Deny > Ask > Allow). The runtime's tool-approval flow consults this
// to short-circuit the user prompt for Allow and to escalate Ask, so
// the ordering must be stable.
func TestAggregateTracksMostRestrictiveDecision(t *testing.T) {
	t.Parallel()

	mk := func(d Decision, reason string) hookResult {
		return hookResult{HandlerResult: HandlerResult{Output: &Output{
			HookSpecificOutput: &HookSpecificOutput{
				HookEventName:            EventPreToolUse,
				PermissionDecision:       d,
				PermissionDecisionReason: reason,
			},
		}}}
	}

	cases := []struct {
		name        string
		results     []hookResult
		wantVerdict Decision
		wantReason  string
		wantAllowed bool
	}{
		{
			name:        "no decision: Allowed=true, Decision empty",
			results:     []hookResult{{}},
			wantVerdict: "",
			wantAllowed: true,
		},
		{
			name:        "single allow",
			results:     []hookResult{mk(DecisionAllow, "safe")},
			wantVerdict: DecisionAllow,
			wantReason:  "safe",
			wantAllowed: true,
		},
		{
			name:        "single ask escalates over no decision",
			results:     []hookResult{{}, mk(DecisionAsk, "unclear")},
			wantVerdict: DecisionAsk,
			wantReason:  "unclear",
			wantAllowed: true, // Ask doesn't flip Allowed; the runtime handles the prompt.
		},
		{
			name: "deny beats ask beats allow",
			results: []hookResult{
				mk(DecisionAllow, "looks fine"),
				mk(DecisionAsk, "second-guess"),
				mk(DecisionDeny, "destructive"),
			},
			wantVerdict: DecisionDeny,
			wantReason:  "destructive",
			wantAllowed: false,
		},
		{
			name: "first reason wins on ties",
			results: []hookResult{
				mk(DecisionAsk, "first ask"),
				mk(DecisionAsk, "second ask"),
			},
			wantVerdict: DecisionAsk,
			wantReason:  "first ask",
			wantAllowed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			final := aggregate(tc.results, EventPreToolUse)
			assert.Equal(t, tc.wantVerdict, final.Decision)
			assert.Equal(t, tc.wantReason, final.DecisionReason)
			assert.Equal(t, tc.wantAllowed, final.Allowed)
		})
	}
}

// TestAggregateDecisionEmptyForNonPreToolUse documents that
// Result.Decision is meaningful only for pre_tool_use events. Other
// events (turn_start, post_tool_use, ...) MUST leave it empty so a
// runtime that consults it can't accidentally act on a stale verdict
// from an unrelated hook.
func TestAggregateDecisionEmptyForNonPreToolUse(t *testing.T) {
	t.Parallel()

	results := []hookResult{{HandlerResult: HandlerResult{Output: &Output{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:      EventTurnStart,
			PermissionDecision: DecisionAllow, // misconfigured but possible
		},
	}}}}

	final := aggregate(results, EventTurnStart)
	assert.Equal(t, Decision(""), final.Decision)
	assert.Empty(t, final.DecisionReason)
}
