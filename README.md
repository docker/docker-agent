# 🤖 Docker Agent 🤖

> Build, run, and share AI agents with a declarative YAML config, rich tool ecosystem, and multi-agent orchestration.

![docker agent in action](docs/demo.gif)

## What is Docker Agent?

`docker-agent` lets you create and run intelligent AI agents that collaborate to solve complex problems — no code required.

`docker-agent` is a `docker` CLI plugin and can be run with `docker agent`.

Define agents in YAML, give them tools, and let them work.

```yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: A helpful AI assistant
    instruction: |
      You are a knowledgeable assistant that helps users with various tasks.
      Be helpful, accurate, and concise in your responses.
    toolsets:
      - type: mcp
        ref: docker:duckduckgo
```

```sh
docker agent run agent.yaml
```

## Key Features

- **Multi-agent architecture** — Create teams of specialized agents that delegate tasks automatically
- **Rich tool ecosystem** — Built-in tools + any [MCP](https://modelcontextprotocol.io/) server (local, remote, or Docker-based)
- **AI provider agnostic** — OpenAI, Anthropic, Gemini, AWS Bedrock, Mistral, xAI, [Docker Model Runner](https://docs.docker.com/ai/model-runner/), and more
- **YAML configuration** — Declarative, versionable, shareable
- **Advanced reasoning** — Built-in think, todo, and memory tools
- **RAG** — Pluggable retrieval with BM25, embeddings, hybrid search, and reranking
- **Package & share** — Push agents to any OCI registry, pull and run them anywhere

## Install

**Docker Desktop** (4.63+) — docker-agent CLI plugin is pre-installed. Just run `docker agent`.

**Homebrew** — `brew install docker-agent`. Run `docker-agent` directly or symlink the binary to `~/.docker/cli-plugins/docker-agent` and run `docker agent`.

**Binary releases** — Download from [GitHub Releases](https://github.com/docker/docker-agent/releases). Symlink the `docker-agent` binary to `~/.docker/cli-plugins/docker-agent` to be able to use `docker agent`, or use `docker-agent` directly.

Set at least one API key (or use [Docker Model Runner](https://docs.docker.com/ai/model-runner/) for local models):

```sh
export OPENAI_API_KEY=sk-...        # or ANTHROPIC_API_KEY, GOOGLE_API_KEY, etc.
```

## Quick Start

```sh
# Run the default agent
docker agent run

# Run from the agent catalog
docker agent run agentcatalog/pirate

# Generate a new agent interactively
docker agent new

# Run your own config
docker agent run agent.yaml
```

More examples in the [`examples/`](examples/README.md) directory.

## Documentation

📖 **[Full documentation](https://docker.github.io/docker-agent/)**

- [Installation](https://docker.github.io/docker-agent/getting-started/installation) · [Quick Start](https://docker.github.io/docker-agent/getting-started/quickstart)
- [Agents](https://docker.github.io/docker-agent/concepts/agents) · [Models](https://docker.github.io/docker-agent/concepts/models) · [Tools](https://docker.github.io/docker-agent/concepts/tools) · [Multi-Agent](https://docker.github.io/docker-agent/concepts/multi-agent)
- [Configuration Reference](https://docker.github.io/docker-agent/configuration/overview)
- [TUI](https://docker.github.io/docker-agent/features/tui) · [CLI](https://docker.github.io/docker-agent/features/cli) · [MCP Mode](https://docker.github.io/docker-agent/features/mcp-mode) · [RAG](https://docker.github.io/docker-agent/features/rag)
- [Model Providers](https://docker.github.io/docker-agent/providers/overview) · [Docker Model Runner](https://docker.github.io/docker-agent/providers/dmr)

## Contributing

Read the [Contributing guide](https://docker.github.io/docker-agent/community/contributing) to get started. We use `docker-agent` to build `docker-agent`:

```sh
docker agent run ./golang_developer.yaml
```

## Telemetry

We collect anonymous usage data to improve the tool. See [Telemetry](https://docker.github.io/docker-agent/community/telemetry).

## Community

[Docker Community Slack](http://dockr.ly/comm-slack) · [#docker-agent channel](https://dockercommunity.slack.com/archives/C09DASHHRU4)
## ❓ FAQ

### General

**Q: What is Docker Agent?**
Docker Agent is a `docker` CLI plugin that lets you create and run intelligent AI agents using declarative YAML configuration. No coding required — define agents, equip them with tools, and let them collaborate to solve complex problems.

**Q: How is Docker Agent different from other AI agent frameworks?**
Docker Agent is designed as a **CLI-first, YAML-configured** agent runtime. It integrates natively with the Docker ecosystem (OCI registry, Docker Model Runner, MCP tools from Docker Hub) and focuses on simplicity and shareability.

**Q: What Docker version is required?**
Docker Desktop 4.63+ (docker-agent is pre-installed). For Docker Engine, install via Homebrew or binary releases.

### Installation & Configuration

**Q: How do I install docker-agent?**
- **Docker Desktop (4.63+)**: Pre-installed, just run `docker agent`
- **Homebrew**: `brew install docker-agent`
- **Binary releases**: Download from [GitHub Releases](https://github.com/docker/docker-agent/releases) and symlink to `~/.docker/cli-plugins/docker-agent`

**Q: Which AI providers are supported?**
OpenAI, Anthropic, Gemini, AWS Bedrock, Mistral, xAI, Docker Model Runner, and any provider compatible with the OpenAI API format. Set the corresponding API key as an environment variable (e.g., `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`).

**Q: Can I use local models?**
Yes, via [Docker Model Runner](https://docs.docker.com/ai/model-runner/) which lets you run models locally in containers.

### Agent Configuration

**Q: How do I define an agent?**
Create a YAML file with agent definitions:
```yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: A helpful AI assistant
    instruction: |
      You are a knowledgeable assistant.
    toolsets:
      - type: mcp
        ref: docker:duckduckgo
```
Then run: `docker agent run agent.yaml`

**Q: What is multi-agent architecture?**
Create teams of specialized agents that delegate tasks automatically. Define multiple agents in YAML and use the `delegate` tool to route tasks between them.

**Q: How does RAG work?**
Docker Agent supports pluggable retrieval with BM25, embeddings, hybrid search, and reranking. Configure a RAG toolset in your agent YAML to enable document retrieval.

### Tools & MCP

**Q: What tools are available?**
Built-in tools include think, todo, and memory. Additionally, any [MCP](https://modelcontextprotocol.io/) server can be used — local, remote, or Docker-based (e.g., `docker:duckduckgo`).

**Q: How do I use MCP servers?**
Add an MCP toolset to your agent configuration:
```yaml
toolsets:
  - type: mcp
    ref: docker:duckduckgo    # Docker Hub MCP server
    # or
    ref: ./my-mcp-server      # Local MCP server
```

### Deployment & Sharing

**Q: How do I share agents?**
Push agents to any OCI registry:
```sh
docker agent push my-agent:latest
```
Others can pull and run your agent:
```sh
docker agent run my-agent:latest
```

**Q: What is the agent catalog?**
A collection of pre-built agents available via `docker agent run agentcatalog/<name>`. Browse and reuse agents built by the community.

### Troubleshooting

**Q: "authentication failed" when running an agent?**
Ensure you have set at least one API key environment variable (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GOOGLE_API_KEY`, etc.) or have Docker Model Runner configured for local models.

**Q: MCP tool not connecting?**
1. Verify the MCP server is running and accessible
2. Check the `ref` path in your YAML configuration
3. For Docker Hub MCP servers, ensure Docker Desktop is running

**Q: Agent not responding or timing out?**
1. Check your model configuration — some models have rate limits
2. Increase timeout settings in the agent YAML
3. Review logs with `docker agent run agent.yaml --verbose`

**Q: TUI not displaying correctly?**
The TUI requires a terminal with true color support. If you experience display issues, try running in CLI mode or update your terminal emulator.

