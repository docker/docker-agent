---
title: "Build a Durable Worker"
description: "Run a complete Go example that saves agent progress, suspends on shutdown, and recovers with SQLite."
weight: 42
canonical: https://docs.docker.com/ai/docker-agent/guides/durable-runtime-quickstart/
---

This example creates a single-agent worker using the
[durable runtime](../durable-runtime/index.md). It can start a conversation, recover recorded
runs, or discard an unfinished run. You control the worker through its
`start`, `recover`, and `discard` commands.

## Create the Program

Create an empty project:

```bash
$ mkdir durable-worker
$ cd durable-worker
$ go mod init example.com/durable-worker
$ go get github.com/docker/docker-agent@main
$ export ANTHROPIC_API_KEY='your-api-key'
```

Save this as `main.go`. The example uses a text-only agent to demonstrate
model-step recovery. `run` opens the resources and dispatches to `startRun`,
`recoverRuns`, or `discardRun`. Runtime setup, signal handling, and event
consumption each have their own function.

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/docker/docker-agent/pkg/agent"
    "github.com/docker/docker-agent/pkg/config/latest"
    "github.com/docker/docker-agent/pkg/environment"
    "github.com/docker/docker-agent/pkg/model/provider/anthropic"
    "github.com/docker/docker-agent/pkg/runtime"
    "github.com/docker/docker-agent/pkg/session"
    "github.com/docker/docker-agent/pkg/session/sqlitestore"
    "github.com/docker/docker-agent/pkg/team"
)

func main() {
    if err := run(); err != nil {
        log.Fatal(err)
    }
}

func run() error {
    if len(os.Args) < 2 {
        return errors.New("usage: durable-worker start | recover | discard SESSION_ID")
    }
    // Keep startup and storage cleanup independent of the shutdown signal.
    ctx := context.Background()
    store, err := sqlitestore.New(ctx, "./data/sessions.db")
    if err != nil {
        return err
    }
    defer store.Close()

    rt, err := newRuntime(ctx, store)
    if err != nil {
        return err
    }
    defer rt.Close()

    workCtx, cleanup := suspendOnSignal(ctx)
    defer cleanup()

    switch os.Args[1] {
    case "start":
        return startRun(workCtx, rt, store.(session.RunStore))
    case "recover":
        return recoverRuns(workCtx, rt, store.(session.RunStore))
    case "discard":
        return discardRun(ctx, rt, os.Args[2:])
    default:
        return fmt.Errorf("unknown command %q", os.Args[1])
    }
}

func newRuntime(ctx context.Context, store session.Store) (*runtime.DurableRuntime, error) {
    llm, err := anthropic.NewClient(ctx, &latest.ModelConfig{
        Provider: "anthropic",
        Model:    "claude-sonnet-4-5",
    }, environment.NewOsEnvProvider())
    if err != nil {
        return nil, err
    }
    root := agent.New("root", "You explain technical topics clearly.", agent.WithModel(llm))
    agents := team.New(team.WithAgents(root))
    return runtime.NewDurableRuntime(ctx, agents,
        runtime.WithSessionStore(store),
        runtime.WithNonInteractive(true),
    )
}

func suspendOnSignal(parent context.Context) (context.Context, func()) {
    // The caller supplies a context independent of shutdown cancellation,
    // so the signal handler can suspend work with ErrSuspended.
    ctx, stop := context.WithCancelCause(parent)
    signals := make(chan os.Signal, 1)
    signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
    go func() {
        select {
        case <-signals:
            stop(runtime.ErrSuspended)
        case <-ctx.Done():
        }
    }()
    return ctx, func() {
        signal.Stop(signals)
        stop(nil)
    }
}

func startRun(ctx context.Context, rt *runtime.DurableRuntime, runs session.RunStore) error {
    pending, err := runs.ListRuns(ctx)
    if err != nil {
        return err
    }
    if len(pending) != 0 {
        return errors.New("unfinished runs exist; recover or discard them first")
    }
    sess := session.New(
        session.WithAgentName("root"),
        session.WithNonInteractive(true),
        session.WithSafetyPolicy(session.SafetyPolicyRestricted),
        session.WithUserMessage("Explain how database write-ahead logs help recovery."),
    )
    fmt.Println("Session:", sess.ID)
    return drain(rt.RunStream(ctx, sess))
}

func recoverRuns(ctx context.Context, rt *runtime.DurableRuntime, runs session.RunStore) error {
    // Establish exclusive database ownership before recovering runs.
    pending, err := runs.ListRuns(ctx)
    if err != nil {
        return err
    }
    for _, record := range pending {
        if ctx.Err() != nil {
            break
        }
        sess, events, err := rt.ContinueStream(ctx, record.SessionID,
            session.WithNonInteractive(true),
            session.WithSafetyPolicy(session.SafetyPolicyRestricted),
        )
        if err != nil {
            return fmt.Errorf("continue %s: %w", record.SessionID, err)
        }
        fmt.Println("Continuing:", sess.ID)
        if err := drain(events); err != nil {
            return err
        }
    }
    return nil
}

func discardRun(ctx context.Context, rt *runtime.DurableRuntime, args []string) error {
    if len(args) != 1 {
        return errors.New("usage: durable-worker discard SESSION_ID")
    }
    return rt.DiscardRun(ctx, args[0])
}

func drain(events <-chan runtime.Event) error {
    var streamErr error
    // Keep reading even after an error or shutdown signal. Checkpointing
    // and settlement must finish before the runtime and store close.
    for event := range events {
        switch e := event.(type) {
        case *runtime.AgentChoiceEvent:
            fmt.Print(e.Content)
        case *runtime.ErrorEvent:
            log.Print(e.Error)
            streamErr = errors.Join(streamErr, errors.New(e.Error))
        }
    }
    fmt.Println()
    return streamErr
}
```

The application owns the store and closes it after the runtime. A larger
application also owns the lifetime of its team's toolsets.

The example deliberately reapplies a restrictive, non-interactive policy on
recovery. Options passed to `ContinueStream` apply **after** restoring the
checkpoint's approval state, so they can change what the continued run may do.
Saved permission grants remain in effect alongside the restricted safety
policy. To replace those grants, pass `session.WithPermissions` with your
chosen permissions to `ContinueStream`. The recorded agent and iteration
limit stay in effect.

## Run, Suspend, and Recover

```bash
$ go mod tidy
$ go build -o durable-worker .
$ ./durable-worker start
```

While the model is still answering, press **Ctrl+C**. The signal handler cancels
the run with `runtime.ErrSuspended`, then the program drains its stream before
closing resources. Completed runs retain their session history; suspended runs
remain available for recovery.

Restart in the same directory, with the same database and credentials:

```bash
$ ./durable-worker recover
```

The program lists run records and continues them one at a time with
`ContinueStream`, using each run's recorded input. An interrupted model call is
made again; the replacement answer can differ from the partial one.
After completion, recovery finds no records, but the session history remains.

To try crash recovery instead, forcibly stop the process while it is answering
(for example, with `kill -KILL PID` from another terminal), then run `recover`.
Unlike graceful suspension, this counts as a crash on continuation.

To abandon an unfinished run, use the ID printed when it started:

```bash
$ ./durable-worker discard SESSION_ID
```

Discarding removes the run record and returns its stored conversation to the
last checkpoint. The session history and external actions remain.

## Add Your Own Team

Replace the code-built team with your existing team loader, or add tools using
the [Go SDK](../go-sdk/index.md). Use the same agent names and compatible
configuration when rebuilding a team for recovery. Supply the agent definitions,
provider credentials, and workspace alongside the recorded progress.

If you add tools, keep an explicit [approval policy](../../configuration/permissions/index.md).
`runtime.WithNonInteractive(true)` declines runtime elicitation requests, while
`session.WithNonInteractive(true)` selects unattended tool approval. The restricted policy
denies calls that require confirmation. An interactive host must handle
confirmations and elicitations through the normal runtime APIs while continuing
to drain events.
