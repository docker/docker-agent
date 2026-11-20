# Remote MCP in the browser replay

The `mcp*` and `args_mcp_*` cases compare what the provider, host and MCP
server receive. The server trace records the raw POST body (including RPC IDs),
headers and which model round was in progress. Fixtures use no real credentials.

- Streamable HTTP tries `server/discover` before `initialize`, within the
  discovery deadline and response budget. For a stateless `2026-07-28` server,
  the version, implementation and client capabilities accompany each request.
  OAuth and legacy SSE keep their existing handshake. Portable does not open a
  subscription stream or mirror arbitrary arguments into `Mcp-Param-*` headers.
- Tool schemas are normalized for OpenAI, not used to validate the model's
  arguments. The Responses `strict` decision uses the original schema, before
  optional properties become required and nullable. A `$ref` never triggers a
  fetch. MCP tool names and approvals still pass through the local catalog.
- An empty argument string sends `{}`; JSON `null` sends `null`. Top-level
  null-valued object members are removed; nested nulls are preserved. A
  decoder error is a tool result and sends no `tools/call`.
- The text projection joins adjacent non-whitespace blocks with a newline.
  `structuredContent` does not substitute for absent text (`no output`).
  Non-text media and binary resources are not silently discarded.
- An SSE response can carry a ping, roots request, sampling request, or host
  elicitation before the tool result. The roots list is empty; sampling does
  not start a model call. A decline or cancel never transmits host content.

Portable does not retry an uncertain invocation, even if the peer returns a
session 404. A changed catalog requires reopening the session. Discovery is
eager within the first use of each profile, only advertised tools are listed,
and stateful sessions get a best-effort DELETE on close. Go can refetch an
empty list, reconnect, or let a turn continue without a failed toolset.

Response sizes, UTF-8 checks, fixed origins, credential handling and approval
requirements also apply to the new protocol. Errors whose text could contain a
session ID, echoed credential or an HTTP error page are not reproduced verbatim.
