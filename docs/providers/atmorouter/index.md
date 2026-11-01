---
title: "AtmoRouter"
description: "Use AtmoRouter models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, atmorouter
weight: 27
canonical: https://docs.docker.com/ai/docker-agent/providers/atmorouter/
---

_Use AtmoRouter models with Docker Agent._

## Overview

[AtmoRouter](https://atmorouter.dev) serves Claude, GPT, Gemini, Qwen and GLM
models through an OpenAI-compatible API. Docker Agent includes built-in support
for AtmoRouter as an alias provider. Current model IDs are on the live
[model list](https://atmorouter.dev/v1/models).

## Setup

1. Create an API key in the [AtmoRouter dashboard](https://atmorouter.dev/)
2. Set the environment variable:

   ```bash
   export ATMOROUTER_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

Use an AtmoRouter model by prefixing its model ID with `atmorouter/`:

```yaml
agents:
  root:
    model: atmorouter/cc/claude-sonnet-4-6
    description: Assistant using AtmoRouter
    instruction: You are a helpful assistant.
```

### Named Model

For more control over parameters:

```yaml
models:
  atmorouter_qwen:
    provider: atmorouter
    model: ali/qwen3.8-max
    temperature: 0.7
    max_tokens: 8192

agents:
  root:
    model: atmorouter_qwen
    description: Assistant using AtmoRouter
    instruction: You are a helpful assistant.
```

When AtmoRouter is selected automatically from available credentials, the
default model is `cc/claude-sonnet-4-6`.

## Available Models

Some chat model IDs AtmoRouter serves:

| Model ID | Model |
| --- | --- |
| `cc/claude-sonnet-4-6` | Claude Sonnet 4.6 (default) |
| `cx/gpt-6.1-sol` | GPT-6.1 Sol |
| `ali/qwen3.8-max` | Qwen3.8 Max |
| `zai/glm-5.3` | GLM 5.3 |
| `ag/gemini-3.8-flash-high` | Gemini 3.8 Flash High |

Model IDs are case-sensitive and include a prefix before the model name. The
live [`/v1/models` endpoint](https://atmorouter.dev/v1/models) lists current IDs.
A listing does not guarantee that inference is currently available.

AtmoRouter is absent from the embedded models.dev catalog. Docker Agent does not
import context limits or attachment capabilities from AtmoRouter's model list.
Attachment capabilities fall back to text-only, so image and PDF attachments
are dropped, with a warning logged once per model.

If you have verified that your chosen model accepts a particular attachment
type, set `models.<name>.capabilities` to override detection. See
[Attachment Capability Overrides](../../configuration/models/index.md#attachment-capability-overrides)
for all supported flags.

## How It Works

AtmoRouter is a built-in alias in Docker Agent:

- **API type:** OpenAI-compatible Chat Completions (`openai_chatcompletions`)
- **Base URL:** `https://atmorouter.dev/v1`
- **Token variable:** `ATMOROUTER_API_KEY`

Some models AtmoRouter serves require a single leading system message, so Docker
Agent coalesces consecutive system messages into one.

## Example: Code Assistant

```yaml
agents:
  coder:
    model: atmorouter/cc/claude-sonnet-4-6
    description: Code assistant using AtmoRouter
    instruction: |
      You are an expert programmer.
      Write clean code and follow language best practices.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
```
