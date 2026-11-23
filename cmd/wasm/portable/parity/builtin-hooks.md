# Builtin hooks and prompt options in Go small

`redactCatalog` sends one synthetic example per pinned Portcullis rule. The
request retains surrounding text and replaces each matched span with
`[REDACTED]`. An invalid checksum need not be a match. Overlapping matches get
one marker; adjacent, disjoint matches get separate markers.

| Boundary | Browser behavior |
| --- | --- |
| `tool_input_transform` | Walk decoded argument **values**, not object keys. A change produces one `warning` before approval and `tool_call`. |
| `tool_response_transform` | Scrub the rendered result, including TOON, before `tool_result` and storage. The large-result hook limits MCP output to a UTF-8 tail with a notice (50 KiB or over 2,000 lines). If a later redaction grows the output past the byte cap, the dispatcher's byte backstop adds a different notice. |
| `before_llm_call` | Scrub the request view, including instructions, replayed assistant text, tool arguments as **JSON source**, and summary requests. This is not a filter on provider replies, results of `send`, or their streamed deltas. |
| Tool token limits | Estimate with `floor(UTF-8 bytes / 4)`. `max_tool_result_tokens` keeps head and tail in history, not the event. `max_old_tool_call_tokens` spends a request budget newest first, replacing over-budget results with `[content truncated]`. |
| Date and environment | The environment (cwd `/`, OS `js`, arch `wasm`) precedes the date after static instructions. Dates follow the acting agent, once per model round, not per retry. The environment follows the opener of the send or delegated run. |
| Iteration guards | The agent cap returns a stop notice as assistant text; the builtin rejects before the next model call. The consecutive-call guard counts equivalent ordered batches, not tool IDs or JSON spelling. A new send gets a fresh count and the completed tool history. |

Portable keeps redacting non-object tool inputs, credentials in diagnostics,
image URLs and ambiguous JWT/Docker config candidates. It need not reproduce a
Go false negative. For inline text documents the scanner runs before making an
envelope tag and again on the outgoing view. If rewriting could restore text
hidden by delimiter removal or by a projected match, the request is refused.

Only simple top-level `${env.NAME}` references in instructions and team
roster descriptions expand; missing variables become empty. Braces inside
quoted strings (including backticks) do not end an expression. A welcome
message and an agent's own description are not system instructions. Go's
browser chat does not substitute configured commands for `/name` prompts.
Portable rejects commands, skills and local prompt file configuration rather
than suggesting it provides command execution, skill loading or a filesystem.
Agent hooks support the documented event subset and literal tool names or `*`;
there is no top-level `hooks` section in the Go config.

Stream deltas are not a safe logging boundary. In Go small an explicit
`pre_tool_use` scrubber can be bypassed by auto-approval, provider error text
can echo credentials, and running an explicit output limiter **before**
redaction can retain an unrecognizable suffix of a secret at the cut. The
last ordering affects both runtimes; prefer the default redact-then-limit order.
