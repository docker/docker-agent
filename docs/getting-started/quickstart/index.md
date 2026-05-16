---
title: "Quick Start"
description: "Get up and running with Docker Agent in under 5 minutes. Pick whichever path suits you best."
permalink: /getting-started/quickstart/
---

# Quick Start

_Get up and running with Docker Agent in under 5 minutes. Pick whichever path suits you best._

## Option A: Run the Default Agent

The fastest way to try Docker Agent — no config file needed:

```bash
# Launch the default agent with the interactive TUI
$ docker agent run
```

This starts a general-purpose assistant with sensible defaults. Just start chatting.

## Option B: Run a Pre-Built Agent from the Registry

Try a ready-made agent from the [agent catalog](https://hub.docker.com/u/agentcatalog) — no YAML needed:

```bash
# Run a pirate-themed assistant
$ docker agent run agentcatalog/pirate

# Run a coding agent
$ docker agent run agentcatalog/coder
```

## Option C: Generate a Config Interactively

Use the `docker agent new` command to scaffold a config file through prompts:

```bash
# Interactive wizard
$ docker agent new

# Or specify options directly
$ docker agent new --model openai/gpt-5-mini

# Override iteration limits
$ docker agent new --model dmr/ai/gemma3-qat:12B --max-iterations 15
```

This generates an `agent.yaml` in the current directory. Then run it:

```bash
$ docker agent run agent.yaml
```

## Option D: Write Your Own Config

Create an `agent.yaml` by hand for full control. Here's a minimal example:

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: A helpful coding assistant
    instruction: |
      You are an expert software developer. Help users write
      clean, efficient code. Explain your reasoning.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
```

This gives your agent:

- **Claude Sonnet 4.5** as the underlying model
- **Filesystem access** to read and write files
- **Shell access** to run commands
- **Think tool** for step-by-step reasoning

```bash
# Launch the interactive terminal UI
$ docker agent run agent.yaml
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Prefer HCL?
</div>
  <p>You can write the same config as <code>agent.hcl</code> using labeled blocks and heredocs. See <a href="{{ '/configuration/hcl/' | relative_url }}">HCL Configuration</a>.</p>

</div>

## Try It Out

Once your agent is running, try asking it to:

- _"List the files in the current directory"_
- _"Create a Python script that fetches weather data"_
- _"Explain what the code in main.go does"_

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Tip
</div>
  <p>Add <code>--yolo</code> to auto-approve all tool calls: `docker agent run agent.yaml --yolo`</p>

</div>

## Non-Interactive Mode

Use `docker agent run --exec` for one-shot tasks:

```bash
# Ask a single question
$ docker agent run --exec agent.yaml "Create a Dockerfile for a Node.js app"

# Pipe input
$ cat error.log | docker agent run --exec agent.yaml "What's wrong in this log?"
```

## Add More Power

Give your agent persistent memory and web search:

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Research assistant with memory
    instruction: |
      You are a research assistant. Search the web for information,
      remember important findings, and provide thorough analysis.
    toolsets:
      - type: think
      - type: memory
        path: ./research.db
      - type: mcp
        ref: docker:duckduckgo
```

<div class="callout callout-info" markdown="1">
<div class="callout-title">Docker MCP Tools
</div>
  <p>The <code>ref: docker:duckduckgo</code> syntax runs the DuckDuckGo MCP server in a Docker container. This is the recommended way to use MCP tools — secure, isolated, and easy to configure. Requires Docker Desktop.</p>

</div>

## What's Next?

<div class="cards">
  <a class="card" href="{{ '/concepts/agents/' | relative_url }}">
    <div class="card-icon">🤖</div>
    <h3>Understand Agents</h3>
    <p>Learn how agents work and what you can configure.</p>
  </a>
  <a class="card" href="{{ '/concepts/multi-agent/' | relative_url }}">
    <div class="card-icon">👥</div>
    <h3>Multi-Agent Systems</h3>
    <p>Build teams of collaborating agents.</p>
  </a>
  <a class="card" href="{{ '/configuration/overview/' | relative_url }}">
    <div class="card-icon">📚</div>
    <h3>Configuration Reference</h3>
    <p>Full reference for all YAML and HCL options.</p>
  </a>
  <a class="card" href="{{ '/community/troubleshooting/' | relative_url }}">
    <div class="card-icon">🔧</div>
    <h3>Troubleshooting</h3>
    <p>Something not working? Debug tips and common fixes.</p>
  </a>
</div>
