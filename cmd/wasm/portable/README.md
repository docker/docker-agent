# Portable browser agent (Rust/Wasm)

An independent port of a **subset** of the [Go small profile](../README.md#small-profile).
The browser runs Rust's YAML loader, conversation and tool loop in Wasm. A small
ES module handles `fetch`, SSE framing, cancellation and the JavaScript API.
There is no Go runtime, `wasm_exec.js`, WASI, server-side agent, npm dependency,
or Go binary to download. The model still runs at the configured provider.
The module and its JS host together are about 450 KiB uncompressed (Rust
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
and use a Chat Completions provider that allows your origin through CORS.
`loadDockerAgent` also accepts module bytes or a compiled `WebAssembly.Module`.
Nothing is installed on `globalThis` unless the host chooses to do so.

| Area | Ported | Not yet ported |
|---|---|---|
| Config | Current schema subset, YAML `parseConfig` / `listAgents`, named agents and model aliases, explicit agent selection, session-env placeholders | General Go config loader, advanced model options, reusable groups, permission policies and hooks |
| Providers | OpenAI-compatible `/chat/completions`, custom `providers:` base URL and token key, streamed text and token usage | Responses API, realtime, retries/fallbacks, other SDKs, cost/catalog data |
| Sessions | `createSession`, multi-turn `send`, `abort`, `restart`, `close`; stateless `chat` with history and `abort` | Delegation/handoffs, compaction, persistence |
| Tools | Streamed function calls, local `think` scratchpad, tool results sent back to the model | Other builtins, MCP, OAuth, approvals/elicitation and RAG. `confirm` and `respondToElicitation` reject when nothing is pending. |

Unsupported config is an error, not a request to run with weaker settings.
There is no network/file/command tool. A model call uses the explicit session
credentials, never ambient environment or cookies, and does not follow redirects.

## Keeping up with docker-agent

The Go small profile remains the reference. This port is source code, not a
transpiler: update the corresponding Rust module when Go behavior changes.
`src/config.rs` implements the config projection and provider selection;
`src/turn.rs` owns prompt history, provider requests and tool responses.
`src/lib.rs` exposes a synchronous JSON command boundary; `agent.mjs` handles
browser I/O. A reference change that isn't ported should remain explicitly
unsupported rather than be silently accepted.

Add a fixture to `../node/small_test.js` for behavior both runtimes should
share. Run it against both:

```sh
task build-wasm-small && node --test cmd/wasm/node/small_test.js
task test-wasm-portable
```

The latter also runs Rust unit tests and a real HTTP/SSE loop against a local
mock provider, checks for zero Wasm imports and enforces a 2 MiB uncompressed
budget for the Wasm **plus** its JS host. Neither test requires model credentials.
