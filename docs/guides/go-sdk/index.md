---
title: "Go SDK"
description: "Use docker-agent as a Go library to embed AI agents in your applications."
permalink: /guides/go-sdk/
---

# Go SDK

_Use docker-agent as a Go library to embed AI agents in your applications._

## Overview

docker-agent can be used as a Go library, allowing you to build AI agents directly into your Go applications. This gives you full programmatic control over agent creation, tool integration, and execution.

<div class="callout callout-info" markdown="1">
<div class="callout-title">Import Path
</div>
<pre><code class="language-go">import "github.com/docker/docker-agent/pkg/..."</code></pre>
</div>

## Core Packages

| Package                | Purpose                                  |
| ---------------------- | ---------------------------------------- |
| `pkg/agent`            | Agent creation and configuration         |
| `pkg/runtime`          | Agent execution and event streaming      |
| `pkg/session`          | Conversation state management            |
| `pkg/team`             | Multi-agent team composition             |
| `pkg/tools`            | Tool interface and utilities             |
| `pkg/tools/builtin`    | Built-in tools (shell, filesystem, etc.) |
| `pkg/model/provider/*` | Model provider clients                   |
| `pkg/config/latest`    | Configuration types                      |
| `pkg/environment`      | Environment and secrets                  |

## Basic Example

Create a simple agent and run it:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os/signal"
    "syscall"

    "github.com/docker/docker-agent/pkg/agent"
    "github.com/docker/docker-agent/pkg/config/latest"
    "github.com/docker/docker-agent/pkg/environment"
    "github.com/docker/docker-agent/pkg/model/provider/openai"
    "github.com/docker/docker-agent/pkg/runtime"
    "github.com/docker/docker-agent/pkg/session"
    "github.com/docker/docker-agent/pkg/team"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    if err := run(ctx); err != nil {
        log.Fatal(err)
    }
}

func run(ctx context.Context) error {
    // Create model provider
    llm, err := openai.NewClient(
        ctx,
        &latest.ModelConfig{
            Provider: "openai",
            Model:    "gpt-4o",
        },
        environment.NewDefaultProvider(),
    )
    if err != nil {
        return err
    }

    // Create agent
    assistant := agent.New(
        "root",
        "You are a helpful assistant.",
        agent.WithModel(llm),
        agent.WithDescription("A helpful assistant"),
    )

    // Create team and runtime
    t := team.New(team.WithAgents(assistant))
    rt, err := runtime.New(t)
    if err != nil {
        return err
    }

    // Run with a user message
    sess := session.New(
        session.WithUserMessage("What is 2 + 2?"),
    )

    messages, err := rt.Run(ctx, sess)
    if err != nil {
        return err
    }

    // Print the response
    fmt.Println(messages[len(messages)-1].Message.Content)
    return nil
}
```

## Custom Tools

Define custom tools for your agent:

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"

    "github.com/docker/docker-agent/pkg/tools"
)

// Define the tool's input schema
type AddNumbersArgs struct {
    A int `json:"a"`
    B int `json:"b"`
}

// Implement the tool handler
func addNumbers(_ context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
    var args AddNumbersArgs
    if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
        return nil, err
    }

    result := args.A + args.B
    return tools.ResultSuccess(fmt.Sprintf("%d", result)), nil
}

func main() {
    // Create the tool definition
    addTool := tools.Tool{
        Name:        "add",
        Category:    "math",
        Description: "Add two numbers together",
        Parameters:  tools.MustSchemaFor[AddNumbersArgs](),
        Handler:     addNumbers,
    }

    // Use with an agent
    calculator := agent.New(
        "root",
        "You are a calculator. Use the add tool for arithmetic.",
        agent.WithModel(llm),
        agent.WithTools(addTool),
    )
    // ...
}
```

## Streaming Responses

Process events as they happen:

```go
func runStreaming(ctx context.Context, rt runtime.Runtime, sess *session.Session) error {
    events := rt.RunStream(ctx, sess)

    for event := range events {
        switch e := event.(type) {
        case *runtime.StreamStartedEvent:
            fmt.Println("Stream started")

        case *runtime.AgentChoiceEvent:
            // Print response chunks as they arrive
            fmt.Print(e.Content)

        case *runtime.ToolCallEvent:
            fmt.Printf("\n[Tool call: %s]\n", e.ToolCall.Function.Name)

        case *runtime.ToolCallConfirmationEvent:
            // Auto-approve tool calls
            rt.Resume(ctx, runtime.ResumeRequest{
                Type: runtime.ResumeTypeApproveSession,
            })

        case *runtime.ToolCallResponseEvent:
            fmt.Printf("[Tool response: %s]\n", e.Response)

        case *runtime.StreamStoppedEvent:
            fmt.Println("\nStream stopped")

        case *runtime.ErrorEvent:
            return fmt.Errorf("error: %s", e.Error)
        }
    }

    return nil
}
```

## Multi-Agent Teams

Create agents that delegate to sub-agents:

```go
package main

import (
    "github.com/docker/docker-agent/pkg/agent"
    "github.com/docker/docker-agent/pkg/team"
    "github.com/docker/docker-agent/pkg/tools/builtin"
)

func createTeam(llm provider.Provider) *team.Team {
    // Create a child agent
    researcher := agent.New(
        "researcher",
        "You research topics thoroughly.",
        agent.WithModel(llm),
        agent.WithDescription("Research specialist"),
    )

    // Create root agent with sub-agents
    coordinator := agent.New(
        "root",
        "You coordinate research tasks.",
        agent.WithModel(llm),
        agent.WithDescription("Team coordinator"),
        agent.WithSubAgents(researcher),
        agent.WithToolSets(builtin.NewTransferTaskTool()),
    )

    return team.New(team.WithAgents(coordinator, researcher))
}
```

## Built-in Tools

Use docker-agent's built-in tools:

```go
import (
    "github.com/docker/docker-agent/pkg/config"
    "github.com/docker/docker-agent/pkg/tools/builtin"
)

func createAgentWithBuiltinTools(llm provider.Provider) *agent.Agent {
    // Runtime config for tools that need it
    rtConfig := &config.RuntimeConfig{
        Config: config.Config{
            WorkingDir: "/path/to/workdir",
        },
    }

    return agent.New(
        "root",
        "You are a developer assistant.",
        agent.WithModel(llm),
        agent.WithToolSets(
            // Shell tool for running commands
            builtin.NewShellTool(os.Environ(), rtConfig),
            // Filesystem tools
            builtin.NewFilesystemTool(rtConfig.Config.WorkingDir),
            // Think tool for reasoning
            builtin.NewThinkTool(),
            // Todo tool for task tracking
            builtin.NewTodoTool(),
        ),
    )
}
```

## Using Different Providers

```go
import (
    "github.com/docker/docker-agent/pkg/model/provider/anthropic"
    "github.com/docker/docker-agent/pkg/model/provider/gemini"
    "github.com/docker/docker-agent/pkg/model/provider/openai"
)

// OpenAI
openaiClient, _ := openai.NewClient(ctx, &latest.ModelConfig{
    Provider: "openai",
    Model:    "gpt-4o",
}, env)

// Anthropic
anthropicClient, _ := anthropic.NewClient(ctx, &latest.ModelConfig{
    Provider: "anthropic",
    Model:    "claude-sonnet-4-5",
}, env)

// Google Gemini
geminiClient, _ := gemini.NewClient(ctx, &latest.ModelConfig{
    Provider: "google",
    Model:    "gemini-2.5-flash",
}, env)
```

## Session Options

```go
import "github.com/docker/docker-agent/pkg/session"

sess := session.New(
    // Set a title for the session
    session.WithTitle("Code Review Task"),

    // Add user message
    session.WithUserMessage("Review this code for bugs"),

    // Limit iterations
    session.WithMaxIterations(20),
)
```

## Error Handling

```go
messages, err := rt.Run(ctx, sess)
if err != nil {
    if errors.Is(err, context.Canceled) {
        // User cancelled
        log.Println("Operation cancelled")
        return nil
    }
    if errors.Is(err, context.DeadlineExceeded) {
        // Timeout
        log.Println("Operation timed out")
        return nil
    }
    // Other error
    return fmt.Errorf("runtime error: %w", err)
}

// Check for errors in the event stream
for event := range rt.RunStream(ctx, sess) {
    if errEvent, ok := event.(*runtime.ErrorEvent); ok {
        return fmt.Errorf("stream error: %s", errEvent.Error)
    }
}
```

## Complete Example

See the [examples/golibrary](https://github.com/docker/docker-agent/tree/main/examples/golibrary) directory for complete working examples:

- `simple/` — Basic agent with no tools
- `tool/` — Custom tool implementation
- `stream/` — Streaming event handling
- `multi/` — Multi-agent with sub-agents
- `builtintool/` — Using built-in tools
