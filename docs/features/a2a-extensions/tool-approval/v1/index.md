---
title: "A2A tool-approval extension (v1)"
description: "Specification of the tool-approval/v1 A2A extension: how a client approves or rejects a tool call of a task paused in TASK_STATE_INPUT_REQUIRED."
keywords: docker agent, a2a, extension, tool approval, input required
weight: 10
canonical: https://docs.docker.com/ai/docker-agent/features/a2a-extensions/tool-approval/v1/
---

## Overview

This page specifies the `tool-approval/v1` [A2A extension](https://a2a-protocol.org/latest/topics/extensions/).
It defines how a client answers a tool call that a Docker Agent A2A server
holds for confirmation. The task waits in the A2A
[`TASK_STATE_INPUT_REQUIRED`](https://a2a-protocol.org/latest/specification/)
state until the client replies.

For setup and usage, see [Tool approval](../../../a2a/index.md#tool-approval-opt-in).

The extension URI is:

```text
https://docker.github.io/docker-agent/features/a2a-extensions/tool-approval/v1
```

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as
described in [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119).

## Declaration

A server that supports the extension lists it in its agent card under
`capabilities.extensions`, with `required: false`. A Docker Agent server
declares it only when started with `--tool-approval`.

```json
{
  "capabilities": {
    "extensions": [
      {
        "uri": "https://docker.github.io/docker-agent/features/a2a-extensions/tool-approval/v1",
        "description": "Approve or reject tool calls of tasks paused in TASK_STATE_INPUT_REQUIRED.",
        "required": false
      }
    ]
  }
}
```

A client activates the extension for a call by listing its URI in the message
`extensions` field or in the `A2A-Extensions` header (allowed by the CORS
policy for browser clients). The server validates
replies the same way whether or not the extension is activated.

## Approval request

When a tool call needs confirmation, the server moves the task to
`TASK_STATE_INPUT_REQUIRED`. The status message holds one data part per
pending call, with:

- `data`: the function call, with `id`, `name` and `args`.
- `metadata.adk_type`: `"function_call"`.
- `metadata.adk_is_long_running`: `true`.
- `metadata["<extension URI>"]`: an object with the fields below.

| Field | Type | Description |
|---|---|---|
| `prompt` | string | Human-readable question to show the user. |
| `grant` | string | The permission pattern that `approve-in-context` stores, e.g. `shell:cmd=ls*`. |
| `grantLabel` | string | Display label for `grant`, e.g. `allow ls* in this context`. |
| `responseSchema` | object | JSON Schema of the expected `response` object. |

```json
{
  "data": {
    "id": "call_123",
    "name": "shell",
    "args": {
      "cmd": "ls"
    }
  },
  "metadata": {
    "adk_type": "function_call",
    "adk_is_long_running": true,
    "https://docker.github.io/docker-agent/features/a2a-extensions/tool-approval/v1": {
      "prompt": "Tool \"shell\" requires approval",
      "grant": "shell:cmd=ls*",
      "grantLabel": "allow ls* in this context",
      "responseSchema": {
        "type": "object",
        "required": ["decision"],
        "properties": {
          "decision": {
            "type": "string",
            "enum": ["approve", "approve-in-context", "reject"]
          },
          "reason": {
            "type": "string"
          }
        }
      }
    }
  }
}
```

`grant` and `grantLabel` are absent when the call cannot be scoped (a command
tool call with an empty command, or a glob, `:` or `\` in its first word); the enum then
omits `approve-in-context`. Clients SHOULD read the allowed decisions from
`responseSchema` rather than hard-code them.

## Approval reply

The client MUST answer with `SendMessage` on the paused task: `taskId` set to
the task's ID, and `contextId` set to its context. The message MUST contain a
data part with:

- `data.id`: the `id` of the pending call.
- `data.name`: the name of the pending call.
- `data.response`: an object valid against `responseSchema`.
- `metadata.adk_type`: `"function_response"`.

```json
{
  "taskId": "…",
  "contextId": "…",
  "extensions": ["https://docker.github.io/docker-agent/features/a2a-extensions/tool-approval/v1"],
  "parts": [
    {
      "data": {
        "id": "call_123",
        "name": "shell",
        "response": {
          "decision": "approve"
        }
      },
      "metadata": {
        "adk_type": "function_response"
      }
    }
  ]
}
```

### Decisions

| `decision` | Effect |
|---|---|
| `approve` | Run this call only. The next matching call asks again. |
| `approve-in-context` | Run this call and allow later calls matching `grant` in the same `contextId`. The grant is stored with the session. Only allowed when `grant` is present. |
| `reject` | Skip the call. The optional `reason` string is passed to the model. |

## Errors

The server rejects the following messages with JSON-RPC error `-32602`
(invalid params). The error `data` holds a `reason` string.

| Message | Example `reason` |
|---|---|
| Reply on a paused task without a `function_response` for the pending call `id` | `expected a function_response for call 'call_123'` |
| Reply with a `decision` outside the enum | `decision 'x' not allowed; expected [approve, approve-in-context, reject]` |
| Reply with parts that can't be decoded | `malformed approval reply` |
| Message with a `null` part | `message parts must not be null` |
| New task (no `taskId`) in a `contextId` held by a paused task | `task … is waiting for tool approval in this context; reply with its taskId or cancel it` |

After a rejected reply, the task stays in `TASK_STATE_INPUT_REQUIRED`, nothing
is added to its history and no push notification is sent. The client MAY
retry.

## Lifecycle

- `CancelTask` on a paused task cancels it and drops the pending approval.
  It also cancels a tool already running after approval. A cancel that races
  the pause ends the task canceled.
- A server MAY expire pending approvals. Docker Agent keeps them in memory
  for one hour and drops them at shutdown. A reply to an expired or lost
  approval fails the task without starting a new model turn.

## Versioning

Changes that break this contract get a new URI (`…/tool-approval/v2`). New
optional metadata fields MAY be added to `v1`. Clients MUST ignore fields
they don't know.
