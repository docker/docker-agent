# Chat Completions stream parity

The `chat*` browser replays compare the first request, streamed events, result,
and the request after a tool result or second user turn. `chatRequest_*` forces
Chat Completions even for families that normally select Responses. See [model
requests](model-requests.md) for explicit `thinking_budget`, provider defaults
and reasoning replay.

| Field | Interpretation |
| --- | --- |
| `choices` | Read the first array element, not the element whose `index` is zero. Missing or empty choices are common on usage trailers. |
| `delta.content` | Null and empty emit nothing. An array is the SDK's **raw JSON text**, not a list of text parts to concatenate. |
| Reasoning | First nonempty string in `reasoning_content`, then `reasoning`. Emit whitespace deltas but omit wholly blank reasoning from the next request. |
| Function tools | A nonempty name replaces the previous name; arguments append. Announce the first name (even an unknown prefix), then new argument bytes only. Dispatch in first-seen order, not numeric slot order. |
| Usage | Choice chunks carry cumulative snapshots; a stop, length, or refusal without usage waits for the trailer or EOF. Missing/null counts are zero. A `tool_calls` finish still drains the stream: a later error vetoes execution. |

Portable does not repair ambiguous input into an executable call. Missing or
changing IDs, duplicate routing fields, a renamed call to an available tool,
malformed names, and alternate choices that could change call identity or
termination reject. A top-level error (including `error: null`) fails the
attempt. Provider diagnostics with credentials stay withheld.

A clean EOF can finish text without `[DONE]`; a partial SSE record, incomplete
call, or call without `[DONE]` rejects. Unlike Go, an early usage trailer does
not discard later text or authorize a call. Non-final text beside a tool
fragment remains visible. XML in assistant text is never a tool instruction.
