# Interrupted turns in Go small and portable

`abort()` cancels execution, not the conversation. After `send()` rejects with
`chat aborted`, another send can see the admitted user prompt, completed model
rounds and tool results. `restart()` instead begins a fresh conversation. A
reentrant send while the restart is stopping the old run still rejects.

The browser callback is not a commit point. In Chrome, aborting a **stalled**
stream after text, reasoning or a tool argument delta kept the question but no
assistant message. Go's stream reader returns cancellation instead of its
accumulator. With Chat Completions, Go can already have consumed `[DONE]` and
committed the *whole* reply when JS receives the first delta. Portable accounts
for complete records through `[DONE]` already in the same read; it does not
read ahead across an abort. Responses deltas alone don't commit an item.

An admitted tool batch remains a balanced assistant-call/tool-result exchange.
A completed handler keeps its actual result and state. Every unanswered call,
including a suspended parent transfer, gets `The tool call was canceled by the
user.` (subject to the usual result budget). This is a history record, not a
handler, grant or tool event. A handoff stays with its new agent. Only an applied
summary replaces the prefix; temporary output reminders don't become history.

## Boundaries

- A send canceled before it runs starts no I/O and adds no prompt. Go sometimes
  starts the model before returning control to the caller; portable doesn't
  emulate that race. Likewise an abort on `fallback` never starts the backup.
- Aborting at a host-backed `tool_call` prevents dispatch if it has not yet
  started. In Go, `add_memory` can already have written the memory by that
  callback. Portable does not perform the write to match the receipt.
- Within the [serial batch envelope](batch-order.md), portable keeps slot order.
  Go can publish more events before the interrupt and record pending results in
  goroutine wake order. Don't run extra calls to reproduce that order.
- Only events projected before cancellation reach the host. Aborting on the
  final `finish` or `error` still rejects the send. Session confirmations retain
  the portable-only correlation token; a late decision must not apply to a
  later invocation with the same model ID.

`interrupt*` in `replay.mjs` compares the follow-up request as well as events and
results. Node tests cover delayed host replies, callback reentry, backoff,
compaction and stale interaction IDs.
