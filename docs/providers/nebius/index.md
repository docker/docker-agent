---
title: "Nebius Token Factory"
description: "Use Nebius Token Factory models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, nebius, token factory
weight: 190
canonical: https://docs.docker.com/ai/docker-agent/providers/nebius/
---

_Use Nebius Token Factory models with Docker Agent._

## Overview

[Nebius Token Factory](https://tokenfactory.nebius.com/) provides hosted AI
models through an OpenAI-compatible API. Docker Agent includes built-in support
for Token Factory as the `nebius` alias provider.

## Setup

1. Create a project API key in [Nebius Token Factory](https://tokenfactory.nebius.com/).
2. Set the environment variable:

   ```bash
   export NEBIUS_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

The simplest way to use Nebius:

```yaml
agents:
  root:
    model: nebius/openai/gpt-oss-120b
    description: Assistant using Nebius Token Factory
    instruction: You are a helpful assistant.
```

### Named Model

For more control over parameters:

```yaml
models:
  nebius_model:
    provider: nebius
    model: openai/gpt-oss-120b
    temperature: 0.7
    max_tokens: 8192

agents:
  root:
    model: nebius_model
    description: Assistant using Nebius Token Factory
    instruction: You are a helpful assistant.
```

## Available Models

Token Factory's catalog changes as models launch and retire. With your API key
configured, list the models currently available to your project:

```console
$ docker agent models --provider nebius
```

You can also browse models in the [Token Factory
console](https://tokenfactory.nebius.com/models). Model IDs are case-sensitive;
copy the full ID shown by the CLI or console.

## How It Works

Nebius is implemented as a built-in alias in Docker Agent:

- **API Type:** OpenAI-compatible (`openai_chatcompletions`)
- **Base URL:** `https://api.tokenfactory.nebius.com/v1`
- **Token Variable:** `NEBIUS_API_KEY`

## Example: Code Assistant

```yaml
agents:
  coder:
    model: nebius/openai/gpt-oss-120b
    description: Code assistant using Nebius Token Factory
    instruction: |
      You are an expert programmer.
      Write clean, well-documented code.
      Follow best practices for the language being used.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
```
