---
title: "Atlas Cloud"
description: "Use Atlas Cloud models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, atlas cloud
weight: 25
canonical: https://docs.docker.com/ai/docker-agent/providers/atlascloud/
---

_Use Atlas Cloud models with Docker Agent._

## Overview

Atlas Cloud provides access to multiple model families through an OpenAI-compatible API. Docker Agent includes built-in support for Atlas Cloud as an alias provider.

## Setup

1. [Create an Atlas Cloud API key](https://www.atlascloud.ai/docs/api-keys)
2. Set the environment variable:

   ```bash
   export ATLASCLOUD_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

Use an Atlas Cloud model by prefixing its model ID with `atlascloud/`:

```yaml
agents:
  root:
    model: atlascloud/qwen/qwen3.8-max
    description: Assistant using Atlas Cloud
    instruction: You are a helpful assistant.
```

Docker Agent splits only the first slash, so model IDs that include a model family prefix are preserved.

### Named Model

For more control over parameters:

```yaml
models:
  atlas_qwen:
    provider: atlascloud
    model: qwen/qwen3.8-max
    temperature: 0.7
    max_tokens: 8192

agents:
  root:
    model: atlas_qwen
    description: Assistant using Atlas Cloud
    instruction: You are a helpful assistant.
```

## How It Works

Atlas Cloud is implemented as a built-in alias in Docker Agent:

- **API Type:** OpenAI-compatible (`openai`)
- **Base URL:** `https://api.atlascloud.ai/v1`
- **Token Variable:** `ATLASCLOUD_API_KEY`
