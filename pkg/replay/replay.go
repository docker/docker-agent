// Package replay compares the behaviour of two recorded agent sessions.
//
// It answers the triage question "did the agent do something different this
// time, and where did it first diverge?" — the thing you actually want to know
// when a task that worked yesterday does not work today.
//
// # Behaviour, not prose
//
// Comparison is over the sequence of tool calls, not over the assistant's text.
// Model output is nondeterministic: two runs of the same task almost always word
// things differently while doing exactly the same work, so diffing prose reports
// a difference on essentially every comparison and is useless as a signal. The
// tool calls are what changed the world, so they are what is compared.
//
// Assistant text is still carried on each [Turn] so a reporter can show what was
// said around a divergence; it just does not decide whether a divergence
// happened.
//
// # Delegated work
//
// A turn taken by a sub-agent is still a turn the run took, so sub-sessions are
// walked in place: their turns appear in sequence where the delegation happened.
// Skipping them would report "identical behaviour" for two runs whose sub-agents
// did entirely different things — the precise wrong answer to the question this
// package exists for.
//
// # First divergence only
//
// Once two runs differ, everything after that point is downstream of the
// difference and comparing it produces noise, not information. So the comparison
// stops at the first divergence and reports where it was.
package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// ToolCall is the comparable part of a tool invocation.
type ToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Turn is one assistant turn: what it said, and what it called.
type Turn struct {
	// Index is the turn's 0-based position in the session.
	Index int `json:"index"`
	// Agent is the agent that produced the turn, so a divergence in a
	// multi-agent run can be attributed.
	Agent string `json:"agent,omitempty"`
	// Content is the assistant text. Carried for reporting; never compared.
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Kind classifies how two runs differ.
type Kind string

const (
	// KindToolCalls means both runs made a turn at this index but called
	// different tools, or the same tools with different arguments.
	KindToolCalls Kind = "tool_calls"
	// KindExtraTurn means the second run kept going after the first stopped.
	KindExtraTurn Kind = "extra_turn"
	// KindMissingTurn means the second run stopped before the first did.
	KindMissingTurn Kind = "missing_turn"
)

// Divergence is the first behavioural difference found.
type Divergence struct {
	Kind Kind `json:"kind"`
	// TurnIndex is the 0-based turn at which the runs first differ.
	TurnIndex int `json:"turn_index"`
	// A and B are the diverging turns. Either may be nil when one run had no
	// turn at this index.
	A *Turn `json:"a,omitempty"`
	B *Turn `json:"b,omitempty"`
}

// Result is the outcome of a comparison.
type Result struct {
	TurnsA int `json:"turns_a"`
	TurnsB int `json:"turns_b"`
	// TurnsMatched is how many leading turns behaved identically.
	TurnsMatched int `json:"turns_matched"`
	// Divergence is nil when both runs behaved identically throughout.
	Divergence *Divergence `json:"divergence,omitempty"`
}

// Identical reports whether the two runs behaved the same the whole way.
func (r Result) Identical() bool { return r.Divergence == nil }

// TurnsOf extracts the assistant turns from a session, in order, including
// those taken by delegated sub-agents.
//
// Only assistant messages are turns: user messages are inputs and tool messages
// are results, neither of which is a decision the model made. A nil session
// yields no turns.
func TurnsOf(sess *session.Session) []Turn {
	var turns []Turn
	appendTurns(&turns, sess, 0)
	return turns
}

// maxSubSessionDepth bounds the sub-session walk. Delegation nests only a few
// levels in practice; the bound exists so a cyclic or pathological session
// cannot recurse without end.
const maxSubSessionDepth = 32

func appendTurns(turns *[]Turn, sess *session.Session, depth int) {
	if sess == nil || depth > maxSubSessionDepth {
		return
	}

	// MessagesSnapshot copies under the session lock, so a live session being
	// written to cannot race this walk.
	for _, item := range sess.MessagesSnapshot() {
		if item.IsSubSession() {
			appendTurns(turns, item.SubSession, depth+1)
			continue
		}
		if !item.IsMessage() {
			continue
		}

		msg := &item.Message.Message
		if msg.Role != chat.MessageRoleAssistant {
			continue
		}

		turn := Turn{
			Index:   len(*turns),
			Agent:   item.Message.AgentName,
			Content: msg.Content,
		}
		for _, tc := range msg.ToolCalls {
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		*turns = append(*turns, turn)
	}
}

// Compare walks two runs' turns and reports the first behavioural divergence.
func Compare(a, b []Turn) Result {
	result := Result{TurnsA: len(a), TurnsB: len(b)}

	shorter := min(len(a), len(b))
	for i := range shorter {
		if sameBehaviour(a[i], b[i]) {
			result.TurnsMatched++
			continue
		}
		result.Divergence = &Divergence{
			Kind:      KindToolCalls,
			TurnIndex: i,
			A:         &a[i],
			B:         &b[i],
		}
		return result
	}

	// The common prefix behaved identically; one run may have continued.
	switch {
	case len(b) > len(a):
		result.Divergence = &Divergence{Kind: KindExtraTurn, TurnIndex: shorter, B: &b[shorter]}
	case len(a) > len(b):
		result.Divergence = &Divergence{Kind: KindMissingTurn, TurnIndex: shorter, A: &a[shorter]}
	}
	return result
}

// CompareSessions is TurnsOf on both sides followed by Compare.
func CompareSessions(a, b *session.Session) Result {
	return Compare(TurnsOf(a), TurnsOf(b))
}

// sameBehaviour reports whether two turns called the same tools with the same
// arguments, in the same order. Order matters: calling a tool before another is
// a different plan, even with the same set of calls.
func sameBehaviour(a, b Turn) bool {
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].Name != b.ToolCalls[i].Name {
			return false
		}
		if !sameArguments(a.ToolCalls[i].Arguments, b.ToolCalls[i].Arguments) {
			return false
		}
	}
	return true
}

// sameArguments compares two tool-call argument payloads semantically.
//
// Argument JSON is model-generated, so key order and whitespace vary run to run
// while the call means the same thing. A byte comparison would report those as
// divergences — the same nondeterminism noise that "compare behaviour, not
// prose" exists to eliminate — so equal JSON values compare equal.
//
// Falls back to string equality when either side is not valid JSON, which is
// the only honest answer for a payload with no structure to compare.
func sameArguments(a, b string) bool {
	if a == b {
		return true
	}
	var av, bv any
	if json.Unmarshal([]byte(a), &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// PrintResult writes a human-readable comparison.
func PrintResult(out io.Writer, r Result, nameA, nameB string) {
	fmt.Fprintf(out, "Comparing %s (%d turns) against %s (%d turns)\n", nameA, r.TurnsA, nameB, r.TurnsB)

	if r.Identical() {
		fmt.Fprintf(out, "\n✅ Identical behaviour across all %d turns.\n", r.TurnsMatched)
		return
	}

	d := r.Divergence
	fmt.Fprintf(out, "\n❌ First divergence at turn %d (after %d matching turn(s)).\n", d.TurnIndex, r.TurnsMatched)

	switch d.Kind {
	case KindExtraTurn:
		fmt.Fprintf(out, "   %s stopped; %s continued with:\n", nameA, nameB)
		printCalls(out, d.B)
	case KindMissingTurn:
		fmt.Fprintf(out, "   %s continued; %s stopped. %s did:\n", nameA, nameB, nameA)
		printCalls(out, d.A)
	case KindToolCalls:
		fmt.Fprintf(out, "   %s called:\n", nameA)
		printCalls(out, d.A)
		fmt.Fprintf(out, "   %s called:\n", nameB)
		printCalls(out, d.B)
	}

	fmt.Fprintln(out, "\nEverything after this point is downstream of the divergence and is not compared.")
}

func printCalls(out io.Writer, t *Turn) {
	if t == nil {
		return
	}
	if len(t.ToolCalls) == 0 {
		fmt.Fprintln(out, "     (no tool calls — a final answer)")
		return
	}
	for i, tc := range t.ToolCalls {
		if i == maxPrintedCalls {
			fmt.Fprintf(out, "     … and %d more call(s)\n", len(t.ToolCalls)-maxPrintedCalls)
			return
		}
		fmt.Fprintf(out, "     %s(%s)\n", sanitizeForTerminal(tc.Name), truncateArgs(tc.Arguments))
	}
}

const (
	// maxArgRunes bounds one call's rendered arguments.
	maxArgRunes = 120
	// maxPrintedCalls bounds how many calls of a single turn are rendered.
	// Per-call truncation alone is unbounded in the number of parallel calls, so
	// a turn with hundreds of them could still flood the report.
	maxPrintedCalls = 10
)

// truncateArgs bounds argument text and strips terminal control characters.
func truncateArgs(args string) string {
	args = sanitizeForTerminal(args)
	runes := []rune(args)
	if len(runes) <= maxArgRunes {
		return args
	}
	return string(runes[:maxArgRunes-1]) + "…"
}

// sanitizeForTerminal removes control characters that let text redraw the
// terminal rather than appear in it.
//
// Tool arguments are model-generated and routinely carry content the agent read
// from a file or the web, so the diagnostic's own output can be shaped by the
// material being diagnosed: a carriage return plus an erase-line sequence
// rewrites the line, letting one tool call be displayed as another. The runtime
// scrubs the same characters before rendering markdown, for the same reason.
func sanitizeForTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\b', '\f', '\v':
			return ' '
		}
		// C0 controls (including ESC) and DEL.
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
