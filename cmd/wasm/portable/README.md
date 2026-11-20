# Portable browser agent (Rust/Wasm)

An independent port of a **subset** of the [Go small profile](../README.md#small-profile).
The browser runs Rust's YAML loader, conversation and tool loop in Wasm. A small
ES module host handles browser I/O, cancellation, approval prompts and MCP.
There is no Go runtime, `wasm_exec.js`, WASI, server-side agent, npm dependency,
or Go binary to download. The model still runs at the configured provider.
The measured release is **1,839,728 bytes (1.75 MiB) uncompressed**, including
all nine runtime JS modules, versus 35.8 MiB for the packed Go small module
alone. CI enforces a 2 MiB budget for the Wasm and JS together.

```sh
# Requires Rust 1.90+, Cargo and Binaryen (`wasm-opt`; e.g. brew install binaryen).
rustup target add wasm32-unknown-unknown
task test-wasm-portable                 # or task build-wasm-portable
python3 -m http.server --directory bin/wasm-portable 8000
```

Open <http://localhost:8000>. The demo uses OpenRouter by default; its key stays
in the tab and is sent to the chosen model endpoint. Only use configs and model
endpoints you trust. To embed it without the demo, deploy `docker-agent.wasm` and **all `.mjs`
files** from `bin/wasm-portable/` together:

```js
import { loadDockerAgent } from './agent.mjs';
const agent = await loadDockerAgent(); // loads the adjacent docker-agent.wasm
const session = await agent.createSession({
  yaml: `agents: {root: {model: openai/gpt-4o-mini}}`,
  env: { OPENAI_API_KEY: key },
}, event => {
  if (event.type === 'delta' && event.content) appendText(event.content);
});
try { await session.send('Hi!'); } finally { await session.close(); }
```

Use a secure context (HTTPS or loopback), serve `.wasm` as `application/wasm`,
and use a provider that allows your origin through CORS.
`loadDockerAgent` also accepts module bytes or a compiled `WebAssembly.Module`.
Nothing is installed on `globalThis` unless the host chooses to do so.

| Area | Ported | Not yet ported |
|---|---|---|
| Config | Current schema: named agents/models, reusable `toolsets` / `use_toolsets`, named remote `mcps`, filters and instruction overrides, redaction, date/environment flags and supported builtin hooks. | Historical versions and filesystem-backed config sources are not part of the Go small target. Unknown options are errors. |
| Providers | Streamed OpenAI Chat Completions and Responses, native and tool-mode structured output, token caps/penalties, service tier, media history, static and selected parameterized endpoints from the Go provider catalog. Azure, Copilot, Cloudflare and session-supplied Codex credentials have separate transport handling. | No ambient login or token storage, realtime/audio generation, encrypted reasoning replay or server-hosted Responses tools. |
| Sessions | Multi-turn send, local transfers/handoffs, ordered fallbacks, abort/restart/close, stateless chat, same-model foreground compaction using a declared `context_window`, and live selection between compatible models. | No disk persistence, compaction window discovery or provider-native compaction. |
| Tools | `datetime`, `random`, `think`, `calculator`, `todo`, `plan`, `memory`, `model_picker`, `session_context`, `user_prompt`, `fetch`, `api` and remote MCP, including deferred discovery, TOON and approvals. In-memory stores survive restart but are isolated by handle. | No stdio, process or file tools. `openapi` and opt-in RAG are not part of Go small. |

For example, a custom provider can set `api_type: openai_responses` (or
`openai_chatcompletions`). The supported sampling fields are `temperature`,
`top_p`, `frequency_penalty`, `presence_penalty`, `max_tokens` and
`parallel_tool_calls`; model values override provider defaults. The wire fields
are protocol-specific. A `user_prompt` answer has the form
`{id: event.id, action: 'accept', content: {answer: 'blue'}}`; `decline` and
`cancel` need no content. Use `respondToElicitation` on the originating session.

Unsupported runtime features are rejected at session creation, even if a config
can be inspected with `parseConfig`. They do not silently weaken settings.
Tool rounds are bounded (8 per turn, 16 calls per round), and scratchpads,
answers and todo lists have portable memory limits. Parallel tool results may
appear in a different order than Go's completion-dependent scheduling.
Model requests use only explicit session credentials, never ambient environment
or cookies, and refuse redirects. Tool calls are a separate trust boundary.

For a runnable team config that both profiles accept, see
[`examples/small-team.yaml`](../examples/small-team.yaml). Paste it into the
portable demo, supply an OpenRouter key, and start a session. The analyst uses
the calculator and the session's shared plan; plan writes prompt for approval.

### Remote tools and consent

- `toolProxy` is an **operator-trusted HTTPS proxy**. It receives configured
  tool credentials and must enforce destination IP policy at dial time and never
  follow redirects. See the [Go bridge proxy contract](../README.md#javascript-api).
  Without it, protected calls fail closed. `allow_private_ips: true` is an
  explicit opt-in to direct browser access, not an automatic fallback.
- In `createSession`, handle `tool_confirmation` by calling `session.confirm({confirmationId: event.confirmationId,
  decision: 'approve'})` (or `'reject'`). The ID prevents a delayed UI response
  from approving a different call. A `chat` callback returns the decision.
  Declining must not perform the call. Only enable `autoApprove` on trusted configs.
- MCP tool catalogs are obtained over guarded HTTP. Treat server instructions
  and tool output as untrusted. The streamable HTTP client supports JSON and
  SSE replies, pagination and session teardown; it does not replay a failed tool
  invocation on a transport error. `remote.transport_type: sse` selects the
  legacy transport; it pins a same-origin POST endpoint, uses configured headers
  and does not perform OAuth or reconnect. HTML conversion in `fetch` is offline:
  no DOM or subresource requests, and unsupported markup is reported explicitly.
- For MCP OAuth set `oauthRedirectURI` to an exact, trusted HTTPS callback.
  Public clients use authorization code + PKCE S256 with resource and issuer
  binding. The host must verify the popup/message source and origin and return
  the **actual callback URL** in `content.callback_url` via `respondToElicitation`.
  Never synthesize it from an untrusted code/state pair. Unsupported servers,
  confidential clients and implicit flows are refused. Access and refresh tokens
  live only in that MCP session. If the server issues a refresh token, the next
  protected request renews a near-expiry bearer using the same resource, issuer,
  and client. Concurrent requests share that refresh. An explicit `invalid_grant`
  can prompt for PKCE again. Once a bearer has been sent, only an HTTP 401
  Bearer `invalid_token` challenge permits one replay of the same RPC and ID.
  A transport error never triggers a replay. There is no background refresh or
  refresh on close. The demo declines URL authorization.

### Structured output

`structured_output.mode: native` (the default) sends the configured JSON schema
to the provider. In `mode: tool`, the portable runtime validates the arguments
of an exclusive `__structured_output__` call against the original schema before
returning its JSON. A mixed tool batch cannot finalize a turn. The local
validator supports typed objects, arrays, string enums and length bounds, and
bounded integers. A property, array item, or root definition can be a bare
`{"$ref":"#/$defs/Name"}` pointing to another root definition. Acyclic chains
and JSON Pointer `~0`/`~1` escapes work. The definitions stay on the provider
wire and are followed locally when validating the answer; nothing is fetched
or inlined. Cycles, unresolved references, `$ref` siblings, external references,
unknown keywords, regex constraints and compositions are rejected before
inference, even in unused definitions. The graph is limited to 128 source
nodes, depth 16 and 512 visits following references. Use native mode for more
complex schemas. Tool mode cannot be combined with team routing, active history
limits, an agent iteration cap or per-tool model overrides.

### Attachments in `chat` history

Use the Go bridge's `multi_content` on **prior** messages, not OpenAI wire-format
arrays in `content`. For example, declare `capabilities: {image: true, pdf: true}`
on a named model and pass this history to `chat`:

```js
messages: [
  { role: 'user', multi_content: [
    { type: 'text', text: 'Here is the invoice.' },
    { type: 'document', document: { name: 'invoice.pdf', mime_type: 'application/pdf',
      source: { inline_data: base64PDF } } },
  ] },
  { role: 'user', content: 'What is the total?' },
]
```

- User and tool history can carry `document` with `inline_text` or canonical
  base64 `inline_data` (PNG, JPEG, GIF, WebP, PDF). WAV and MP3 are supported
  with `audio: true` on **Chat Completions only**. Text documents need no
  capability flag. As in Go small, the model catalog is empty; do not rely
  on a model name to enable binary documents. Undeclared modalities and audio
  on Responses are not sent. System and assistant history replay text, not files.
- Legacy `image_url` parts accept HTTPS URLs without credentials/fragments or
  base64 raster image data URLs. URLs are sent to the **provider**; the browser
  does not fetch, inspect or upload remote files. Tool replies stay grouped
  before their attachment messages (Chat), or retain the attachments under the
  corresponding `function_call_output` (Responses).
- `session.send` and the final user prompt are text-only. Portable rejects
  final-prompt attachments rather than silently dropping them. `developer` is
  not a Go `chat.Message` role; use `system`. No `file_id`, paths, artifacts,
  video, direct `input_audio` parts or audio output.
- Portable validates base64 and binary signatures (not full file integrity).
  Use an ASCII basename, a MIME type without parameters, and at most 5 MiB per
  attachment and 16 MiB total per history (including text-envelope overhead),
  1,024 parts and 4,096 messages. HTTPS image URLs are limited to 8 KiB.

For a single agent with `fallback.models`, multipart history is replayable when
**every** target uses the same API (Chat Completions or Responses) and the same
effective media capabilities, including model-name handling of legacy image
URLs. Each attempt still uses its own endpoint and credentials. Multipart
history with a routed team or an incompatible fallback chain is rejected before
the first model request.

### Routing and tool policy

Local `sub_agents` run `transfer_task` with fresh history and return their
final answer to the caller. A `handoff` keeps history and switches the acting
agent, its instructions and tools. `force_handoff` routes on natural completion
without advertising a switch tool; missing targets and cycles are errors.
`fallback.models` can name other providers. The host binds each attempt's
body to that model's transport and credentials. It retries only before visible
output; it never retries a completed tool action to recover a model stream.
`session_compaction` uses a foreground summary when a declared `context_window`
and threshold warrant it. Summary requests honor the same outbound secret
redaction as ordinary model requests, including an explicit `before_llm_call`
redaction hook. Provider-native and alternate-model compaction are not implemented.
Picker/toolset model overrides must be transport-compatible. `session_context`
reads only the originating handle's previous conversations and is unavailable
inside a routed team. Teams reject readonly-to-writable routing and disabling
secret redaction. Combining team/fallback routing with active compaction or
per-tool model/history limits is not supported. In particular, Go can use its
primary model to summarize while its fallback is serving the conversation;
equal window sizes alone do not make those routes interchangeable. Teams share
plan and memory within one handle, but each acting agent gets its own
authorized tool catalog, MCP connection, credentials and grants. Teams are
limited to ten nested transfers and 32 switches per user turn.

`num_history_items`, `max_old_tool_call_tokens` and `max_tool_result_tokens`
control the stored or transmitted history; they do not truncate a tool event
just to hide it from the host. Agent `max_iterations` finishes normally at its
limit, while the `max_iterations` builtin hook rejects the run. Identical
tool batches are checked against `max_consecutive_tool_calls` after execution;
successful random and datetime reads are exempt, as in Go.

The tool registry applies `tools`, `readonly`, and explicit denials before
exposing a tool. `defer` provides `search_tool`/`add_tool`; activation takes
effect on the next model call. Selective `toon` encoding is applied to tool
results, not to credentials or wire protocol messages. Within an admitted
batch, every call (including Rust builtins) goes through a permission decision
before a side effect. The portable policy accepts bounded ASCII name globs and
string-argument conditions; explicit asks remain mandatory even with
`autoApprove`, and unsupported checks deny rather than allow. An untrusted MCP
server cannot self-authorize a write by declaring its tool read-only.

Supported builtin hooks are `add_date`, `add_environment_info`, `max_iterations`,
`limit_large_tool_results` and `redact_secrets`. Secret redaction defaults on;
there is no process environment lookup and no hook can run a command. The
scanner catalog is regenerated from the pinned Go dependency with
`sh cmd/wasm/portable/gen/redaction/generate.sh`. Resource limits and matcher
syntax are intentionally bounded. As in Go, raw model stream deltas are not a
redaction boundary; avoid logging untrusted stream data.

## Keeping up with docker-agent

The Go small profile remains the reference. This port is source code, not a
transpiler: update the corresponding Rust module when Go behavior changes.
`src/config.rs` implements the config projection and provider selection;
`src/turn.rs` owns prompt history and tool execution; `src/responses.rs`
adapts that loop to the Responses wire protocol.
`src/lib.rs` exposes a synchronous JSON command boundary; `agent.mjs` handles
browser I/O. A reference change that isn't ported should remain explicitly
unsupported rather than be silently accepted.

Add a fixture to `../node/small_test.js` for behavior both runtimes should
share. Run it against both:

```sh
task build-wasm-small && node --test cmd/wasm/node/{small,config_projection}_test.js
task test-wasm-portable
```

The latter also runs Rust unit tests and a real HTTP/SSE loop against a local
mock provider, checks for zero Wasm imports and enforces a 2 MiB uncompressed
budget for the Wasm **plus** its JS host. Neither test requires model credentials.

### Differential replay

`task test-wasm-parity` builds both binaries and runs the same fixtures in two
fresh headless Chrome profiles. Set `CHROME_BIN` if Chrome isn't on the default
path. A scripted `fetch` supplies identical model streams; neither runtime can
contact a provider. The test compares every model request body (including tool
schemas, prompts, and follow-up history), endpoint path, authorization header,
ordered JS events and turn results. Only generated correlation IDs are renamed;
missing vs. empty fields and array order are significant. The traces are in
`bin/parity-go.json` and `bin/parity-rust.json` for inspection.

Add paired fixtures in `parity/replay.mjs` when porting a feature. Refresh
Go-derived builtin prompts and schemas with `task generate-wasm-portable-tools`;
the generator's Go test detects stale definitions. Passing replay establishes
parity for the scenarios covered, not for every possible agent configuration.
