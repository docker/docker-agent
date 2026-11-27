---
title: "Recovery and Operations"
description: "Operate durable agent runs safely: stream lifecycle, single-writer recovery, persisted state, and troubleshooting."
weight: 43
canonical: https://docs.docker.com/ai/docker-agent/guides/durable-runtime-recovery/
---

Your application owns scheduling, shutdown, and deciding which unfinished runs
to continue. The [durable runtime](../durable-runtime/index.md) supplies
checkpointing and continuation.

## Own the Database Before Recovering

`store.(session.RunStore).ListRuns(ctx)` lists **all recorded runs**, including
ones that are still executing. Your application must establish exclusive
ownership before recovery, for example by stopping the old worker before
starting its replacement.

Before calling `ContinueStream` or `DiscardRun`:

1. Establish that your application is the store's single writer. Stop the old
   worker before starting its replacement and reserve the database for recovery.
2. Finish draining the session's old stream before continuing it, including
   streams in your own process.
3. Recreate the team, tools, credentials, and working directory the run needs.
4. List the records and decide whether to continue or discard each one.

> [!WARNING]
> Continuation and discard modify the stored transcript. Calling either while
> the previous run is active can cut the conversation back underneath it.
> Recover a listed run only after confirming its previous writer has stopped.

Use `ContinueStream` to recover an unfinished recorded run from its checkpoint.
After that run is settled or deliberately discarded, use normal session loading
and `RunStream` to start new work on the conversation.

A runtime has one current agent shared by unpinned sessions. If you execute
several sessions concurrently on one runtime, pin each fresh session with
`session.WithAgentName`. Continuation restores the recorded pin and agent
selection.

## Stream and Shutdown Lifecycle

A durable run waits for its events to be read before recording progress. Always
read a stream until its channel closes, including after an error event or a
shutdown request. Collect errors while continuing to consume events.
The [worker example](../durable-runtime-quickstart/index.md) shows a draining consumer.

| How the run stops | Run record |
| --- | --- |
| Completes normally | Removed after final completed output reaches the store. Session history remains. |
| Context cancelled with `runtime.ErrSuspended` | Kept; a successfully saved suspension marker exempts the next continuation from counting a crash. |
| Other context cancellation or deadline | Ends the run and removes its record under normal storage conditions. |
| Process dies | Last saved record remains; continuation counts a crash. |
| Required transcript/checkpoint write fails | Stops with an error and retains the last record, if one was successfully created. |
| Final transcript flush fails | Reports an error instead of a clean end and leaves the record recoverable. |

For graceful shutdown, use `context.WithCancelCause` and cancel with
`runtime.ErrSuspended` **before** closing resources. Keep the run context
independent of a parent that shutdown cancels with ordinary `context.Canceled`:
the first cancellation wins. Then drain the stream, close the runtime, and
close the store. Give your process shutdown grace time to finish those writes;
a forced kill or storage failure can prevent the suspension marker from being
saved. Inspect `Failed to settle run record` warnings: a failed suspension write
is logged, and the next continuation counts a crash if the record remains
unmarked.

`ContinueStream(ctx, sessionID, opts...)` returns the reloaded session, an event
channel, and an error. Setup/recovery errors are returned directly with no
stream. Execution failures arrive as events once streaming starts. Reapply
host-only options, notably `session.WithNonInteractive`. Options can override
restored safety policy and permissions. The recorded agent selection and run
iteration limit stay in effect.

## What Survives a Restart

| State | Behavior |
| --- | --- |
| Conversation and pending answer | Restored to the transcript position represented by the checkpoint. Entries from unfinished work after it are removed. |
| Agent, routing, iteration progress, instruction context | Restored from the checkpoint. |
| Safety mode and ongoing tool approvals | Restored as of the checkpoint; continuation options can change them. |
| Tool-call progress | Restored from the journal; see [Tool Calls and Side Effects](../durable-runtime/index.md#tool-calls-and-side-effects). |
| Root vs. delegated runs | Only root runs are recorded independently. An interrupted delegation is reported to the model as an interrupted tool call. |
| In-memory budgets | Start afresh. Enforce cross-restart spending caps in your application. |
| Model-picker selections and deferred tool activation | Rebuilt agents start with their configured state. |
| Background-agent tasks | Tasks live in the original process. After restart, a status lookup can report a missing task; the model decides what to do next. |
| `Steer` / `FollowUp` queues | In memory until taken up. Input is recoverable once included in a saved checkpoint; queued input taken at turn end is recorded at the next model checkpoint. |
| Host-only session options | Pass these again to `ContinueStream`. |
| Workspace and external services | Retain their external state. Restore access separately. |

Keep the stored conversation synchronized with the session supplied to the
runtime. The durable start transaction stores the full conversation for a
session with no stored history and the current prompt for an existing
conversation.

## Retry Limits and Discarding

Continuation increments the crash count unless the run was marked suspended.
Saving progress resets it. When the count reaches **five** consecutive crashes
without progress,
`ContinueStream` gives up and returns an error. It restores the session to its
last checkpoint and removes the run record, provided those storage operations
succeed. A missing required agent in the rebuilt team also causes the run to
be given up this way.

For a run you choose not to continue, call:

```go
if err := rt.DiscardRun(ctx, sessionID); err != nil {
    return err
}
```

This fragment assumes an existing durable runtime and an inactive recorded
session. Discard restores the session to the checkpoint. External tool effects,
such as file writes or payments, remain. Review uncertain external operations
before starting new work.

A checkpoint with an unreadable payload or incompatible version is retained by
`ContinueStream`. `DiscardRun` can explicitly remove it;
the store still truncates the transcript to its recorded boundary, but an
unreadable checkpoint cannot restore its session metadata. Back up the database
before attempting continuation or discard of a suspect record. Both operations
first call the store's `ResumeRun`: it truncates entries after the checkpoint,
deletes associated sub-session trees, increments the crash count for an
unmarked run, and clears the suspension marker. These changes commit before
checkpoint decoding, so even a decoding error can follow transcript removal.

## Storage and Security

- **Keep the database persistent.** A container needs a persistent writable
  volume for the selected database path. Recreating an ephemeral filesystem
  loses its runs as well as its conversations.
- **Protect the database like conversation history.** Checkpoints can contain
  prompt-derived context, tool arguments and results, and approval state, in
  addition to the stored transcript. Restrict directory access and apply your
  application's backup, retention, secret-handling, and encryption policies.
- **Back up SQLite consistently.** The store normally uses WAL mode outside
  sandboxes. Stop writers for a consistent offline backup or use an appropriate
  SQLite backup procedure that includes committed writes in the WAL.
- **Plan upgrades.** Opening the store applies schema migrations, including the
  `session_runs` table. Checkpoint payloads are separately versioned and the
  runtime rejects versions it cannot read. Back up before upgrades and verify
  database and checkpoint compatibility before switching binaries.
- **Watch startup logs.** For some migration failures, `sqlitestore.New` moves
  the database and associated WAL/SHM files to a `.bak` path and creates a fresh
  database. Newer-schema, filesystem-open, and transient failures are returned
  instead. Check the resulting run records and investigate backup/recovery
  warnings.

## Limitations and Troubleshooting

| Symptom | What to check |
| --- | --- |
| Constructor says the store must record runs | Supply `runtime.WithSessionStore(store)` with a store implementing both `session.Store` and `session.RunStore`. SQLite is the shipped implementation. |
| `ContinueStream` reports no run | Confirm the session ID and database path. Records remain available for interrupted or suspended runs until completion, discard, or exhausted recovery. |
| A continued tool is reported interrupted | The journal has a started call with an uncertain outcome. Inspect the external system before repeating the operation. |
| An external harness agent is refused | Use native-loop agents throughout the team, including handoff targets. |
| A run stops because progress could not be recorded | Check disk space, database permissions/availability, and storage errors. Fix the cause before continuing. A failure before the first record was saved leaves nothing to recover. |
| A run stalls around checkpointing or shutdown | Ensure the host keeps consuming events through channel closure and has not closed the store early. |
| Repeated continuation gives up | Inspect the underlying failure rather than blindly retrying. Five consecutive crashes at the same progress point exhaust recovery. |
| Cost changes after recovery | Repeated model calls can incur new charges. Paid calls remain billed after transcript truncation; subsequent usage updates recompute the stored cost column from recorded items. |

Streaming deltas are persisted best-effort; checkpoints wait for the final
message write. Treat displayed output as provisional until its step is
recorded: recovery can replace streamed output from an unfinished step. A model
request retried after it began streaming can also leave an earlier attempt's
row in stored history alongside the completed attempt.

At normal completion, session metadata such as cost and token counts retains
the local runtime's best-effort update behavior; the durable guarantee concerns
the transcript and run checkpoints. Preserve the store's usage metadata when
writing session updates after a run.
