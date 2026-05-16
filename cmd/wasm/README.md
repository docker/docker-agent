# docker-agent in the browser (js/wasm)

`cmd/wasm` is a `GOOS=js GOARCH=wasm` entry point that exposes a thin slice
of docker-agent — config parsing and a single-round streaming chat — to a
JavaScript host (a browser tab or Node).

It is **not** a port of the full agent. It is a proof-of-concept for the
"realistic plan" outlined when we surveyed which parts of docker-agent could
even be cross-compiled to wasm. See the *Limits* section below.

## Build

```sh
# Compile.
GOOS=js GOARCH=wasm go build -o cmd/wasm/web/docker-agent.wasm ./cmd/wasm

# Copy the matching wasm_exec.js shim from the Go toolchain (its API is
# tied to the compiler version and must match the binary).
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" cmd/wasm/web/wasm_exec.js
```

The output is a ~75 MB `.wasm`. It includes the entire YAML parser, three LLM
provider clients (OpenAI, Anthropic, Google) and their dependencies. With
`tinygo` or `-ldflags="-s -w"` plus `wasm-opt` you can roughly halve it; we
have not optimised the size.

## Run (Node smoke test)

```sh
node cmd/wasm/smoke_test.js
```

Prints the parsed config of a small two-agent YAML and exits 0. This proves
that the Go runtime starts under wasm, that `globalThis.dockerAgent` gets
registered, and that `parseConfig` returns the expected shape.

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
`env.OPENROUTER_API_KEY` to `dockerAgent.chat(...)`, the existing
`pkg/model/provider/openai` reads it via `env.Get(ctx, cfg.TokenKey)`, and
the Go HTTP transport (mapped to `fetch` under js/wasm) sends the
request. **Same code path the CLI uses** — no special browser-only branch.

### Bring-your-own-key fallback

The `<details>` block on the page lets advanced users paste OpenAI /
Anthropic / Gemini keys directly. Anthropic recently added
`anthropic-dangerous-direct-browser-access` so it actually works from a
tab; OpenAI and Gemini still block CORS for browser origins, so those
fields are mostly there for use against a self-hosted proxy.

## JavaScript API

Once the wasm boots, two functions are exported on `globalThis.dockerAgent`:

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

### `chat({yaml, agentName?, env?, messages}, onEvent) -> Promise`

Asynchronous. Loads the config, picks the agent (or the only one), builds
its model provider, and opens one streaming chat completion.

- `yaml` — the YAML document, same as for `parseConfig`.
- `agentName` — required if the config defines more than one agent.
- `env` — `{ OPENAI_API_KEY: "...", ANTHROPIC_API_KEY: "...", GEMINI_API_KEY: "..." }`.
  Whatever your model needs.
- `messages` — array of `{role, content}` objects, OpenAI-style. The agent's
  `instruction` is automatically prepended as a `system` message if you
  haven't already supplied one.
- `onEvent(ev)` — called from Go for every stream event:
  - `{type: "delta",  content?: string, reasoning?: string}`
  - `{type: "finish", reason: string}`

Resolves to `{message: {role, content, reasoning, finish}}` once the stream
ends. Rejects with an `Error` on any failure.

## Limits

These are not bugs to fix; they are direct consequences of `GOOS=js`:

- **No tools, no MCP, no hooks, no sub-agent handoffs.** Anything that needs
  `os/exec` or local file I/O is excluded from the build.
- **No sessions.** `pkg/session` and `pkg/memory/database/sqlite` pull in
  `modernc.org/libc` which does not have a js port. The browser caller is
  responsible for keeping the message history.
- **No fallbacks, no rule-based routing.** The rule-based router uses bleve,
  which depends on `mmap` / file-locking primitives that don't exist on
  js/wasm. A js-only `factory_js.go` swaps the full provider factory for a
  slim variant with only openai / anthropic / google.
- **No Docker Model Runner, no Bedrock, no Vertex AI.** Same reason —
  `dmr` shells out, `bedrock` and `vertexai` pull in cloud SDKs that don't
  cross-compile to wasm.
- **No Docker Desktop integration.** `pkg/desktop` has stubs for js that
  return empty paths and refuse to dial.
- **CORS.** Mentioned above. Real deployment needs a proxy.

## Where the cross-compilation work lives

The shims that make the existing tree compile under `GOOS=js GOARCH=wasm`
are intentionally tiny:

| File | Purpose |
| --- | --- |
| `pkg/cache/lock_js.go` | No-op file-lock stubs (single-threaded js). |
| `pkg/desktop/sockets_js.go` | Returns empty Docker Desktop paths. |
| `pkg/desktop/connection_js.go` | Refuses Unix-socket / named-pipe dials. |
| `pkg/desktop/connection_other.go` | Build tag updated to `!windows && !js`. |
| `pkg/model/provider/factory.go` | Build tag added: `!js`. |
| `pkg/model/provider/factory_js.go` | js-only provider dispatch (openai / anthropic / google). |

Everything else compiles unchanged because docker-agent already had the
`os/exec`, sandbox, sound, audio, server, browser, keyring code well-isolated
behind their own packages — the wasm entry just doesn't import them.

## Sanity check

```sh
# Native build still happy.
go build ./...

# Wasm build still happy.
GOOS=js GOARCH=wasm go build -o /tmp/cagent.wasm ./cmd/wasm

# Existing tests of the touched packages still pass.
go test ./pkg/cache/... ./pkg/desktop/... ./pkg/model/provider/...

# End-to-end runtime smoke test.
node cmd/wasm/smoke_test.js
```
