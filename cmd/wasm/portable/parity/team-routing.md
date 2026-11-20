# Team routing and model selection

A transfer starts a new conversation with a task system message and the user
message `Please proceed.` It returns the delegate's last retained assistant text,
trimmed. A handoff instead keeps the conversation, replaces the acting agent's
instructions and tools, and leaves a handoff note in history. `agentName` selects
the entry agent; it does not pin the conversation there.

- The repetition guard counts a transfer **after the child returns**. Successful
  and refused switches count. A handoff stopped by repetition or by the frame's
  iteration cap is visible on the next send, not before the stop.
- Compatible picker choices belong to the agent on that handle. A new task for
  the same agent gets fresh messages but keeps its selected model. Restart
  clears messages, not the selection. Revert always restores the configured
  model, even if nothing was picked.
- A new routed model round checks the acting agent's fallback cooldown after
  tool results. Only the new model request can retry; none of the calls that
  produced its history are dispatched again. Portable does not retry a stream
  that already exposed output.

**Intentional differences from Go small:**

- `approve_tool` is scoped to an agent **and** a conversation. A delegate can
  reuse an existing grant for that very agent, not a different agent's grant
  for a same-named tool. Grants made in the child don't survive its return.
  Portable team confirmations identify the agent; session confirmations also
  carry a token so a late approval cannot authorize the next call.
- A terminal delegate error stops the send. Go can make another model request
  after the event bridge has stopped reporting work. In Chrome that request
  called `think` and changed the caller's scratchpad without a host tool event.
  Portable neither runs that continuation nor forwards the child's diagnostic
  to the caller's model.
- A dispatched switch must be the only dispatched call in its reply. Portable
  also enforces its 32-switch budget, including forced handoffs; Go continued
  through a 33rd switch in the browser probe.

Teams still reject active compaction, a picker combined with a fallback chain
on the same agent, and picker choices that change transport or compaction
policy. After a context overflow, Go may compact even without a declared
window; portable does not start an unmodeled recovery round.
