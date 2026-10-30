---
title: "cheapaimodel"
description: "Use cheapaimodel models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, cheapaimodel
weight: 25
canonical: https://docs.docker.com/ai/docker-agent/providers/cheapaimodel/
---

_Use cheapaimodel models with Docker Agent._

## Overview

[cheapaimodel](https://cheapaimodel.ai/?utm_source=docker-agent) is a
pay-per-token API gateway for open-weight models (DeepSeek, GLM, Kimi) and Grok,
billed from a prepaid USD balance. It serves them through an OpenAI-compatible
Chat Completions API. Docker Agent includes built-in support for cheapaimodel
as an alias provider.

## Setup

1. [Create a cheapaimodel API key](https://console.cheapaimodel.ai/keys)
2. Set the environment variable:

   ```bash
   export CHEAPAIMODEL_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

Use a cheapaimodel model by prefixing its model ID with `cheapaimodel/`:

```yaml
agents:
  root:
    model: cheapaimodel/deepseek-v4-1-flash
    description: Assistant using cheapaimodel
    instruction: You are a helpful assistant.
```

### Named Model

For more control over parameters:

```yaml
models:
  cheapaimodel_glm:
    provider: cheapaimodel
    model: glm-5.3
    max_tokens: 8192

agents:
  root:
    model: cheapaimodel_glm
    description: Assistant using cheapaimodel
    instruction: You are a helpful assistant.
```

## Available Models

Chat model IDs cheapaimodel serves:

| Model ID | Model |
| --- | --- |
| `deepseek-v4-1-flash` | DeepSeek V4.1 Flash (default) |
| `glm-5.3` | GLM-5.3 |
| `glm-5.3-flash` | GLM-5.3 Flash |
| `kimi-k3` | Kimi K3 |
| `grok-4.7` | Grok 4.7 |

The [model list](https://cheapaimodel.ai/models) shows current IDs, prices and
context windows; `GET https://api.cheapaimodel.ai/v1/models` returns the IDs your
key can call.

`kimi-k3` accepts only `temperature` 0.6 or 1 and `top_p` 0.95. Leave both unset
for it, or set one of those values.

cheapaimodel is not in the models.dev catalog. Docker Agent does not import
metadata from cheapaimodel's model list, so it has no catalog-derived pricing or
context limits for these models. Attachment capabilities conservatively fall
back to text-only; image and PDF attachments are dropped, with a warning logged
once per model.

`deepseek-v4-1-flash`, `glm-5.3-flash`, `kimi-k3` and `grok-4.7` accept image
input. To send images to one of them, set `models.<name>.capabilities`. See
[Attachment Capability Overrides](../../configuration/models/index.md#attachment-capability-overrides)
for all supported flags.

## How It Works

cheapaimodel is implemented as a built-in alias in Docker Agent:

- **API Type:** OpenAI-compatible Chat Completions (`openai_chatcompletions`)
- **Base URL:** `https://api.cheapaimodel.ai/v1`
- **Token Variable:** `CHEAPAIMODEL_API_KEY`

Because cheapaimodel serves open-weight models whose chat templates can reject
multiple leading system messages, Docker Agent coalesces its per-source system
messages into one.

## Example: Code Assistant

```yaml
agents:
  coder:
    model: cheapaimodel/deepseek-v4-1-flash
    description: Code assistant using cheapaimodel
    instruction: |
      You are an expert programmer.
      Write clean code and follow language best practices.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
```
