# Tool batches in the browser replay

The reference is the Go **small** build in Chrome, not a native Go process.
Compare both the events and the tool messages in the *next request*. The
assistant's own `tool_calls` array stays in model order.

- Start calls FIFO. A pending approval does not stall already-authorized work.
  Only one confirmation can be outstanding. Its reply wakes one waiter, which
  checks for a permitted `approve_tool` grant before asking again.
- An unavailable call and a Go handler error both write to stderr. With the
  synchronous `wasm_exec.js` filesystem shim the writer does not return until
  the scheduler is idle. The writers complete in arrival order. A denial, a
  redaction warning event, and a tool-level error do **not** imply this yield.
- A plain `chat()` reply can wake its call before those writers finish. A
  thenable or an explicit session reply needs another JS turn. Elicitation
  requests reach the stream via a separate Go goroutine, after initial calls
  have run to their first wait.
- Local host tools (`memory`, `plan`, `datetime`, `random`) are atomic like
  their Go handlers. Simple direct API/fetch calls can be in flight together;
  their results are recorded as they settle. No host action starts before its
  own permission check.

**Limits.** Batches containing a model picker call retain serial dispatch:
multiple INFO writes can interleave with other writers, and repeated Go runs
have not produced one order. MCP, non-simple HTTP calls, and network calls
with more than one potential interaction also use serial dispatch. A network
call can fail synchronously before I/O, MCP can itself ask a question, and
multiple callback settlements can race with transport-specific promise chains.
`simple_network_call` is a conservative scheduling hint, not an admission or
authorization check. A dispatched team switch still has to be the only call.

`batch*` in `replay.mjs` exercises these rules with deterministic outputs.
Use `delete_memory` to exercise approval without introducing wall time: Go's
in-memory `add_memory` uses a timestamp as the ID and two additions in the
same browser millisecond can collide.
