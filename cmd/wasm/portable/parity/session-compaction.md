# Session compaction

A declared `context_window` enables threshold checks. Without one the agent
can still run: it doesn't compact proactively and reports a context limit of
zero. The summary never uses the ordinary tool catalog or output schema, even
when the same provider serves both calls. Outbound redaction applies to the
summary, its tool-call replies and the resumed conversation.

| When | What the browser does |
| --- | --- |
| The provider rejects the context | Try remaining fallbacks first. On exhaustion, warn, compact and retry once. A successful ordinary call (including a tool round) resets that allowance. Only the failed step is discarded, not completed tools or already streamed deltas. |
| The summary window is unknown | Warn a second time and retry unchanged. A second overflow surfaces the provider-classified error. |
| A complete summary is blank | Skip it. The next ordinary call gets the full history. After a tool round or overflow, the next iteration still checks the threshold. |
| The summarizer calls a tool | Send an unavailable-tool result back to the **summarizer**. Nothing is dispatched or reported as an ordinary tool event. Only complete final text can replace history. |
| The summary fails | Report the error. Drain a text continuation without streaming it; its answer is in the next send's history. Even if the agent advertises tools, no hidden tool call can execute. In a delegate, stop rather than silently resuming its caller. |

The summary starts a new model-facing prefix; stored system messages in the
removed prefix are not reinserted. Agent instructions and the kept suffix
remain. A blank ordinary reply can still resolve to an assistant answer from
before compaction, as it does in Go. Summary usage is not ordinary turn usage.

Compatible tool-free fallback chains, a single agent with a dedicated summary
model (including ordinary tools), and isolated leaf delegates support recovery.
Compacting handoff frames remain unsupported. Bounded/model-override runs,
a `session_context` archive, a missing conversation prefix, and projections
that lose seed token estimates do not gain known-window overflow recovery.
Simple user text/image parts that survive projection retain Go's byte estimate
and 1,500-token charge per image; routed multipart compaction is still
unsupported.

Two intentional limits: do not apply unfinished, refused, truncated or
ambiguous output, including tool narration followed by a blank final reply.
Stop before a fifth summary tool round rather than risk a hidden loop. A
continuation that requests a tool after a summary error still rejects the send;
Go can run that tool after its host has stopped seeing events.
