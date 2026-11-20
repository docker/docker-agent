# Experimental MCP Events push demo

Run from the repository root:

```sh
./bin/docker-agent run examples/mcp-events/agent.yaml
```

Ask: **“Subscribe to demo.message with interval_seconds 5 and count 3.”**
The agent discovers the event schema and opens an `events/stream`. The Go
server emits three simulated external job updates, then closes the stream.
Ask for `count: 100` to try cancelling it with `demo_mcp_events_unsubscribe`.
Change the model in the YAML if needed. The server can also be started directly
with `go run ./examples/mcp-events`; stdin/stdout are reserved for JSON-RPC.

## Streamable HTTP

Start the server in one terminal:

```sh
go run ./examples/mcp-events -listen 127.0.0.1:8080
```

Then run the agent in another terminal and use the same subscription prompt:

```sh
./bin/docker-agent run examples/mcp-events/agent-http.yaml
```

The example enables `allow_private_ips` for the local demo server. The HTTP
server is unauthenticated; keep it bound to loopback for local use.

## Opt in

MCP Events is disabled by default. Both demo configs enable it per MCP toolset:

```yaml
toolsets:
  - type: mcp
    experimental:
      mcp_events: true
    # command or remote configuration
```

Without this setting, docker-agent does not advertise Events support, install
event adapters, or expose event tools. Shared `mcps` definitions can also set
it; a referencing toolset can override it with `experimental: {mcp_events: false}`.
Embedders using MCP toolset constructors can opt in with
`SetExperimentalEvents(true)` before starting the toolset.

## Scope

This implements the **push mode over stdio and Streamable HTTP** of the proposed
[SEP-3415 Events Extension](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/3415),
pinned to revision `b08e4f9dd684f39d846d94089487fbeb28e666f5`.
It is experimental, not a finalized MCP feature.

The server advertises `capabilities.extensions["io.modelcontextprotocol/events"]`
under protocol `2026-07-28`. For compatible stdio and Streamable HTTP servers, docker-agent adds
three local tools: `mcp_events_list`, `mcp_events_subscribe`, and
`mcp_events_unsubscribe`, prefixed by the toolset name. Subscriptions are
explicit agent tool calls; no event types are subscribed automatically.
The unsubscribe tool cancels `events/stream`; it does **not** call
`events/unsubscribe`, which the proposal reserves for webhooks.

The existing Go MCP SDK supports custom RPC calls but not custom notifications
on clients. A stdio connection adapter and an HTTP response-body adapter
receive and correlate extension notifications. The HTTP adapter leaves the
SDK connection intact and retains existing authentication, header resolution,
and network policy. Both regular and session-owned remote clients support it;
no dependency upgrade or SDK fork is needed. Cancelling an HTTP subscription
aborts its POST response stream without closing other subscriptions.
Legacy SSE transport, polling, webhooks, and event-list change notifications
are not implemented.

Events use the existing tool-runtime recall path: steering while a turn is
active, and the host's recall wake-up path when idle. Hosts without idle wake-up
support retain messages for the next turn. Subscriptions end with the MCP
connection, not the originating tool call. They do not survive server process
restarts. A stalled stream is reopened after 60 seconds without traffic using
its latest in-memory cursor. The demo is emit-only and always returns a null
cursor, so missed updates cannot be replayed.

Payloads are labeled as untrusted external data and grant no authorization;
tool permissions still apply. Delivery is bounded to 16 subscriptions per
connection, 32 buffered notifications per subscription, 256 KiB per event,
and a 1,024-event deduplication window per subscription. A full delivery queue
or buffer stops the subscription and logs the failure rather than silently
advancing its cursor. Cursors are not persisted across process restarts.
