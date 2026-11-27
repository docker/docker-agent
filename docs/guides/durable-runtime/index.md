---
title: "Durable Runtime"
description: "Keep embedded agent runs recoverable across process restarts with checkpoints and a tool-call journal."
keywords: docker agent, durable runtime, checkpoints, Go SDK, recovery
weight: 41
canonical: https://docs.docker.com/ai/docker-agent/guides/durable-runtime/
---

The **durable runtime** lets your Go application continue an agent run after
its process crashes or deliberately suspends work. It records the run's progress
alongside its conversation in a session store. On restart, your application
recreates the agent team and asks the runtime to continue the unfinished run,
using the recorded input.

Use it for embedded workers that must pick up unfinished conversations after a
restart. Use the local runtime for applications that only need to save
conversation history.

> [!NOTE]
> **Available on main**
>
> The durable runtime is available on `main`. Enable this opt-in **Go SDK**
> API in your application by calling `runtime.NewDurableRuntime`.

## Sessions, Runs, and Checkpoints

A **session** holds a conversation. A **run** is the work the runtime performs
on that conversation during one invocation. A durable run adds a **checkpoint**:
the next step to execute, its working state, and the transcript position that
belongs to it.

| | Local runtime with a session store | Durable runtime |
| --- | --- | --- |
| Constructor | `runtime.NewLocalRuntime` | `runtime.NewDurableRuntime` |
| Conversation storage | Best-effort persistence | Checkpoints wait for completed transcript writes |
| Progress in flight | In memory | Recorded in the store |
| Storage failure | Logs the failure and continues | Stops; retains the last recorded progress when possible |
| Restart | Load conversation history | `ContinueStream` loads the session and continues its recorded run |
| External coding harness | Supported when configured | Refused |

Use [session history](../../features/sessions/index.md) to reopen a conversation
with the CLI's `--session` option, [workspace snapshots](../../features/snapshots/index.md)
to restore files, and durable checkpoints to recover execution progress in your
Go application.

## How Checkpoints Work

A new run records its session and initial input before starting toolsets.
For a session with no stored conversation, that includes its full supplied
history. For an existing stored conversation, it adds the current prompt. Keep
that conversation synchronized with the store before starting a run.

The runtime and session store then track these steps:

| Next step | What continuation does |
| --- | --- |
| `starting` | Runs the start hooks for the recorded prompt. |
| `model` | Prepares the turn and asks the model again, replacing any partial streamed answer. |
| `tools` | Processes the recorded answer's tool calls using the tool-call journal. |
| `answered` | Processes the saved answer: stop hooks, structured-output enforcement, handoffs or completion routing, and queued input. |

After a step finishes, the runtime waits for the persistence observer to store
its completed output, then saves the checkpoint. Tool calls also update a
journal as they progress. On continuation, the store removes transcript entries
after the checkpoint, so the conversation and recorded progress agree. Paid
usage from side requests such as `/btw` is preserved rather than replayed.

Checkpoints restore the conversation's agent selection and routing, iteration
count and limit, instruction context, approval state, and working state needed
by the next step. Your application recreates the surrounding team and tools.
See [What Survives a Restart](../durable-runtime-recovery/index.md#what-survives-a-restart).

## Tool Calls and Side Effects

A tool call can leave its external outcome uncertain: the process might die
after the operation finishes but before its result is recorded. Recovery uses
the recorded call state to decide how to proceed.

| Recorded state | Recovery behavior |
| --- | --- |
| Not started | Runs the call normally. |
| Started, no recorded result | Retries only if the tool is annotated idempotent, is not a delegation, and receives the same arguments as before. Otherwise records an interrupted-call result for the model to handle. |
| Result recorded | Runs the pending `post_tool_use` hook with the original call and raw result. |
| Finished | Preserves its recorded outcome, including an outcome that stops the run. |

An **idempotent** tool can safely repeat the same operation with the same
arguments. The journal requires the tool's `Annotations.IdempotentHint`, a
non-delegating tool, and identical arguments to retry a started call. Hooks can
rewrite arguments; changed arguments lead to an interrupted-call result.

Design side-effecting tools with their own deduplication or reconciliation
where needed. An interrupted result leaves the action's external outcome
uncertain. Inspect the external system before repeating an operation with side
effects. An interrupted sub-agent becomes an interrupted tool call, and the
model can decide whether to delegate again.

Hooks in a repeated step can run again. Start-hook context is restored once
recorded; a run interrupted before completing its starting step can repeat
those hooks. A continued turn past the model step also fires `turn_start` again
to pair with that stream's `turn_end`. Make hook side effects safe to repeat.

## Start Here

1. [Build a Durable Worker](../durable-runtime-quickstart/index.md) — a complete Go program with
   SQLite storage, graceful suspension, and startup recovery.
2. [Recovery and Operations](../durable-runtime-recovery/index.md) — lifecycle, ownership, storage,
   limitations, and troubleshooting.
3. [Go SDK](../go-sdk/index.md) — team loading, tools, runtime options, and
   interactive event handling.
