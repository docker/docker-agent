---
title: "Go SDK"
description: "Use Docker Agent as a Go library to embed AI agents in your applications."
keywords: docker agent, ai agents, guides, go sdk
weight: 40
canonical: https://docs.docker.com/ai/docker-agent/guides/go-sdk/
---

_Use Docker Agent as a Go library to embed AI agents in your applications._

## Overview

Docker Agent can be used as a Go library, allowing you to build AI agents directly into your Go applications. This gives you full programmatic control over agent creation, tool integration, and execution.

> [!NOTE]
> **Import Path**
>
> ```go
> import "github.com/docker/docker-agent/pkg/..."
> ```

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
| `pkg/embeddedchat`     | Headless chat session for embedding the agent runtime in a custom UI |
| `pkg/tui/components/toolconfirm` | Tool-confirmation policy: `Decision` enum, `BuildPermissionPattern`, key bindings, and rejection-reason presets. Share this instead of copying the permission-pattern logic. |
| `pkg/tui/service`      | `StaticSessionState` — a `SessionStateReader` with conservative fixed values, for rendering message/tool views outside the full TUI app. Replaces hand-rolled nine-method stubs. |
| `pkg/tui/animation`    | `Stopper` / `StopView` — animation lifecycle contract. Call `StopAnimation` on views removed from the UI to prevent leaked tick subscriptions. |
| `pkg/tui/components/transcript` | Embedded transcript view with read-only `Messages()` accessor for observing conversation structure in host tests and persistence layers. |

## Embedding TUI Components

When building custom UIs on top of Docker Agent's TUI primitives, four packages define the contracts that keep the runtime and the UI in sync:

- **`pkg/tui/components/toolconfirm`** — import this package for the permission-decision policy rather than copying the pattern-building logic. The `Decision` enum, `BuildPermissionPattern` helper, and rejection-reason presets are the canonical source of truth: whatever pattern is shown to the user in the confirmation dialog is exactly the pattern granted to the runtime.
- **`pkg/tui/service`** — use `StaticSessionState` as a stub `SessionStateReader` when rendering individual message or tool views outside the full TUI app. It returns conservative fixed values for all nine interface methods, eliminating the need for hand-rolled stubs.
- **`pkg/tui/animation`** — implement `animation.Stopper` on any view that owns a tick-based animation. Call `StopAnimation` whenever a view is removed from the UI hierarchy to prevent leaked `time.Tick` subscriptions from firing against a dead view.
- **`pkg/tui/components/transcript`** — embed the transcript view for displaying conversation history. Use the `Messages()` method to read the current slice of transcript messages (treat as read-only — mutations desync renders). This is useful for host-side tests asserting on chat history, and for persistence layers that need to snapshot conversation state.

## Headless Embedded Chat (`pkg/embeddedchat`)

`pkg/embeddedchat` is a thin wrapper around the Docker Agent runtime that lets you drive an agent from your own UI instead of running Docker Agent's Bubble Tea application. It handles runtime construction, event projection, and conversation state, exposing a simple `Send` / `Confirm` / `Restart` / `Close` API.

### Creating a session

```go
import (
    "context"
    "fmt"
    "strings"

    dagentcfg "github.com/docker/docker-agent/pkg/config"
    dagentruntime "github.com/docker/docker-agent/pkg/runtime"
    "github.com/docker/docker-agent/pkg/embeddedchat"
    "github.com/docker/docker-agent/pkg/embeddedchat/defaults"
)

chat, err := embeddedchat.New(ctx, embeddedchat.Config{
    // AgentSource can be a file path, raw YAML bytes, or an OCI reference.
    AgentSource: dagentcfg.NewBytesSource("agent", []byte(agentYAML)),
    LoadOpts: defaults.Opts(),
})
if err != nil {
    return err
}
defer chat.Close()
```

### Sending a message and reading events

`Send` appends the user message to the conversation and returns a channel of `Event` values. Drain the channel until it closes.

```go
events, err := chat.Send(ctx, "Hello! What can you do?")
if err != nil {
    return err
}

var response strings.Builder
for ev := range events {
    switch {
    case ev.Text != "":
        response.WriteString(ev.Text)
    case ev.Tool != nil && ev.Tool.NeedsConfirmation:
        // Approve the pending tool call (use ResumeApproveSession to allow all).
        if err := chat.Confirm(ctx, dagentruntime.ResumeApprove()); err != nil {
            return err
        }
    case ev.Tool != nil && ev.Tool.Finished:
        fmt.Printf("[tool %s finished]\n", ev.Tool.Def.Name)
    case ev.Err != nil:
        fmt.Printf("error: %v\n", ev.Err)
    case ev.Done:
        fmt.Println("\n[turn complete]")
    }
}
fmt.Print(response.String())
```

### Restarting the conversation

To start a fresh conversation without recreating the runtime:

```go
if err := chat.Restart(); err != nil {
    return err
}
```

### Event types

| Field          | When set                                                                 |
| -------------- | ------------------------------------------------------------------------ |
| `Text`         | Assistant text delta; accumulate into a string for the full reply.       |
| `Tool`         | A tool call started, needs confirmation, or finished.                    |
| `Tool.NeedsConfirmation` | Runtime is blocked until `Confirm` is called.              |
| `Tool.Finished` | Tool call completed; `Tool.IsError` is true if it errored.             |
| `Err`          | A user-facing runtime error; no further content events follow.           |
| `Done`         | Clean end of turn; no more events.                                       |
| `RuntimeEvent` | The original `runtime.Event` for callers that need the full stream.      |

For advanced use (custom elicitation, raw event inspection), call `chat.Runtime()` to access the underlying `runtime.Runtime` directly.

> [!WARNING]
> **Breaking change: `Runtime.ResumeElicitation` (#3584)**
>
> `Runtime.ResumeElicitation` gained an `elicitationID` parameter so responses can
> be correlated with a specific concurrent elicitation request (needed once
> multiple background jobs can be eliciting input at the same time). It is
> declared **variadic** (`elicitationID ...string`) specifically so existing
> *callers* of the 3-argument form keep compiling unchanged — `rt.ResumeElicitation(ctx, action, content)`
> still works and falls back to resolving the sole pending request.
>
> If you implement your own `runtime.Runtime` (rather than embedding
> `runtime.LocalRuntime`/`runtime.RemoteRuntime`), you do need to update your
> method's signature to match, and also add an `OnElicitationRequest(handler
> func(runtime.Event))` method (a no-op is fine if your runtime never raises
> elicitations) — both are required interface methods, matching the existing
> no-op-able pattern already used by `OnToolsChanged`/`OnBackgroundEvent`.

## RAG Toolset (opt-out)

The RAG toolset (`type: rag`) is included in `NewDefaultToolsetRegistry()` (from `pkg/teamloader/toolsets`) and `loaderdefaults.Opts()` (from `pkg/teamloader/defaults`, using the conventional import alias `loaderdefaults`).

The underlying tree-sitter code parser uses cgo, but build-tag guards in `pkg/rag/treesitter` mean importing the package is safe regardless of `CGO_ENABLED`: with `CGO_ENABLED=0` the parser stub compiles in and returns a runtime error on first use rather than failing at compile time.

To disable the RAG toolset at runtime — surfacing a load-time warning rather than a deferred error from the `!cgo` stub — remove it from the registry before passing it to `teamloader.Load`. This does not remove its package dependencies; use a hand-picked registry without importing the full defaults for that:

```go
import (
    "github.com/docker/docker-agent/pkg/teamloader"
    loadertoolsets "github.com/docker/docker-agent/pkg/teamloader/toolsets"
)

// Opt out of the RAG toolset; a config that declares type: rag attaches
// a load-time warning to the agent instead of failing at document processing.
creators := loadertoolsets.DefaultToolsetCreators()
delete(creators, "rag")
registry := teamloader.NewToolsetRegistry(creators)
```

Pass the custom registry via `teamloader.WithToolsetRegistry(registry)` when calling `teamloader.Load`. Note that `teamloader.Load()` does not return an error for unknown toolset types unless `teamloader.WithStrict` is set — the failure is recorded as a load-time warning and can be retrieved with `agent.DrainWarnings()`; it is also surfaced via logging and TUI notifications.

## Loading YAML with Hand-Picked Registries (lean embedding)

`loaderdefaults.Opts()` links every provider SDK, every built-in toolset and every agent-source type. When you embed docker-agent and load agents from YAML (a file shipped in your binary, or an OCI artifact), you can instead declare exactly what your binary supports and have docker-agent reject anything else **before** any model or toolset is built:

```go
import (
    "github.com/docker/docker-agent/pkg/config"
    "github.com/docker/docker-agent/pkg/config/ocisource"
    "github.com/docker/docker-agent/pkg/model/provider"
    "github.com/docker/docker-agent/pkg/model/provider/anthropic"
    "github.com/docker/docker-agent/pkg/teamloader"
    "github.com/docker/docker-agent/pkg/tools/builtin/api/client"
    "github.com/docker/docker-agent/pkg/tools/builtin/think"
)

team, err := teamloader.Load(ctx, ocisource.New("myorg/agent:v1"), runConfig,
    teamloader.WithProviderRegistry(provider.NewRegistry(map[string]provider.Factory{
        "anthropic": provider.Adapt(anthropic.NewClient),
    })),
    teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{
        "api":   client.Creator(teamloader.NewEnvExpander),
        "think": teamloader.Creator(think.CreateToolSet),
    })),
    // Deny every optional feature; pass e.g. config.FeatureSkills to allow one.
    teamloader.WithStrict(),
)
```

- **Providers**: every provider package's `NewClient` becomes a `provider.Factory` through `provider.Adapt`. Providers are matched on the type the registry resolves them to — a custom `providers:` entry or an alias such as `mistral` counts as `openai`. `providers.DefaultFactories()` (from `pkg/model/provider/providers`) is the full table if you prefer to copy and trim it.
- **Toolsets**: built-in toolset packages whose constructor needs the runtime config (plus `pkg/tools/mcp` and `pkg/tools/a2a`) export a `Creator` matching `teamloader.ToolsetCreator`; the config-free ones (`think`, `todo`, `plan`, ...) are wrapped with `teamloader.Creator(think.CreateToolSet)` or `teamloader.CreatorFromToolset(todo.CreateToolSet)`, which keeps those packages free of `pkg/config` so they still cross-compile to wasm/plan9. `toolsets.DefaultToolsetCreators()` (from `pkg/teamloader/toolsets`) is the full table.
- **Agent sources**: `config.NewFileSource` / `config.NewBytesSource` / `config.NewURLSource` live in `pkg/config`; the OCI source lives in `pkg/config/ocisource`; HCL support is a source decorator, `hcl.NewSource(inner)`, in `pkg/config/hcl`. `pkg/config/sources` resolves any reference (files, directories, URLs, OCI, user aliases, built-in agents) at the cost of linking all of them. Sub-agents referencing external agents (`sub_agents: [myorg/reviewer]`) need a `teamloader.WithSourceResolver` that wraps `sources.Resolve` (as `loaderdefaults.Opts()` does) — or your own resolver.
- **Optional loader features**: everything the loader used to link unconditionally is now an option, so `pkg/teamloader` adds no module over `pkg/runtime`. Enable what your configs use: `teamloader.WithExpander(js.NewJsExpander)` for `${...}` JavaScript in instructions/commands (the default only resolves `${env.NAME}`; also call `jscommands.Register()` for slash commands), `teamloader.WithCodeMode(codemode.Wrap)` for `code_mode_tools`, `teamloader.WithToon(toon.Wrap)` for the `toon` field, `teamloader.WithDeferredTools(deferred.New)` for `defer`, and `runtime.RegisterHarness(codingharness.Factory)` for `harness:` agents. A config that uses a feature you did not enable fails to load with an error naming the option.
- **Strict mode**: `teamloader.WithStrict(features...)` fails the load with a `*config.UnsupportedError` listing **every** provider type, toolset type and optional feature the config relies on that you did not enable, with the config locations that need them. Optional features are `config.FeatureHooks`, `config.FeatureHarness`, `config.FeatureSkills`, `config.FeatureExternalAgents`, `config.FeatureCodeMode`, `config.FeatureToon` and `config.FeatureDeferredTools`; none is enabled unless listed. Agents on the `auto` model are resolved eagerly so the provider they land on is checked too. Without `WithStrict`, unknown toolset types stay load-time warnings and unknown providers fail when their model is built.

`config.Requires(cfg)` exposes the same audit for your own checks. `pkg/embeddedchat` accepts all of this through `Config.LoadOpts`. A complete example lives in [examples/golibrary/yamlstrict](https://github.com/docker/docker-agent/tree/main/examples/golibrary/yamlstrict).

> [!WARNING]
> **Breaking change: agent sources moved out of `pkg/config`**
>
> `config.Resolve`, `config.ResolveSources`, `config.ResolveAlias` and `config.BuiltinAgentNames` are now in `pkg/config/sources`; `config.NewOCISource` is `ocisource.New` in `pkg/config/ocisource`; and `config.Load` no longer auto-detects HCL — wrap the source with `hcl.NewSource` (which `sources.Resolve` does for you). `teamloader.ToolsetRegistry` gained a `Has(toolsetType string) bool` method.
>
> If you call `teamloader.Load` without `loaderdefaults.Opts()`, JavaScript expansion, code mode, TOON, deferred tools and harness agents are now off until you enable them (see *Optional loader features* above). Code-built teams that use `harness:` agents must call `runtime.RegisterHarness(codingharness.Factory)`; `codingharness.Label` moved into the runtime.

### Per-runtime feature configuration

Prefer instance options over `runtime.RegisterHarness` and
`runtime.RegisterCommandEvaluator`, which affect the entire process:

```go
rt, err := runtime.New(ctx, team,
    runtime.WithProviderRegistry(providers),
    runtime.WithHarnessFactory(codingharness.Factory),
    runtime.WithCommandEvaluatorFactory(jscommands.Factory),
)
```

Import `pkg/codingharness` or `pkg/runtime/jscommands` only when needed.
Passing `nil` to either factory option explicitly disables that feature for
this runtime, even if another caller registered a global default. Omitting the
options retains the legacy global fallback, including registrations made after
runtime construction. Factories are still invoked lazily, at execution time.
Runtime decorators used with `ResolveCommand` should forward
`CommandEvaluatorFactory() runtime.CommandEvaluatorFactory` to preserve this
selection. The `runtime.Runtime` interface itself is unchanged.

These options also work through `embeddedchat.Config.RuntimeOptions`. Loader
policy remains separate: `teamloader.WithStrict(config.FeatureHarness)` permits
harness declarations but does not install a driver. Likewise, supplying an
implementation does not automatically authorize a feature in strict mode.

### HTTP tools without JavaScript

The `pkg/tools/builtin/api/client` package accepts an expander instead of
importing JavaScript. Register a placeholder-only HTTP tool with:

```go
"api": client.Creator(teamloader.NewEnvExpander),
```

This supports `${env.NAME}` and bound `${argument}` placeholders and resolves
credentials on each request. Unknown placeholders and JavaScript expressions
remain unchanged. `api.Creator` and `api.New` retain the full JavaScript and
upstream-header behavior for existing callers. The leaf package does **not**
automatically expand `${headers.NAME}`; use `client.WithHeaderResolver` to supply
that policy. Passing `upstream.ResolveHeaders` restores the legacy behavior but
also imports JavaScript.

A complete placeholder-only example lives in
[examples/golibrary/leanapi](https://github.com/docker/docker-agent/tree/main/examples/golibrary/leanapi).
The older `yamlstrict` example intentionally retains JavaScript-capable API and
fetch tools.

Selecting the leaf removes four external modules from the HTTP tool's import
closure: Goja, regexp2, go-sourcemap, and pprof. Importing the full defaults and
then deleting registry entries does **not** remove those package dependencies.

Go package dependencies and module requirements are different: these changes
reduce the packages compiled and their required source modules. Docker Agent
still has one `go.mod`, so this does not promise an equally small
`go list -m all` graph or a small `go mod download all`. Independently versioned
optional modules would be a separate packaging change.

## Registering Custom Built-in Themes

When embedding Docker Agent, you can contribute your own built-in themes via `styles.RegisterBuiltinThemes`. Registered themes integrate seamlessly with the existing theme picker, `/theme` command, and `settings.theme` config key — they behave exactly like Docker Agent's own bundled themes.

```go
import (
    "embed"

    "github.com/docker/docker-agent/pkg/tui/styles"
)

//go:embed themes/*.yaml
var brandThemes embed.FS

// Call at startup, before applying any persisted theme:
if err := styles.RegisterBuiltinThemes(brandThemes); err != nil {
    return err
}
```

Each theme file lives at `themes/<name>.yaml` inside the embedded filesystem and is a **partial override** — only the colors you want to change are required; everything else falls back to `DefaultTheme()`.

```yaml
# themes/brand.yaml
name: Brand
colors:
  accent: "#FF6A00"
  background: "#1A0F0A"
```

If `name:` is omitted, Docker Agent uses the filename stem as the display name in the theme picker (e.g. `brand` from `themes/brand.yaml`).

To replace Docker Agent's default theme entirely, ship the file as `themes/default.yaml` — it masks the bundled default while inheriting any colors you don't set.

**Semantics:**

- Registered sources take precedence over bundled themes; a registered ref overrides a bundled theme of the same name.
- Among multiple registered sources, last-registered wins on a collision.
- `RegisterBuiltinThemes` validates eagerly (nil fs, missing `themes/` dir) so errors surface at registration time, not at picker time.

## MCP OAuth Token Persistence

By default, MCP OAuth tokens are stored in-memory only and are not persisted across process restarts. The CLI registers a keyring-backed store automatically at startup; when embedding Docker Agent as a library you must do this yourself if you want tokens to survive restarts.

Call `keyringstore.Register()` **before** any MCP toolset is initialised to enable the OS keyring-backed token store:

```go
import "github.com/docker/docker-agent/pkg/tools/mcp/keyringstore"

func main() {
    // Must be called before teamloader.Load() on configs with remote MCP
    // toolsets; calling it after the store is created panics.
    keyringstore.Register()
    // ... rest of your startup code
}
```

> [!WARNING]
> **Call order matters**
>
> If `keyringstore.Register()` is called after the default token store has already been lazily initialised, Docker Agent panics. The store is initialised when any remote MCP toolset is constructed — which happens inside `teamloader.Load()`. Always call `keyringstore.Register()` before calling `teamloader.Load()` on a config that includes remote MCP toolsets.

If you do not need persistent OAuth tokens (for example, in short-lived batch jobs or tests), omit the call and tokens will be kept in-memory for the process lifetime.

## JavaScript Command Expressions (opt-in)

Slash-command instructions can embed `${...}` JavaScript expressions (`${args[0]}`, `${args.join(" ")}`, `${tool({...})}`). Evaluating them requires the goja JavaScript engine, which is deliberately kept out of `pkg/runtime`'s import graph so code-built embedders don't link it by default.

The CLI, `loaderdefaults.Opts()`, `pkg/cli.Run()` and `embeddedchat/defaults` enable it automatically. If you build teams in code, call `runtime.ResolveCommand` (or `cli.PrepareUserMessage`) directly **and** use `${...}` expressions in commands, register the evaluator yourself:

```go
import "github.com/docker/docker-agent/pkg/runtime/jscommands"

func main() {
    jscommands.Register()
    // ... rest of your startup code
}
```

Without the registration, `${...}` expressions are left unexpanded and a warning naming the fix is logged; everything else about command resolution (including the legacy `!tool(...)` syntax) works as usual.

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
    rt, err := runtime.New(ctx, t)
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

    "github.com/docker/docker-agent/pkg/agent"
    "github.com/docker/docker-agent/pkg/model/provider"
    "github.com/docker/docker-agent/pkg/tools"
)

// Define the tool's input schema
type AddNumbersArgs struct {
    A int `json:"a"`
    B int `json:"b"`
}

// Implement the tool handler
func addNumbers(_ context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
    var args AddNumbersArgs
    if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
        return nil, err
    }

    result := args.A + args.B
    return tools.ResultSuccess(fmt.Sprintf("%d", result)), nil
}

func createCalculator(llm provider.Provider) *agent.Agent {
    // Create the tool definition
    addTool := tools.Tool{
        Name:        "add",
        Category:    "math",
        Description: "Add two numbers together",
        Parameters:  tools.MustSchemaFor[AddNumbersArgs](),
        Handler:     addNumbers,
    }

    // Use with an agent
    return agent.New(
        "root",
        "You are a calculator. Use the add tool for arithmetic.",
        agent.WithModel(llm),
        agent.WithTools(addTool),
    )
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
    "github.com/docker/docker-agent/pkg/model/provider"
    "github.com/docker/docker-agent/pkg/team"
    "github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
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
        agent.WithToolSets(transfertask.New()),
    )

    return team.New(team.WithAgents(coordinator, researcher))
}
```

## Built-in Tools

Use Docker Agent's built-in tools:

```go
import (
    "os"

    "github.com/docker/docker-agent/pkg/agent"
    "github.com/docker/docker-agent/pkg/config"
    "github.com/docker/docker-agent/pkg/model/provider"
    "github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
    "github.com/docker/docker-agent/pkg/tools/builtin/shell"
    "github.com/docker/docker-agent/pkg/tools/builtin/think"
    "github.com/docker/docker-agent/pkg/tools/builtin/todo"
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
            shell.New(os.Environ(), rtConfig),
            // Filesystem tools
            filesystem.New(rtConfig.Config.WorkingDir),
            // Think tool for reasoning
            think.New(),
            // Todo tool for task tracking
            todo.New(),
        ),
    )
}
```

## HTTP Middleware / Transport Wrappers

Use `options.WithHTTPTransportWrapper` to inject HTTP middleware into the transport chain of all provider clients built by Docker Agent. This is useful for request tracing, injecting custom headers, collecting metrics, or any other cross-cutting concern at the HTTP layer.

```go
import (
    "net/http"

    "github.com/docker/docker-agent/pkg/model/provider/options"
)

type headerTransport struct {
    base http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
    req = req.Clone(req.Context())
    req.Header.Set("X-Request-Source", "my-app")
    return t.base.RoundTrip(req)
}

// Example: add a custom header to every outbound LLM request
wrapper := options.WithHTTPTransportWrapper(
    func(base http.RoundTripper) http.RoundTripper {
        return &headerTransport{base: base}
    },
)

client, err := openai.NewClient(ctx, &latest.ModelConfig{
    Provider: "openai",
    Model:    "gpt-4o",
}, env, wrapper)
```

The wrapper receives the already-instrumented transport (OpenTelemetry, SSE decompression, Desktop proxy support) as its `base` argument, so wrapping it preserves all built-in behaviour.

**Supported providers:** Anthropic, OpenAI, Gemini (GeminiAPI backend), Bedrock. Works in both direct and gateway/proxy mode.

> [!WARNING]
> **Vertex AI not supported**
>
> Vertex AI uses an ADC-managed HTTP client that Docker Agent cannot intercept. When a transport wrapper is set, Docker Agent falls back to the GeminiAPI backend instead of Vertex AI — a debug message is logged.

In **gateway mode** the wrapper is called on every LLM request because gateway clients are rebuilt each call for short-lived auth tokens. In **direct mode** it is called once at client construction. Rate-limit responses (HTTP 429) are classified as non-retryable by the runtime and cause the model chain to skip to the next fallback, so wrappers that track per-request outcomes will observe these as failures rather than retried calls.

Returning `nil` from your wrapper function is not allowed; Docker Agent logs a warning and keeps the original transport instead.

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
    Model:    "gemini-3.5-flash",
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
- `yamlstrict/` — Loading YAML (file or OCI) with hand-picked providers/toolsets and strict mode
