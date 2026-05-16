---
title: "ACP (Agent Client Protocol)"
description: "Expose docker-agent agents via the Agent Client Protocol for integration with ACP-compatible hosts like VS Code, IDEs, and other developer tools."
permalink: /features/acp/
---

# ACP (Agent Client Protocol)

_Expose docker-agent agents via the Agent Client Protocol for integration with ACP-compatible hosts like VS Code, IDEs, and other developer tools._

## Overview

The `docker agent serve acp` command starts an ACP server that communicates over **stdio** (standard input/output). This makes it ideal for integration with editors, IDEs, and other tools that spawn agent processes — the host sends JSON-RPC messages to docker-agent's stdin and reads responses from stdout.

ACP is built on the [ACP Go SDK](https://github.com/coder/acp-go-sdk) and provides a standardized way for client applications to interact with AI agents.

<div class="callout callout-info" markdown="1">
<div class="callout-title">ACP vs A2A vs MCP
</div>
  **ACP** connects an agent to a *host application* (IDE, CLI tool) via stdio. **A2A** connects *agents to other agents* over HTTP. **MCP** exposes agents as *tools* for other MCP clients. Choose based on your integration target.

</div>

## Usage

```bash
# Start ACP server on stdio
$ docker agent serve acp ./agent.yaml

# With a multi-agent team config
$ docker agent serve acp ./team.yaml

# From the agent catalog
$ docker agent serve acp agentcatalog/pirate

# With a custom session database
$ docker agent serve acp ./agent.yaml --session-db ./my-sessions.db
```

## How It Works

1. The host application spawns `docker agent serve acp agent.yaml` as a child process
2. Communication happens over **stdin/stdout** using the ACP protocol
3. The host sends user messages, docker-agent processes them through the agent
4. Agent responses, tool calls, and events stream back to the host
5. Sessions are persisted in a SQLite database for continuity

```bash
# Conceptual flow:
Host Application
  └── spawns: docker agent serve acp agent.yaml
        ├── stdin  ← JSON-RPC requests from host
        └── stdout → JSON-RPC responses to host
```

## Features

- **Stdio transport** — No network ports needed; ideal for subprocess integration
- **Session persistence** — SQLite-backed sessions survive process restarts
- **Full agent support** — All docker-agent features work: tools, multi-agent, model fallbacks
- **Multi-agent configs** — Team configurations with sub-agents work transparently
- **Filesystem operations** — Agents can read/write files relative to the host's working directory

## CLI Flags

```bash
docker agent serve acp <agent-file>|<registry-ref> [flags]
```

| Flag                              | Default                | Description                                                                                                          |
| --------------------------------- | ---------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `-s, --session-db <path>`         | `~/.cagent/session.db` | Path to the SQLite session database.                                                                                 |
| `--working-dir <path>`            | current dir            | Working directory the agent runs in.                                                                                 |
| `--env-from-file <file>`          | (none)                 | Load additional environment variables from a `.env` file (repeatable).                                               |
| `--models-gateway <url>`          | (none)                 | Route all provider traffic through a models gateway URL.                                                             |
| `--code-mode-tools`               | `false`                | Expose tools as a single "code" toolset that accepts a JavaScript snippet to run.                                    |
| `--hook-pre-tool-use <cmd>`       | (none)                 | Add a pre-tool-use hook (repeatable). See [Hooks]({{ '/configuration/hooks/' | relative_url }}).                     |
| `--hook-post-tool-use <cmd>`      | (none)                 | Add a post-tool-use hook (repeatable).                                                                               |
| `--hook-session-start <cmd>`      | (none)                 | Add a session-start hook (repeatable).                                                                               |
| `--hook-session-end <cmd>`        | (none)                 | Add a session-end hook (repeatable).                                                                                 |
| `--hook-on-user-input <cmd>`      | (none)                 | Add an on-user-input hook (repeatable).                                                                              |
| `--hook-stop <cmd>`               | (none)                 | Add a stop hook, fired when the model finishes responding (repeatable).                                              |

## Integration Example

A host application would spawn docker-agent as a subprocess and communicate via the ACP protocol:

```javascript
// Pseudocode for an IDE extension
const child = spawn("docker", ["agent", "serve", "acp", "./agent.yaml"]);

// Send a message to the agent
child.stdin.write(
  JSON.stringify({
    jsonrpc: "2.0",
    method: "agent/run",
    params: { message: "Explain this code" },
  }),
);

// Read responses
child.stdout.on("data", (data) => {
  const response = JSON.parse(data);
  // Handle agent response, tool calls, etc.
});
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">When to use ACP
</div>
  <p>Use ACP when building **IDE integrations**, **editor plugins**, or any tool that wants to embed a docker-agent agent as a subprocess. For HTTP-based integrations, use the <a href="{{ '/features/api-server/' | relative_url }}">API Server</a> instead.</p>

</div>

<div class="callout callout-info" markdown="1">
<div class="callout-title">See also
</div>
  <p>For HTTP-based agent access, see the <a href="{{ '/features/api-server/' | relative_url }}">API Server</a>. For agent-to-agent communication, see <a href="{{ '/features/a2a/' | relative_url }}">A2A Protocol</a>. For exposing agents as MCP tools, see <a href="{{ '/features/mcp-mode/' | relative_url }}">MCP Mode</a>.</p>

</div>
