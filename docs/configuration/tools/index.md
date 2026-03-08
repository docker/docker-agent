---
title: "Tool Configuration"
description: "Complete reference for configuring built-in tools, MCP tools, and Docker-based tools."
permalink: /configuration/tools/
---

# Tool Configuration

_Complete reference for configuring built-in tools, MCP tools, and Docker-based tools._

## Built-in Tools

Built-in tools are included with docker-agent and require no external dependencies. Add them to your agent's `toolsets` list.

### Filesystem

Read, write, list, search, and navigate files in the working directory.

```yaml
toolsets:
  - type: filesystem
    ignore_vcs: false # Optional: ignore .gitignore files
    post_edit: # Optional: run commands after file edits
      - path: "*.go"
        cmd: "gofmt -w ${file}"
```

| Operation              | Description                                                               |
| ---------------------- | ------------------------------------------------------------------------- |
| `read_file`            | Read the complete contents of a file                                      |
| `read_multiple_files`  | Read several files in one call (more efficient than multiple `read_file`) |
| `write_file`           | Create or overwrite a file with new content                               |
| `edit_file`            | Make line-based edits (find-and-replace) in an existing file              |
| `list_directory`       | List files and directories at a given path                                |
| `directory_tree`       | Recursive tree view of a directory                                        |
| `search_files_content` | Search for text or regex patterns across files                            |

| Property           | Type    | Default | Description                                                       |
| ------------------ | ------- | ------- | ----------------------------------------------------------------- |
| `ignore_vcs`       | boolean | `false` | When `true`, ignores `.gitignore` patterns and includes all files |
| `post_edit`        | array   | `[]`    | Commands to run after editing files matching a path pattern       |
| `post_edit[].path` | string  | —       | Glob pattern for files (e.g., `*.go`, `src/**/*.ts`)              |
| `post_edit[].cmd`  | string  | —       | Command to run (use `${file}` for the edited file path)           |

<div class="callout callout-tip">
<div class="callout-title">💡 Tip
</div>
  <p>The filesystem tool resolves paths relative to the working directory. Agents can also use absolute paths.</p>

</div>

### Shell

Execute arbitrary shell commands. Each call runs in a fresh, isolated shell session — no state persists between calls.

```yaml
toolsets:
  - type: shell
    env: # Optional: environment variables
      MY_VAR: "value"
      PATH: "${PATH}:/custom/bin"
```

The agent has access to the full system shell and environment variables. Commands have a default 30-second timeout. Requires user confirmation unless `--yolo` is used.

| Property  | Type   | Description                                                                      |
| --------- | ------ | -------------------------------------------------------------------------------- |
| `env`     | object | Environment variables to set for all shell commands                              |

### Think

Step-by-step reasoning scratchpad. The agent writes its thoughts without producing visible output — ideal for planning, decomposition, and decision-making.

```yaml
toolsets:
  - type: think
```

No configuration options. No side effects. Recommended for all agents — adds minimal overhead while improving reasoning quality.

### Todo

Task list management. Agents can create, update, and track tasks with status (pending, in-progress, completed).

```yaml
toolsets:
  - type: todo
    shared: false # Optional: share todos across agents
```

| Operation      | Description                              |
| -------------- | ---------------------------------------- |
| `create_todo`  | Create a new task                        |
| `create_todos` | Create multiple tasks at once            |
| `update_todos` | Update status of one or more tasks       |
| `list_todos`   | List all current tasks with their status |

| Property | Type    | Default | Description                                                             |
| -------- | ------- | ------- | ----------------------------------------------------------------------- |
| `shared` | boolean | `false` | When `true`, todos are shared across all agents in a multi-agent config |

### Memory

Persistent key-value storage backed by SQLite. Data survives across sessions, letting agents remember context, user preferences, and past decisions. Memories can be organized with categories and searched by keyword.

Each agent gets its own database at `~/.cagent/memory/<agent-name>/memory.db` by default.

```yaml
toolsets:
  - type: memory
    path: ./agent_memory.db # optional: override the default location
```

| Property | Type   | Default                                      | Description                          |
| -------- | ------ | -------------------------------------------- | ------------------------------------ |
| `path`   | string | `~/.cagent/memory/<agent-name>/memory.db`    | Path to the SQLite database file     |

| Operation          | Description                                                         |
| ------------------ | ------------------------------------------------------------------- |
| `add_memory`       | Store a new memory with optional category                           |
| `get_memories`     | Retrieve all stored memories                                        |
| `delete_memory`    | Delete a specific memory by ID                                      |
| `search_memories`  | Search memories by keywords and/or category (more efficient than get_all) |
| `update_memory`    | Update an existing memory's content and/or category by ID           |

Memories support an optional `category` field (e.g., `preference`, `fact`, `project`, `decision`) for organization and filtering.

### Fetch

Make HTTP requests to external APIs and web services.

```yaml
toolsets:
  - type: fetch
    timeout: 30 # Optional: request timeout in seconds
```

Supports GET, POST, PUT, DELETE, and other HTTP methods. The agent can set headers, send request bodies, and receive response data. Useful for calling REST APIs, reading web pages, and downloading content.

| Property  | Type | Default | Description                |
| --------- | ---- | ------- | -------------------------- |
| `timeout` | int  | `30`    | Request timeout in seconds |

### Script

Define custom shell scripts as named tools. Unlike the generic `shell` tool, scripts are predefined and can be given descriptive names — ideal for exposing safe, well-scoped operations.

**Simple format:**

```yaml
toolsets:
  - type: script
    shell:
      run_tests:
        cmd: task test
        description: Run the project test suite
      lint:
        cmd: task lint
        description: Run the linter
      deploy:
        cmd: ./scripts/deploy.sh ${env}
        description: Deploy to an environment
        args:
          env:
            type: string
            enum: [staging, production]
        required: [env]
```

| Property                         | Type   | Description                                                |
| -------------------------------- | ------ | ---------------------------------------------------------- |
| `shell.&lt;name&gt;.cmd`         | string | Shell command to execute (supports `${arg}` interpolation) |
| `shell.&lt;name&gt;.description` | string | Description shown to the model                             |
| `shell.&lt;name&gt;.args`        | object | Parameter definitions (JSON Schema properties)             |
| `shell.&lt;name&gt;.required`    | array  | Required parameter names                                   |
| `shell.&lt;name&gt;.env`         | object | Environment variables for this script                      |
| `shell.&lt;name&gt;.working_dir` | string | Working directory for script execution                     |

### Transfer Task

The `transfer_task` tool is automatically available when an agent has `sub_agents`. Allows delegating tasks to sub-agents. No configuration needed — it's enabled implicitly.

### LSP (Language Server Protocol)

Connect to language servers for code intelligence: go-to-definition, find references, diagnostics, and more.

```yaml
toolsets:
  - type: lsp
    command: gopls
    args: []
    file_types: [".go"]
```

| Property     | Type   | Description                               |
| ------------ | ------ | ----------------------------------------- |
| `command`    | string | LSP server executable command             |
| `args`       | array  | Command-line arguments for the LSP server |
| `env`        | object | Environment variables for the LSP process |
| `file_types` | array  | File extensions this LSP handles          |

See [LSP Tool](/tools/lsp/) for full documentation.

### User Prompt

Ask users questions and collect interactive input during agent execution.

```yaml
toolsets:
  - type: user_prompt
```

The agent can use this tool to ask questions, present choices, or collect information from the user. Supports JSON Schema for structured input validation.

See [User Prompt Tool](/tools/user-prompt/) for full documentation.

### API

Create custom tools that call HTTP APIs without writing code.

```yaml
toolsets:
  - type: api
    name: get_weather
    method: GET
    endpoint: "https://api.weather.example/v1/current?city=${city}"
    instruction: Get current weather for a city
    args:
      city:
        type: string
        description: City name
    required: ["city"]
    headers:
      Authorization: "Bearer ${env.WEATHER_API_KEY}"
```

| Property   | Type   | Description                          |
| ---------- | ------ | ------------------------------------ |
| `name`     | string | Tool name                            |
| `method`   | string | HTTP method: `GET` or `POST`         |
| `endpoint` | string | URL with `${param}` interpolation    |
| `args`     | object | Parameter definitions                |
| `required` | array  | Required parameter names             |
| `headers`  | object | HTTP headers (supports `${env.VAR}`) |

See [API Tool](/tools/api/) for full documentation.

### Handoff

Delegate tasks to remote agents via the A2A (Agent-to-Agent) protocol.

```yaml
toolsets:
  - type: handoff
    name: research_agent
    description: Specialized research agent
    url: "http://localhost:8080/a2a"
    timeout: 5m
```

| Property      | Type   | Description                   |
| ------------- | ------ | ----------------------------- |
| `name`        | string | Tool name for delegation      |
| `description` | string | Description for the agent     |
| `url`         | string | A2A server endpoint URL       |
| `timeout`     | string | Request timeout (default: 5m) |

See [A2A Protocol](/features/a2a/) for full documentation.

### A2A (Agent-to-Agent)

Connect to remote agents via the A2A protocol. Similar to handoff but configured as a toolset.

```yaml
toolsets:
  - type: a2a
    name: research_agent
    url: "http://localhost:8080/a2a"
```

| Property | Type   | Description                    |
| -------- | ------ | ------------------------------ |
| `name`   | string | Tool name for the remote agent |
| `url`    | string | A2A server endpoint URL        |

See [A2A Protocol](/features/a2a/) for full documentation.

## MCP Tools

Extend agents with external tools via the [Model Context Protocol](https://modelcontextprotocol.io/).

### Docker MCP (Recommended)

Run MCP servers as secure Docker containers via the [MCP Gateway](https://github.com/docker/mcp-gateway):

```yaml
toolsets:
  - type: mcp
    ref: docker:duckduckgo # web search
  - type: mcp
    ref: docker:github-official # GitHub integration
  - type: mcp
    ref: docker:fetch # HTTP fetching
```

Browse available tools at the [Docker MCP Catalog](https://hub.docker.com/search?q=&type=mcp).

| Property      | Type   | Description                                                      |
| ------------- | ------ | ---------------------------------------------------------------- |
| `ref`         | string | Docker MCP reference (`docker:name`)                             |
| `tools`       | array  | Optional: only expose these tools                                |
| `instruction` | string | Custom instructions injected into the agent's context            |
| `config`      | any    | MCP server-specific configuration (passed during initialization) |

### Local MCP (stdio)

Run MCP servers as local processes communicating over stdin/stdout:

```yaml
toolsets:
  - type: mcp
    command: python
    args: ["-m", "mcp_server"]
    tools: ["search", "fetch"] # optional: whitelist specific tools
    env:
      API_KEY: value
```

| Property      | Type   | Description                                           |
| ------------- | ------ | ----------------------------------------------------- |
| `command`     | string | Command to execute the MCP server                     |
| `args`        | array  | Command arguments                                     |
| `tools`       | array  | Optional: only expose these tools                     |
| `env`         | array  | Environment variables (`"KEY=value"` format)          |
| `instruction` | string | Custom instructions injected into the agent's context |
| `version`     | string | Package reference for [auto-installing](#auto-installing-tools) the command binary |

### Remote MCP (SSE / Streamable HTTP)

Connect to MCP servers over the network:

```yaml
toolsets:
  - type: mcp
    remote:
      url: "https://mcp-server.example.com"
      transport_type: "sse" # sse or streamable
      headers:
        Authorization: "Bearer your-token"
    tools: ["search_web", "fetch_url"]
```

| Property                | Type   | Description                       |
| ----------------------- | ------ | --------------------------------- |
| `remote.url`            | string | Base URL of the MCP server        |
| `remote.transport_type` | string | `sse` or `streamable`             |
| `remote.headers`        | object | HTTP headers (typically for auth) |

## Auto-Installing Tools

When configuring MCP or LSP tools that require a binary command, cagent can **automatically download and install** the command if it's not already available on your system. This uses the [aqua registry](https://github.com/aquaproj/aqua-registry) — a curated index of CLI tool packages.

### How It Works

1. When a toolset with a `command` is loaded, cagent checks if the command is available in your `PATH`
2. If not found, it checks the cagent tools directory (`~/.cagent/tools/bin/`)
3. If still not found, it looks up the command in the aqua registry and installs it automatically

### Explicit Package Reference

Use the `version` property to specify exactly which package to install:

```yaml
toolsets:
  - type: mcp
    command: gopls
    version: "golang/tools@v0.25.0"
    args: ["mcp"]
  - type: lsp
    command: rust-analyzer
    version: "rust-lang/rust-analyzer@2024-01-01"
    file_types: [".rs"]
```

The format is `owner/repo` or `owner/repo@version`. When a version is omitted, the latest release is used.

### Automatic Detection

If the `version` property is not set, cagent tries to auto-detect the package from the command name by searching the aqua registry:

```yaml
toolsets:
  - type: mcp
    command: gopls  # auto-detected as golang/tools
    args: ["mcp"]
```

### Disabling Auto-Install

You can disable auto-installation in two ways:

**Per toolset** — set `version` to `"false"` or `"off"`:

```yaml
toolsets:
  - type: mcp
    command: my-custom-server
    version: "false"
```

**Globally** — set the `DOCKER_AGENT_AUTO_INSTALL` environment variable:

```bash
export DOCKER_AGENT_AUTO_INSTALL=false
```

### Environment Variables

| Variable              | Default                | Description                                      |
| --------------------- | ---------------------- | ------------------------------------------------ |
| `DOCKER_AGENT_AUTO_INSTALL` | (enabled)              | Set to `false` to disable all auto-installation  |
| `DOCKER_AGENT_TOOLS_DIR`    | `~/.cagent/tools/`     | Base directory for installed tools               |
| `GITHUB_TOKEN`        | —                      | GitHub token to raise API rate limits (optional) |

Installed binaries are placed in `~/.cagent/tools/bin/` and cached so they are only downloaded once.

<div class="callout callout-tip">
<div class="callout-title">💡 Tip
</div>
  <p>Auto-install supports both Go packages (via <code>go install</code>) and GitHub release binaries (via archive download). The aqua registry metadata determines which method is used.</p>

</div>

## Tool Filtering

Toolsets may expose many tools. Use the `tools` property to whitelist only the ones your agent needs. This works for any toolset type — not just MCP:

```yaml
toolsets:
  - type: mcp
    ref: docker:github-official
    tools: ["list_issues", "create_issue", "get_pull_request"]
  - type: filesystem
    tools: ["read_file", "search_files_content"]
  - type: shell
    tools: ["shell"]
```

<div class="callout callout-tip">
<div class="callout-title">💡 Tip
</div>
  <p>Filtering tools improves agent performance — fewer tools means less confusion for the model about which tool to use.</p>

</div>

## Tool Instructions

Add context-specific instructions that get injected when a toolset is loaded:

```yaml
toolsets:
  - type: mcp
    ref: docker:github-official
    instruction: |
      Use these tools to manage GitHub issues.
      Always check for existing issues before creating new ones.
      Label new issues with 'triage' by default.
```

## Combined Example

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-0
    description: Full-featured developer assistant
    instruction: You are an expert developer.
    toolsets:
      # Built-in tools
      - type: filesystem
      - type: shell
      - type: think
      - type: todo
      - type: memory
        path: ./dev.db
      - type: user_prompt
      # LSP for code intelligence
      - type: lsp
        command: gopls
        file_types: [".go"]
      # Custom scripts
      - type: script
        shell:
          run_tests:
            description: Run the test suite
            cmd: task test
          lint:
            description: Run the linter
            cmd: task lint
      # Custom API tool
      - type: api
        api_config:
          name: get_status
          method: GET
          endpoint: "https://api.example.com/status"
          instruction: Check service health
      # Docker MCP tools
      - type: mcp
        ref: docker:github-official
        tools: ["list_issues", "create_issue"]
      - type: mcp
        ref: docker:duckduckgo
      # Remote MCP
      - type: mcp
        remote:
          url: "https://internal-api.example.com/mcp"
          transport_type: "sse"
          headers:
            Authorization: "Bearer ${INTERNAL_TOKEN}"
```

<div class="callout callout-warning">
<div class="callout-title">⚠️ Toolset Order Matters
</div>
  <p>If multiple toolsets provide a tool with the same name, the first one wins. Order your toolsets intentionally.</p>

</div>
