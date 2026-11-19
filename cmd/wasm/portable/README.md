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
| Providers | OpenAI Chat Completions **and** streamed Responses (text/function calls). Explicit `api_type` or model-based selection; sampling fields, token caps, parallel tool flag; static HTTPS API-key aliases from the Go provider catalog, e.g. Groq and OpenRouter; ordered fallbacks and per-agent cooldown | Realtime, encrypted reasoning state, multimodal/hosted tools, OAuth/dynamic provider endpoints, cost/catalog limits |
| Sessions | `createSession`, multi-turn `send`, `abort`, `restart`, `close`; stateless `chat` with history; bounded local `sub_agents` and `handoffs`. Host documents are validated but not sent or indexed. | Remote agents, compaction, persistence |
| Tools | `think`, exact rational `calculator`, session-local `todo`, `user_prompt`, `model_picker` and `session_context`. Single-agent sessions can also use guarded HTTP and MCP tools. Sessions answer questions by ID with `respondToElicitation`; `chat` awaits the callback's answer. No callback means decline. | Hosted tools in local teams, filesystem, shell and optional RAG |

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
A model call uses the explicit session
credentials, never ambient environment or cookies, and does not follow redirects.

### Model routing and tool isolation

A transfer starts a fresh child history and returns its answer as a tool result.
A handoff keeps the history but replaces the active instructions and tools.
Only declared local edges are callable. A switch must be the only call in its
batch; a turn allows 32 switches and 10 nested transfers.

- Configure `fallback: {models: [backup], retries: 2, cooldown: 1m}` on the
  agent that owns the chain. Retries and fallback are allowed before output on
  the first model request of a turn (including a new transfer child), not on a
  tool follow-up or an in-place handoff. A successful backup after a permanent
  primary failure is pinned until cooldown expires. Each frame resumes its own
  target; each target uses its own API, parameters, endpoint and credential.
- A `model_picker` choice must keep the agent's provider, endpoint, API and
  token key. A picker and a fallback model list on **the same agent** are not
  supported. A parent picker and a child's fallback chain can be used together.
- Hosted tool registrations, approvals and `session_context` archives belong to
  a session, not an agent. If any reachable local agent uses a hosted toolset or
  `session_context`, a team session is rejected before model or discovery I/O.
  Single-agent sessions retain host tools across model fallback; completed tool
  calls and elicitation answers are not re-executed.
- All reachable model endpoints are validated when the session opens. A model
  in a team or fallback chain that overrides its provider's origin must specify
  its own `token_key` rather than redirect an inherited credential. When
  switching to Responses, history that cannot be replayed losslessly is rejected.

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
