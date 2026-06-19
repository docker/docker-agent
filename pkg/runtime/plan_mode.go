package runtime

import (
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// planModeReminder is the per-turn system instruction injected when a session
// is in plan mode and the active agent has not declared a plan persona. Two
// layers enforce plan mode: the runtime hides every non-read-only tool from
// the model (see filterToolsForSession in loop.go), and this reminder tells
// the model how it should behave. Hiding the tools is the hard guarantee; the
// reminder is the explanation, so the model produces a useful plan instead
// of just bouncing off missing tools.
const planModeReminder = `<system-reminder>
You are currently in PLAN MODE.

In this mode you research the codebase, ask clarifying questions, and write a
clear, actionable plan for the user. You MUST NOT make any changes to the
system:

- No edits to files (no write, edit, create, or delete).
- No shell commands or background jobs.
- No state-changing tool calls of any kind.

Only read-only tools have been made available to you for this turn. If you try
to call a tool that isn't in your list, the user has explicitly disabled it
for planning.

End the turn by presenting the plan in your final message and asking the user
to review it. The user will switch you to BUILD MODE when they want execution
to begin.
</system-reminder>`

// planPersonaGuardrail is the short preamble the runtime prepends to a
// declared plan persona. The persona owns the workflow framing and tone; the
// guardrail owns the read-only contract so persona authors don't have to
// repeat it (and can't accidentally drop it).
const planPersonaGuardrail = `You are currently in PLAN MODE. Only read-only tools have been made available to you for this turn; every state-changing tool has been filtered out by the runtime.`

// planModeReminderMessages returns the system-reminder messages to splice
// before the conversation history when sess is in plan mode. Returns nil for
// other modes so callers can use it unconditionally.
//
// When the active agent has declared a plan persona (see
// [latest.PlanPersonaConfig]), the persona's instruction is wrapped in a
// <system-reminder> envelope and prefixed with [planPersonaGuardrail] so
// persona authors own the workflow framing while the runtime keeps the
// read-only contract intact. When no persona is declared, the canned
// [planModeReminder] is used unchanged — preserving today's behaviour for
// agents that haven't opted in.
func planModeReminderMessages(sess *session.Session, a *agent.Agent) []chat.Message {
	if sess == nil || sess.Mode != session.ModePlan {
		return nil
	}
	return []chat.Message{{
		Role:    chat.MessageRoleSystem,
		Content: planModeReminderContent(a),
	}}
}

// planModeReminderContent picks the right reminder body for the active
// agent: the agent's declared plan persona (wrapped in the runtime's
// guardrail envelope) when set, otherwise the canned reminder.
func planModeReminderContent(a *agent.Agent) string {
	if a == nil {
		return planModeReminder
	}
	persona := strings.TrimSpace(a.PlanInstruction())
	if persona == "" {
		return planModeReminder
	}
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString(planPersonaGuardrail)
	b.WriteString("\n\n")
	b.WriteString(persona)
	b.WriteString("\n</system-reminder>")
	return b.String()
}
