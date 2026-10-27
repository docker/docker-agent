---
title: "CloudGPU"
description: "Use CloudGPU models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, cloudgpu
weight: 25
canonical: https://docs.docker.com/ai/docker-agent/providers/cloudgpu/
---

_Use CloudGPU models with Docker Agent._

## Overview

[CloudGPU](https://cloudgpu.app/api?utm_source=github&utm_medium=directory&utm_campaign=202611_seg04_codingagent&utm_content=github_docker_agent_provider)
rents cloud GPUs and serves DeepSeek, Kimi, GLM, MiniMax and other models
through an OpenAI-compatible API. Docker Agent includes built-in support for
CloudGPU as an alias provider.

## Setup

1. Create an API key in the [CloudGPU console](https://cloudgpu.app/)
2. Set the environment variable:

   ```bash
   export CLOUDGPU_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

Use a CloudGPU model by prefixing its model ID with `cloudgpu/`:

```yaml
agents:
  root:
    model: cloudgpu/deepseek-v4.1-flash
    description: Assistant using CloudGPU
    instruction: You are a helpful assistant.
```

### Named Model

For more control over parameters:

```yaml
models:
  cloudgpu_kimi:
    provider: cloudgpu
    model: kimi-k2.6
    temperature: 0.7
    max_tokens: 8192

agents:
  root:
    model: cloudgpu_kimi
    description: Assistant using CloudGPU
    instruction: You are a helpful assistant.
```

## Available Models

Some chat model IDs CloudGPU serves:

| Model ID | Model |
| --- | --- |
| `deepseek-v4.1-flash` | DeepSeek V4.1 Flash (default) |
| `kimi-k2.6` | Kimi K2.6 |
| `glm-5.3-flash` | GLM-5.3 Flash |
| `minimax-m3` | MiniMax M3 |

Model IDs are case-sensitive. The live [`/v1/models` endpoint](https://cloudgpu.app/v1/models)
lists current IDs, and [the API docs](https://cloudgpu.app/api/docs) describe
the endpoints.

CloudGPU is not in the models.dev catalog. Docker Agent does not import metadata
from CloudGPU's model list, so it has no catalog-derived pricing or context limits
for these models. Attachment capabilities conservatively fall back to text-only;
image and PDF attachments are dropped, with a warning logged once per model.

If you have verified that your chosen model accepts a particular attachment
type, set `models.<name>.capabilities` to override detection. See
[Attachment Capability Overrides](../../configuration/models/index.md#attachment-capability-overrides)
for all supported flags.

## How It Works

CloudGPU is implemented as a built-in alias in Docker Agent:

- **API Type:** OpenAI-compatible Chat Completions (`openai_chatcompletions`)
- **Base URL:** `https://cloudgpu.app/v1`
- **Token Variable:** `CLOUDGPU_API_KEY`

Some model chat templates reject multiple leading system messages, so Docker
Agent coalesces its per-source system messages into one for CloudGPU.

## Example: Code Assistant

```yaml
agents:
  coder:
    model: cloudgpu/deepseek-v4.1-flash
    description: Code assistant using CloudGPU
    instruction: |
      You are an expert programmer.
      Write clean code and follow language best practices.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
```
