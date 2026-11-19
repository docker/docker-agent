# Portable browser agent (Rust/Wasm)

An independent port of a **subset** of the [Go small profile](../README.md#small-profile).
The browser runs Rust's YAML loader, conversation and tool loop in Wasm. A small
ES module handles `fetch`, SSE framing, cancellation and the JavaScript API.
There is no Go runtime, `wasm_exec.js`, WASI, server-side agent, npm dependency,
or Go binary to download. The model still runs at the configured provider.
The module and its JS host together are about 747 KiB uncompressed (Rust
1.90, release build), versus 35.8 MiB for the Go small module alone.

```sh
rustup target add wasm32-unknown-unknown  # Rust 1.90+ and Cargo
task test-wasm-portable                 # or task build-wasm-portable
python3 -m http.server --directory bin/wasm-portable 8000
```

Open <http://localhost:8000>. The demo uses OpenRouter by default; its key stays
in the tab and is sent to the chosen model endpoint. Only use configs and model
endpoints you trust. To embed the same two runtime files without the demo:

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
| Config | Current-version config projection and named agents/models. `parseConfig`/`listAgents` can inspect local sub-agent and handoff references without starting a session. Instructions, model names and endpoints expand against the session env. | Historical versions, general Go config loading, reusable groups, permission policies and hooks |
| Providers | OpenAI Chat Completions **and** streamed Responses (text/function calls). Explicit `api_type` or model-based selection; sampling fields, token caps, parallel tool flag; static HTTPS API-key aliases from the Go provider catalog, e.g. Groq and OpenRouter | Realtime, encrypted reasoning state, multimodal/hosted tools, OAuth/dynamic provider endpoints, retries/fallbacks, cost/catalog limits |
| Sessions | `createSession`, multi-turn `send`, `abort`, `restart`, `close`; stateless `chat` with history. Host documents are validated but not sent or indexed. | Executing delegations/handoffs, compaction, persistence |
| Tools | `think`, exact rational `calculator`, session-local `todo`, and `user_prompt`. Sessions answer questions by ID with `respondToElicitation`; `chat` awaits the callback's answer. No callback means decline. | Other toolsets, remote MCP, tool approvals, OAuth elicitation and optional RAG |

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
There is no network/file/command tool. A model call uses the explicit session
credentials, never ambient environment or cookies, and does not follow redirects.

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
