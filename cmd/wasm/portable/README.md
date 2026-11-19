# Portable browser agent (Rust/Wasm)

An independent port of a **subset** of the [Go small profile](../README.md#small-profile).
The browser runs Rust's YAML loader, conversation and tool loop in Wasm. A small
ES module host handles browser I/O, cancellation, approval prompts and MCP.
There is no Go runtime, `wasm_exec.js`, WASI, server-side agent, npm dependency,
or Go binary to download. The model still runs at the configured provider.
CI enforces a 2 MiB uncompressed budget for the Wasm **and all runtime JS**
(the packed Go small module alone is 35.8 MiB on the reference toolchain).

```sh
rustup target add wasm32-unknown-unknown  # Rust 1.90+ and Cargo
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
| Providers | Streamed OpenAI Chat Completions and Responses, native JSON-schema output, token caps/penalties, service tier, media history, static and selected parameterized endpoints from the Go provider catalog. Azure, Copilot, Cloudflare and session-supplied Codex credentials have separate transport handling. | No ambient login or token storage, realtime/audio generation, encrypted reasoning replay or server-hosted Responses tools. |
| Sessions | Multi-turn send, abort/restart/close, stateless chat, same-model foreground compaction using a declared `context_window`, and live selection between compatible models. | No disk persistence, compaction window discovery or provider-native compaction. |
| Tools | `datetime`, `random`, `think`, `calculator`, `todo`, `plan`, `memory`, `model_picker`, `session_context`, `user_prompt`, `fetch`, `api` and remote MCP. In-memory stores survive restart but are isolated by handle. | No stdio, process or file tools. `openapi` and opt-in RAG are not part of Go small. |

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

### Remote tools and consent

- `toolProxy` is an **operator-trusted HTTPS proxy**. It receives configured
  tool credentials and must enforce destination IP policy at dial time and never
  follow redirects. See the [Go bridge proxy contract](../README.md#createsessionoptions-onevent---promisesession).
  Without it, protected calls fail closed. `allow_private_ips: true` is an
  explicit opt-in to direct browser access, not an automatic fallback.
- In `createSession`, handle `tool_confirmation` by calling `session.confirm({confirmationId: event.confirmationId,
  decision: 'approve'})` (or `'reject'`). The ID prevents a delayed UI response
  from approving a different call. A `chat` callback returns the decision.
  Declining must not perform the call. Only enable `autoApprove` on trusted configs.
- MCP tool catalogs are obtained over guarded HTTP. Treat server instructions
  and tool output as untrusted. The streamable HTTP client supports JSON and
  SSE replies, pagination and session teardown; it does not replay a failed tool
  invocation on a transport error.
- For MCP OAuth set `oauthRedirectURI` to an exact, trusted HTTPS callback.
  Public clients use authorization code + PKCE S256 with resource and issuer
  binding. The host must verify the popup/message source and origin and return
  the **actual callback URL** in `content.callback_url` via `respondToElicitation`.
  Never synthesize it from an untrusted code/state pair. Unsupported servers,
  confidential clients and implicit flows are refused. Tokens are in memory only;
  there is no background refresh. The simple demo declines URL authorization.

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
