---
title: "Tools"
description: "Tools give agents the ability to interact with the world — read files, run commands, search the web, query databases, and more."
permalink: /concepts/tools/
---

# Tools

_Tools give agents the ability to interact with the world — read files, run commands, search the web, query databases, and more._

## How Tools Work

When an agent needs to perform an action, it makes a **tool call**. The docker-agent runtime executes the tool and returns the result to the agent, which can then use it to continue its work.

1. Agent receives a user message
2. Agent decides it needs to use a tool (e.g., read a file)
3. docker-agent executes the tool and returns the result
4. Agent incorporates the result and responds

<div class="callout callout-info" markdown="1">
<div class="callout-title">Tool Confirmation
</div>
  <p>By default, docker-agent asks for user confirmation before executing tools that have side effects (shell commands, file writes). Use <code>--yolo</code> to auto-approve all tool calls.</p>
</div>

## Built-in Tools

docker-agent ships with several built-in tools that require no external dependencies. Each is enabled by adding its `type` to the agent's `toolsets` list:

| Tool | Description |
| --- | --- |
| [Filesystem]({{ '/tools/filesystem/' | relative_url }}) | Read, write, list, search, and navigate files and directories |
| [Shell]({{ '/tools/shell/' | relative_url }}) | Execute synchronous and background shell commands |
| [Think]({{ '/tools/think/' | relative_url }}) | Step-by-step reasoning scratchpad for planning and decision-making |
| [Todo]({{ '/tools/todo/' | relative_url }}) | Task list management for complex multi-step workflows |
| [Tasks]({{ '/tools/tasks/' | relative_url }}) | Persistent task database shared across sessions |
| [Memory]({{ '/tools/memory/' | relative_url }}) | Persistent key-value storage backed by SQLite |
| [Fetch]({{ '/tools/fetch/' | relative_url }}) | Read content from HTTP/HTTPS URLs (GET only) |
| [Script]({{ '/tools/script/' | relative_url }}) | Define custom shell scripts as named tools |
| [LSP]({{ '/tools/lsp/' | relative_url }}) | Connect to Language Server Protocol servers for code intelligence |
| [API]({{ '/tools/api/' | relative_url }}) | Create custom tools that call HTTP APIs without writing code |
| [OpenAPI]({{ '/tools/openapi/' | relative_url }}) | Generate tools from an OpenAPI 3.x document |
| [RAG]({{ '/features/rag/' | relative_url }}) | Retrieval-augmented generation over indexed sources |
| [Model Picker]({{ '/tools/model-picker/' | relative_url }}) | Let the agent pick between several models per turn |
| [User Prompt]({{ '/tools/user-prompt/' | relative_url }}) | Ask users questions and collect interactive input |
| [Transfer Task]({{ '/tools/transfer-task/' | relative_url }}) | Delegate tasks to sub-agents (auto-enabled with `sub_agents`) |
| [Background Agents]({{ '/tools/background-agents/' | relative_url }}) | Dispatch work to sub-agents concurrently |
| [Handoff]({{ '/tools/handoff/' | relative_url }}) | Hand the conversation off to another local agent in the same config (auto-enabled with `handoffs:`) |
| [A2A]({{ '/tools/a2a/' | relative_url }}) | Connect to remote agents via the Agent-to-Agent protocol |

## MCP Tools

docker-agent supports the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) for extending agents with external tools. There are three ways to connect MCP tools:

- **Docker MCP** (recommended) — Run MCP servers in Docker containers via the [MCP Gateway](https://github.com/docker/mcp-gateway). Browse the [Docker MCP Catalog](https://hub.docker.com/search?q=&type=mcp).
- **Local MCP (stdio)** — Run MCP servers as local processes communicating over stdin/stdout.
- **Remote MCP (SSE / HTTP)** — Connect to MCP servers running on a network. See [Remote MCP Servers]({{ '/features/remote-mcp/' | relative_url }}).

```yaml
toolsets:
  - type: mcp
    ref: docker:duckduckgo
```

See [Tool Config]({{ '/configuration/tools/#mcp-tools' | relative_url }}) for full MCP configuration reference.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">See also
</div>
  <p>For full configuration reference, see <a href="{{ '/configuration/tools/' | relative_url }}">Tool Config</a>. For RAG (document retrieval), see <a href="{{ '/features/rag/' | relative_url }}">RAG</a>.</p>
</div>
