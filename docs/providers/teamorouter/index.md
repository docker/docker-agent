---
title: "TeamoRouter"
description: "Use TeamoRouter models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, teamorouter
weight: 136
canonical: https://docs.docker.com/ai/docker-agent/providers/teamorouter/
---

_Use TeamoRouter models with Docker Agent._

## Overview

[TeamoRouter](https://teamorouter.com) is a hosted AI API gateway. One API key
reaches Claude, GPT, Gemini, DeepSeek, GLM, Kimi and Qwen models through an
OpenAI-compatible API, billed pay-as-you-go from a prepaid balance. Docker Agent
includes TeamoRouter as a built-in provider in its embedded provider catalog.

## Setup

1. Create an API key in the [TeamoRouter dashboard](https://teamorouter.com/dashboard).
2. Set the environment variable:

   ```bash
   export TEAMOROUTER_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

Prefix a TeamoRouter model ID with `teamorouter/`:

```yaml
agents:
  root:
    model: teamorouter/deepseek-v4-flash
    description: Assistant using TeamoRouter
    instruction: You are a helpful assistant.
```

### Named Model

For more control over parameters:

```yaml
models:
  teamorouter_gpt:
    provider: teamorouter
    model: gpt-6.1-sol
    max_tokens: 8192

agents:
  root:
    model: teamorouter_gpt
    description: Assistant using TeamoRouter
    instruction: You are a helpful assistant.
```

When TeamoRouter is selected automatically from available credentials, the
default model is `deepseek-v4-flash`. Model IDs are bare (no vendor prefix) and
are passed through as supplied.

## Available Models

The examples on this page use `deepseek-v4-flash`, `gpt-6.1-sol` and
`claude-sonnet-5-5`. TeamoRouter is listed in the models.dev catalog under the
`teamorouter` provider ID. The [model list](https://teamorouter.com/models)
shows every model and its price, and the models endpoint returns the IDs your
key can call:

```bash
curl https://api.teamorouter.com/v1/models \
  -H "Authorization: Bearer $TEAMOROUTER_API_KEY"
```

## How It Works

TeamoRouter is an entry in Docker Agent's built-in provider catalog
(`pkg/model/provider/catalog/providers.json`) that uses the existing OpenAI
implementation:

- **API type:** OpenAI-compatible Chat Completions (`openai_chatcompletions`)
- **Base URL:** `https://api.teamorouter.com/v1`
- **Token variable:** `TEAMOROUTER_API_KEY`

As with other multi-model hosts, Docker Agent combines consecutive system
messages for compatibility with models that require a single leading system
message.

## Anthropic Messages Endpoint

The same host also exposes a native Anthropic Messages endpoint at
`https://api.teamorouter.com` (no `/v1`). To send Claude requests in the
Messages format instead of Chat Completions, define a
[named provider](../custom/index.md) that reuses the same key:

```yaml
providers:
  teamorouter_messages:
    provider: anthropic
    base_url: https://api.teamorouter.com
    token_key: TEAMOROUTER_API_KEY

models:
  teamorouter_claude:
    provider: teamorouter_messages
    model: claude-sonnet-5-5

agents:
  root:
    model: teamorouter_claude
    description: Assistant using Claude through TeamoRouter
    instruction: You are a helpful assistant.
```

A custom Anthropic `base_url` does not inherit Claude API capability, beta or
price metadata; see
[Claude access paths](../anthropic/index.md#claude-access-paths).

## Example: Code Assistant

```yaml
agents:
  coder:
    model: teamorouter/deepseek-v4-flash
    description: Code assistant using TeamoRouter
    instruction: |
      You are an expert programmer.
      Write clean code and follow language best practices.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
```
