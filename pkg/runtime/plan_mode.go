package runtime

import (
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// planModeReminder is the per-turn system instruction injected when a session
// is in plan mode. Two layers enforce plan mode: the runtime hides every
// non-read-only tool from the model (see filterToolsForSession in loop.go),
// and this reminder tells the model how it should behave. Hiding the tools
// is the hard guarantee; the reminder is the explanation, so the model
// produces a useful plan instead of just bouncing off missing tools.
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

// planModeReminderMessages returns the system-reminder messages to splice
// before the conversation history when sess is in plan mode. Returns nil for
// other modes so callers can use it unconditionally.
func planModeReminderMessages(sess *session.Session) []chat.Message {
	if sess == nil || sess.Mode != session.ModePlan {
		return nil
	}
	return []chat.Message{{
		Role:    chat.MessageRoleSystem,
		Content: planModeReminder,
	}}
}
