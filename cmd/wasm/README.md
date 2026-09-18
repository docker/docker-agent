# docker-agent in the browser (js/wasm)

`cmd/wasm` is a `GOOS=js GOARCH=wasm` entry point that exposes docker-agent
to a JavaScript host (a browser tab or Node): config parsing, and long-lived
agent sessions running the **same `pkg/runtime` the CLI uses**, assembled
through `pkg/embeddedchat` with browser-safe registries.

It is not a port of the full CLI: everything that needs a host process, a
filesystem or a socket is left out, and configs that ask for it are rejected
when the session is created. See *Limits*.

## Build

```sh
# Compile.
GOOS=js GOARCH=wasm go build -o cmd/wasm/web/docker-agent.wasm ./cmd/wasm

# Copy the matching wasm_exec.js shim from the Go toolchain (its API is
# tied to the compiler version and must match the binary).
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" cmd/wasm/web/wasm_exec.js
```

The output is a large `.wasm` (the runtime, the YAML parser, three LLM
provider clients, RAG and the MCP client). Size depends on the toolchain
and optional providers; compression and `-ldflags="-s -w"` can reduce transfer
size. No size-reduction ratio is guaranteed.

### Optional cloud providers

The default binary keeps the original OpenAI / Anthropic / Gemini API set;
a `google` model with `project`, `location`, `publisher` or
`GOOGLE_GENAI_USE_VERTEXAI` is rejected at session creation.
To include Bedrock, Vertex AI and explicitly configured Docker Model Runner:

```sh
GOOS=js GOARCH=wasm go build -tags docker_agent_wasm_cloud \
  -o cmd/wasm/web/docker-agent.wasm ./cmd/wasm
```

- **Bedrock:** supply `AWS_BEARER_TOKEN_BEDROCK` (or the model's `token_key`)
  in the session `env`. AWS profiles, assume-role and metadata discovery are
  unavailable. Bearer authentication requires HTTPS.
- **Vertex AI:** supply `GOOGLE_OAUTH_ACCESS_TOKEN` (or `token_key`) and
  project/location. Gemini and Model Garden use the token without ADC or
  host credential files. Tokens are session inputs; the host must obtain
  fresh credentials and recreate the session when they expire.
- **DMR:** set an explicit HTTP(S) `base_url`. No CLI/socket discovery or
  model auto-pull runs. The provider still attempts its normal model
  configuration request at creation.

These providers still require CORS-enabled endpoints or a provider proxy;
`toolProxy` only routes SSRF-guarded tool traffic, not model requests.
See `examples/cloud-bedrock.yaml` and `examples/cloud-vertex.yaml`.

## Run (Node)

```sh
# Go tests of the runtime bridge, with a mocked provider registry.
task test-wasm-js

# Smoke test of parseConfig/listAgents against the built binary.
node cmd/wasm/smoke_test.js

# JS bridge tests: session handles, chat(), failure modes.
node --test cmd/wasm/node/bridge_test.js
```

Both Node scripts load `web/docker-agent.wasm` through `node/boot.js`, which
picks `wasm_exec.js` from `go env GOROOT` (or `$GOROOT`).

## Browser integration test

```sh
task test-wasm-browser
# Other Chrome installations:
CHROME_BIN=/path/to/chrome task test-wasm-browser
```

Requires Chrome and `openssl`; no npm dependencies. The runner starts local
mock model and HTTPS proxy servers, tests real browser streaming, tools,
RAG, approval, cancellation and proxy redirects, then removes its temporary
profile and certificate. It does not contact model services. This validates
Chromium; Safari and Firefox are not covered.

## Run (browser, with OpenRouter sign-in)

The demo page implements OpenRouter's [PKCE OAuth flow](https://openrouter.ai/docs/use-cases/oauth-pkce):

1. Serve `cmd/wasm/web/` over HTTP (`WebAssembly.instantiateStreaming`
   needs the `application/wasm` MIME type, so `file://` won't work):

   ```sh
   cd cmd/wasm/web && python3 -m http.server 8765
   ```

2. Open <http://localhost:8765/>.

3. Click **Sign in with OpenRouter** — you'll be redirected to
   `openrouter.ai`, log in, approve the app, and bounced back. The page
   exchanges the `?code=` for a user-controlled API key (PKCE / S256) and
   stores it in `localStorage`.

4. Pick a free model from the dropdown (the YAML textarea updates
   automatically) and click **Run**. Streaming completion deltas appear in
   the output box.

5. To revoke the key: click **Sign out** in the page (clears local copy)
   and/or visit
   <https://openrouter.ai/settings/keys> to revoke server-side.

### Why this works in a browser

Most LLM providers block direct browser fetches because anyone could read
the API key out of the Network tab. OpenRouter solves this by issuing
*per-app, per-user* keys via PKCE — the user owns the key, can revoke it,
and the app never sees a master credential.

We verified the relevant CORS posture before shipping:

```
$ curl -is -X OPTIONS https://openrouter.ai/api/v1/auth/keys \
    -H "Origin: http://localhost:8765" \
    -H "Access-Control-Request-Method: POST"
HTTP/2 204
access-control-allow-origin: *
access-control-allow-headers: Authorization,...,Content-Type,...
```

Both `/api/v1/auth/keys` (token exchange) and `/api/v1/chat/completions`
(inference) return `access-control-allow-origin: *` with `Authorization`
in the allowed headers. No proxy needed.

### What the YAML looks like

OpenRouter exposes an OpenAI-compatible API, so it slots in as a custom
provider:

```yaml
providers:
  openrouter:
    provider: openai
    base_url: https://openrouter.ai/api/v1
    token_key: OPENROUTER_API_KEY
agents:
  root:
    model: openrouter/meta-llama/llama-3.3-70b-instruct:free
    instruction: |
      You are a helpful assistant ...
```

When the user clicks **Run**, the page passes the stored key as
`env.OPENROUTER_API_KEY`. That map is the *only* environment the session
sees (`RuntimeConfig.EnvProviderOverride`): nothing is copied into the
process environment, and two sessions never see each other's keys.
`pkg/model/provider/openai` reads it via `env.Get(ctx, cfg.TokenKey)`, and
the Go HTTP transport (mapped to `fetch` under js/wasm) sends the request.
**Same code path the CLI uses** — no special browser-only branch.

### Bring-your-own-key fallback

The `<details>` block on the page lets advanced users paste OpenAI /
Anthropic / Gemini keys directly. Anthropic recently added
`anthropic-dangerous-direct-browser-access` so it actually works from a
tab; OpenAI and Gemini still block CORS for browser origins, so those
fields are mostly there for use against a self-hosted proxy.

## JavaScript API

Once the wasm boots, `globalThis.dockerAgent` offers:

### `parseConfig(yamlString) -> object`

Synchronous. Loads the YAML through `pkg/config.Load` (so all the version
upgraders run) and returns a small JS-shaped projection:

```js
dockerAgent.parseConfig(`
version: "2"
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: hi
`);
// =>
// {
//   version: "2",
//   agents: [{ name: "root", model: "openai/gpt-4o-mini", instruction: "hi", ... }],
//   models: { "openai/gpt-4o-mini": { provider: "openai", model: "gpt-4o-mini" } }
// }
```

Throws a JS `Error` on invalid YAML / unsupported version / failed validation.

### `listAgents(yamlString) -> [{name, model, description, instruction}]`

Synchronous; same loading and error behaviour as `parseConfig`.

### `createSession(options, onEvent) -> Promise<session>`

Loads the config with the browser registries, builds the runtime and starts
an empty conversation. Rejects — before anything touches the network —
when the config needs something the browser cannot provide (see *Limits*),
when a model's API key is missing from `env`, or when `agentName` is unknown.

`options`:

| Field | Meaning |
| --- | --- |
| `yaml` | The YAML document, any config version. |
| `agentName?` | Agent to talk to; defaults to the config's root agent. |
| `env?` | `{OPENAI_API_KEY: "...", MCP_TOKEN: "..."}` — the session's whole environment: API keys, `${env.X}` placeholders in instructions, MCP URLs and headers. |
| `toolProxy?` | HTTPS URL of a trusted egress proxy (`httpclient.WithEgressProxy`). Browsers cannot enforce the SSRF guard on `fetch`, so remote MCP, `fetch`, `api` and `openapi` requests are routed through it and fail closed without one (unless the toolset sets `allow_private_ips: true`). |
| `oauthRedirectURI?` | `redirect_uri` advertised for MCP server OAuth flows; the flow itself is relayed to the host as an `elicitation` event. |
| `autoApprove?` | Run tool calls without asking, like `--yolo`. Otherwise calls the safety policy does not clear raise a `tool_confirmation` event. |
| `documents?` | `{"handbook/leave.md": "...", ...}` — the session's documents, keyed by logical path. They are the only thing `type: rag` toolsets can index: a RAG's `docs` select among these paths (exact path, directory prefix or glob), and a `docs` entry selecting nothing rejects the session. Values must be strings; at most 1000 documents and 16 MiB in total. See `examples/handbook-rag.yaml`. |

The trusted proxy receives the target URL in the `url` query parameter, with
the original method, body and credentials. It must enforce destination IP
policy and **must not follow redirects**. Relay target redirects with the same
status, move `Location` to `X-Docker-Agent-Location`, and expose that header
through CORS. Go then enforces the caller's redirect and credential policies.
The fetch sends no cookies (`credentials: "omit"`); the proxy must strip the
headers the browser adds on its own — `Cookie`, `Origin`, `Referer`, `Host` —
before forwarding, so the target only sees the request the tool built.
Only configure infrastructure you trust with the target credentials.

The session handle:

| Method | Behaviour |
| --- | --- |
| `send(prompt) -> Promise<{message, usage?}>` | Runs one turn: the full agentic loop with streaming, tool calls, handoffs and fallbacks. Rejects with "a run is already active" while a turn is running, with "session is restarting" while `restart()` winds the previous turn down, with "chat aborted" when cut short, or with the runtime's error. `usage` sums this turn's model calls. |
| `confirm(decision, reason?)` | Answers the pending `tool_confirmation`. `decision` is `"approve"`, `"approve_tool"` (needs `toolName`), `"approve_balanced"`, `"approve_autonomous"` or `"reject"`; also accepts `true`/`false` or `{decision, toolName?, reason?}`. Rejects with "no tool confirmation is pending" when there is nothing to answer, e.g. before any turn or once the call completed. |
| `respondToElicitation({id, action, content?})` | Answers an `elicitation` (`accept`, `decline`, `cancel`); `content` is the form payload or the OAuth `{code, state}`. |
| `restart()` | Aborts the running turn, waits for it to stop and starts a fresh conversation on the same runtime. Sends are refused until the new conversation is in place. |
| `abort()` | Cuts the running turn short; the session stays usable. Synchronous, and effective even when called right after `send()`. |
| `close()` | Aborts, releases the runtime and its MCP connections. Idempotent; later calls reject with "session is closed". |

The runtime stays blocked on an unanswered `tool_confirmation` or
`elicitation` until the host answers, aborts, restarts or closes. Without
an `onEvent` handler nobody could answer, so such a session declines them
all. Return values from a session's `onEvent` are ignored; a returned
Promise is not awaited.

### `chat({...options, messages}, onEvent) -> Promise<{message, usage?}>`

The original stateless call, kept for the demo page: one session per call,
seeded with `messages` (OpenAI-style, `tool_calls` and `tool_call_id`
included, so a client-side history replays faithfully). The last message
must be from the user; it is the prompt. Because there is no handle to
answer prompts with, the `onEvent` return value does: return `true`, a
decision string or `{decision, ...}` for `tool_confirmation`, and
`{action, content?}` for `elicitation`; a Promise is awaited. Anything else
— including no handler at all — declines, so the call never hangs.

### `abort() -> void`

Cancels the in-flight `chat()` call (only one runs at a time; a new call
cancels the previous). Safe to call before the call has started running.

### Events

`onEvent` receives, for both APIs:

| Event | Fields |
| --- | --- |
| `delta` | `content?` or `reasoning?` — a streamed chunk. |
| `tool_call_delta` | `id, name, arguments` — a streamed chunk of a tool call's arguments. |
| `tool_call` | `id, name, args` — the call is about to run. |
| `tool_confirmation` | `id, name, args` — the runtime waits for `confirm()`. |
| `tool_output` | `id, name, output` — incremental output of a running call. |
| `tool_result` | `id, name, output, is_error` |
| `tool_blocked` | `id, name, reason` — a `pre_tool_use` hook denied the call. |
| `elicitation` | `id, message, mode, url, schema, meta` — an MCP server asks for input or OAuth; answer with `respondToElicitation()`. |
| `handoff` | `from, to` — `handoff`, `transfer_task` (and its return). |
| `fallback` | `from, to, attempt, reason` — the model chain moved on. |
| `usage` | `input_tokens, output_tokens` for that model call, plus `context_length, context_limit, cost` for the session. |
| `warning` | `message` |
| `error` | `message` — the turn failed; the promise rejects with the same message. |
| `finish` | `reason: "stop"` — the turn completed. |

### Differences from the previous bespoke loop

The earlier `cmd/wasm` re-implemented the agent loop in 800 lines; it now
runs the shared runtime. Intentional differences:

- Tool calls go through the runtime's approval chain. Without
  `autoApprove`, non-read-only tools raise `tool_confirmation`; the old
  loop ran everything unasked.
- `type: filesystem` toolsets are rejected instead of pointing at an empty
  in-memory `/`; the legacy `url:` field on `mcp` toolsets is gone — use
  `remote.url`. The portable builtins (`todo`, `plan`, `memory`, `fetch`,
  ...) are available; see *Limits*.
- `tool_result.output` is no longer truncated to 500 characters.
- `usage` is still emitted once per model call, with session totals added.
- Errors, hooks, fallbacks, compaction and delegation follow the CLI's
  behaviour exactly, since it is the same code.

## Limits

What the browser build supports, and what it refuses and why.

### Supported

| Area | In the browser |
| --- | --- |
| Toolsets | `mcp` (remote only), `think`, `todo`, `plan`, `memory`, `user_prompt`, `session_context`, `fetch`, `api`, `openapi`, `model_picker`, `rag` (over `documents`). See `examples/portable-team.yaml` and `examples/handbook-rag.yaml`. |
| Toolset options | `tools`, `readonly`, `instruction`, `model`, `toon`, `defer`, `timeout`, `allow_private_ips`. |
| Agent features | `code_mode_tools`, sub-agents, handoffs, fallbacks, compaction, `add_date`, `add_environment_info`, structured output, `${...}` JavaScript in instructions and descriptions. |
| Hooks | `type: builtin` only: `add_context`, `add_date`, `add_environment_info`, `limit_large_tool_results`, `max_iterations`, `redact_secrets`. `limit_large_tool_results` keeps only the bounded tail excerpt: there is no filesystem to spill the full result to, and the notice says so. |
| Providers | OpenAI (all API variants), Anthropic and Google, registered in `providers.go`. |

Stateful toolsets are scoped to the session: a `todo` with `shared: true`,
`plan` and `memory` are one store per session, shared by every agent of the
team. They survive `restart()` and are gone when the session closes. Two
sessions never see each other's todos, plans or memories, and nothing is
persisted; the `memory` instructions tell the model so instead of promising
memory across sessions. `plan` drops its `export_plan_to_file` /
`update_plan_from_file` tools since there is no filesystem to go through.
`session_context` only lists the conversations of its own runtime. `memory`
refuses `path`; `openapi` refuses a spec that is not an `http(s)` URL.

`${...}` expansion is the same goja evaluator the CLI uses (`js.NewJsExpander`);
it sees the session `env` and nothing else — no I/O, no process, no `require`.
Slash commands are not resolved by `send()`, so the runtime's command
evaluator is not wired.

`fetch`, `api` and `openapi` share the SSRF-guarded transport with remote
MCP: in a browser they go through `toolProxy` and fail closed without one,
unless the toolset sets `allow_private_ips: true`.

`rag` runs the shared `pkg/rag` pipeline — `bm25`, `chunked-embeddings` and
`semantic-embeddings`, fusion and reranking — over the session's `documents`
instead of files: the index is built in memory when the tool first starts,
results report the documents' logical paths, `return_full_content` reads
the supplied document, and nothing is watched since the documents cannot
change. Embedding and reranking models are ordinary `models` entries and
need their API key in `env`. A RAG whose `docs` select none of the supplied
documents is rejected when the session is created.

### Refused

- **Toolsets**: `mcp` only with `remote.url`. stdio servers (`command`),
  catalog references (`ref`) and the local-only MCP fields (`working_dir`,
  `env`, `config`, `version`, `path`) are rejected rather than ignored.
  `shell`, `script`, `filesystem`, `file`, `git`, `background_jobs`, `lsp`,
  `tasks`, `environment`, `scheduler`, `webhook`, `open_url`, `a2a`,
  `mcp_catalog` and `background_agents` need a process, a filesystem, a
  socket or the host environment and are not registered.
- **RAG**: a strategy `database` (nothing persists), `chunking.code_aware`
  (tree-sitter needs cgo) and `respect_vcs: true` (no checkout) are
  rejected; `docs` never reach the filesystem.
- **Hooks**: `type: command` and the builtins that read files or run git.
- **Local files**: `add_prompt_files`, `cache.path`, `skills`, `memory.path`,
  non-HTTP `openapi.url`.
- **Features needing extra wiring**: `harness`, external agents (OCI/URL
  references). `teamloader.WithStrict` reports every unmet requirement in
  one error.
- **Providers**: OpenAI (all API variants), Anthropic and Google, registered
  in `providers.go`. The shared core registry is empty on every platform.
  Bedrock, Vertex AI and explicit-URL DMR are opt-in with
  `docker_agent_wasm_cloud` (see above). Host credential discovery is not
  available in a browser.
- **No persistence**: sessions live in memory for the lifetime of the tab;
  todos, plans, memories, RAG indexes and MCP OAuth tokens are per-session,
  in memory.
- **models.dev**: the catalog baked into the binary is used; there is no
  cache directory to refresh it into.
- **CORS**: see above. Real deployment needs a proxy for most providers.

## Where the code lives

| File | Purpose |
| --- | --- |
| `main.go` | JS API registration, `parseConfig`/`listAgents`, `createSession`, the stateless `chat()`/`abort()`. |
| `runtime.go` | Builds an `embeddedchat.Session` from YAML with the browser registries and loader features; audits the config for host-only features. |
| `toolsets.go` | The per-session toolset registry: remote MCP plus the portable builtins, `rag` over the session documents, and the per-toolset browser checks. |
| `session.go` | `chatSession`: one conversation, its lifetime context, send/confirm/abort/restart/close. JS-agnostic, tested from Go. |
| `events.go` | Projects runtime events onto the JS event shapes. |
| `handle.go` | The JS session object and the lifetime of its callbacks. |
| `bridge.go` | JS ⇄ Go value helpers, promises, awaiting JS callbacks. |
| `providers.go` | Explicit demo provider registry (OpenAI / Anthropic / Google). |
| `examples/` | Configs that run unchanged in the browser and in the CLI. |
| `node/` | Node loader and JS bridge tests. |
| `browser/` | Headless Chrome integration tests using real fetch and local mock services. |

The shims that make the tree compile under `GOOS=js GOARCH=wasm` are
intentionally tiny (`pkg/cache/lock_js.go`, `pkg/userconfig/lock_js.go`,
`pkg/desktop/*_js.go`, `pkg/httpclient/egress_js.go`, ...); everything else
compiles unchanged because the `os/exec`, sandbox, sound, audio, server,
browser and keyring code is isolated behind packages the wasm entry does not
import.

## Embedding with fewer providers

`pkg/model/provider` shares its registry implementation between native and
js/wasm builds and imports no concrete SDK-backed providers. Its
`EmptyRegistry()` contains no providers: code that constructs models must pass
an explicit registry. The demo retains its provider set through
`demoProviders`; a smaller application can register just Anthropic:

```go
registry := provider.NewRegistry(map[string]provider.Factory{
    "anthropic": provider.Adapt(anthropic.NewClient),
})
```

Pass it to `teamloader.WithProviderRegistry(registry)` when loading YAML and
`runtime.WithProviderRegistry(registry)` when constructing a local runtime, so
runtime model switching uses the same provider set — `runtime.go` shows the
full recipe, including `teamloader.WithStrict()` to reject unsupported
configuration features. Do not import `pkg/teamloader/defaults` or
`pkg/model/provider/providers` in a restricted bootstrap: those deliberately
wire the full implementations.

## Sanity check

```sh
# Native build still happy.
go build ./...

# Wasm build still happy.
GOOS=js GOARCH=wasm go build -o cmd/wasm/web/docker-agent.wasm ./cmd/wasm

# Runtime bridge tests under Node's Go/WASM runner, with mocked models.
task test-wasm-js

# JS surface against the built binary.
node cmd/wasm/smoke_test.js
node --test cmd/wasm/node/bridge_test.js
```
