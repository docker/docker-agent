---
title: "AG-UI Protocol"
description: "Expose Docker Agent agents to user-facing applications through the AG-UI protocol, with any AG-UI client SDK."
keywords: docker agent, ai agents, features, ag-ui protocol
weight: 65
canonical: https://docs.docker.com/ai/docker-agent/features/ag-ui/
---

## Overview

The `docker agent serve ag-ui` command starts an HTTP server that speaks the
server side of the [AG-UI protocol](https://docs.ag-ui.com). An application
sends a `RunAgentInput` and receives the run as a stream of AG-UI events over
Server-Sent Events (SSE): assistant text, reasoning, tool calls and their
results, and the agent of a team that is currently working.

Any AG-UI client can talk to the agent, whatever its language: the
[AG-UI client SDKs](https://github.com/ag-ui-protocol/ag-ui) exist for
TypeScript, Python, Go and more. You don't write protocol code.

> [!WARNING]
> **First step of AG-UI support**
>
> This release covers a streaming chat. Human-in-the-loop, frontend tools,
> shared state and attachments are not implemented yet. See
> [Current limitations](#current-limitations).

## Usage

```bash
# Start an AG-UI server for an agent
$ docker agent serve ag-ui ./agent.yaml
Listening on 127.0.0.1:8084
AG-UI endpoint: http://127.0.0.1:8084/ag-ui

# Pick one agent of a team as the entry point
$ docker agent serve ag-ui ./team.yaml --agent reviewer

# Use an agent from an OCI registry, on another address
$ docker agent serve ag-ui myorg/agent:tag --listen 127.0.0.1:9090
```

## Flags

See the [CLI reference](../cli/index.md#docker-agent-serve-ag-ui) for all flags,
defaults, and shared runtime options.

## Endpoint

| Method | Path     | Description                                                          |
| ------ | -------- | -------------------------------------------------------------------- |
| `POST` | `/ag-ui` | Takes a `RunAgentInput` JSON body and answers with `text/event-stream` |

```bash
$ curl -N http://127.0.0.1:8084/ag-ui \
    -H 'Content-Type: application/json' \
    -d '{
      "threadId": "thread-1",
      "runId": "run-1",
      "messages": [{"id": "msg-1", "role": "user", "content": "Hello"}],
      "tools": [], "context": [], "state": {}, "forwardedProps": {}
    }'
```

Each event is one SSE frame. Its `data` line is the AG-UI event as JSON:

```text
id: RUN_STARTED_1764345600000
data: {"type":"RUN_STARTED","timestamp":1764345600000,"threadId":"thread-1","runId":"run-1"}

id: TEXT_MESSAGE_START_1764345600120
data: {"type":"TEXT_MESSAGE_START","timestamp":1764345600120,"messageId":"56738741-aa2b-4dba-a1be-7f0e005043ce","role":"assistant"}

id: TEXT_MESSAGE_CONTENT_1764345600121
data: {"type":"TEXT_MESSAGE_CONTENT","timestamp":1764345600121,"messageId":"56738741-aa2b-4dba-a1be-7f0e005043ce","delta":"Hello!"}

id: TEXT_MESSAGE_END_1764345600300
data: {"type":"TEXT_MESSAGE_END","timestamp":1764345600300,"messageId":"56738741-aa2b-4dba-a1be-7f0e005043ce"}

id: RUN_FINISHED_1764345600301
data: {"type":"RUN_FINISHED","timestamp":1764345600301,"threadId":"thread-1","runId":"run-1"}
```

### Use an AG-UI client SDK

With the TypeScript client (`npm install @ag-ui/client`):

```ts
import { HttpAgent } from "@ag-ui/client";

const agent = new HttpAgent({
  url: "http://127.0.0.1:8084/ag-ui",
  threadId: "thread-1",
});
agent.messages = [{ id: "msg-1", role: "user", content: "Hello" }];

await agent.runAgent({}, {
  onTextMessageContentEvent: ({ event }) => process.stdout.write(event.delta),
});
```

## Threads and sessions

The AG-UI `threadId` is the Docker Agent session ID.

- An unknown `threadId` creates a session; a known one resumes it. Sessions are
  stored in the session database (`--session-db`), so a thread survives a
  server restart.
- **The server-side session is the source of truth.** AG-UI clients resend the
  whole conversation on every run. The server appends only the user messages
  that end `messages` and ignores the rest of that history, including system,
  assistant and tool messages.
- A user message is identified by its `id`. A message the thread already holds
  is not appended twice, so a client can retry a failed run with the same
  messages.
- Only one run at a time per thread. A run on a thread that already has a run
  in progress is rejected with `409 Conflict`.
- A `threadId` that matches a session created another way (the TUI, the API
  server, A2A) is rejected with `409 Conflict`, and that session is left
  untouched.
- Closing the connection cancels the run.

Before each resumed run, Docker Agent reapplies the selected agent's
`max_iterations`, `max_consecutive_tool_calls`, `max_old_tool_call_tokens` and
`max_tool_result_tokens`. The safety policy of a thread is preserved or
tightened to the server's policy, never relaxed.

## Event mapping

| Docker Agent runtime event              | AG-UI event                                                   |
| --------------------------------------- | ------------------------------------------------------------- |
| Run started / stopped                   | `RUN_STARTED` / `RUN_FINISHED`                                |
| Error, canceled run                     | `RUN_ERROR`                                                   |
| Assistant text                          | `TEXT_MESSAGE_START` / `TEXT_MESSAGE_CONTENT` / `TEXT_MESSAGE_END` |
| Reasoning                               | `REASONING_START` / `REASONING_MESSAGE_START` / `REASONING_MESSAGE_CONTENT` / `REASONING_MESSAGE_END` / `REASONING_END` |
| Tool call being streamed by the model   | `TOOL_CALL_START` / `TOOL_CALL_ARGS`                          |
| Tool call dispatched / tool result      | `TOOL_CALL_END` / `TOOL_CALL_RESULT`                          |
| Hand-over to a sub-agent and back       | `STEP_STARTED` / `STEP_FINISHED` (`stepName` is the agent name) |
| Token usage, warning, session title, model fallback | `CUSTOM`                                          |

Notes:

- Tools run on the server. Tool events show progress; the client is not asked
  to execute anything.
- The server ends every message, tool call and step it started before
  `RUN_FINISHED`. Nothing follows `RUN_FINISHED` or `RUN_ERROR`.
- `RUN_ERROR` carries a `code`: `canceled` for a canceled run, the runtime
  error code when there is one, `run_failed` otherwise.
- `CUSTOM` events are named `docker-agent.token_usage`, `docker-agent.warning`,
  `docker-agent.session_title` and `docker-agent.model_fallback`. Their `value`
  is the runtime event, in the format of the [API server](../api-server/index.md).

## Input validation

The request is validated before anything is stored or run. An invalid request
gets `400 Bad Request` with a JSON body, `{"error": "<reason>"}`, and changes
nothing: the same `threadId` still accepts a valid request.

| Field            | Rule                                                                      |
| ---------------- | ------------------------------------------------------------------------- |
| `threadId`, `runId` | Required, at most 256 bytes.                                           |
| `messages`       | Every message needs an `id` and a known `role`. The history must end with at least one user message whose content is non-empty text (a string, or a list of `text` parts). |
| `tools`          | Must be empty: frontend tools are not supported.                           |
| `state`          | Must be empty (`null`, `{}`): shared state is not supported.               |
| `resume`         | Must be empty: interrupts are not supported.                               |
| `context`, `forwardedProps` | Accepted and ignored.                                           |

Unsupported fields are rejected rather than silently ignored, so a client finds
out immediately that a feature it relies on is missing. A request body larger
than 8 MiB is rejected with `413 Request Entity Too Large`.

## Authentication and network exposure

Loopback listeners may run without authentication. Non-loopback listeners
require `--auth-token` unless `--insecure-no-auth` explicitly acknowledges the
exposure. Clients must send `Authorization: Bearer <token>`.

Browser access is off by default. Enable it with `--cors-origin`; it accepts
comma-separated literal origins or `~`-prefixed regular expressions. Allowed
origins may send the `Authorization`, `Content-Type` and `Accept` headers.

```bash
$ docker agent serve ag-ui ./agent.yaml --auth-token "$AG_UI_TOKEN" \
    --cors-origin http://localhost:3000
```

## Tool safety

AG-UI threads default to the `restricted` tool safety policy. Precedence is the
`--safety` flag, then agent YAML, then runtime YAML. YAML may select `strict`,
`balanced`, or `restricted`; `--safety autonomous` is the only way to opt in to
autonomous tool execution.

Nobody can approve a tool call over AG-UI yet. A tool call that the policy does
not allow is denied, and the agent is told so. An elicitation request from an
MCP server is declined.

## Current limitations

- **No human-in-the-loop.** Tool approval and elicitation through AG-UI
  interrupts are not implemented (`resume` is rejected).
- **No frontend tools.** A non-empty `tools` field is rejected.
- **No shared state.** `STATE_SNAPSHOT` and `STATE_DELTA` are never sent, and a
  non-empty `state` is rejected.
- **Text only.** Images and other attachments in user messages are rejected.
- **One entry agent per server.** Use `--agent` to choose it; sub-agents are
  reached through delegation.

> [!TIP]
> **See also**
>
> For OpenAI-compatible clients, see [Chat Server](../chat-server/index.md). For agent-to-agent communication, see [A2A](../a2a/index.md). For full control over sessions and runtime events, see [API Server](../api-server/index.md).
