---
title: "Standard Compute"
description: "Use Standard Compute models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, standard compute, standardcompute
weight: 255
canonical: https://docs.docker.com/ai/docker-agent/providers/standardcompute/
---

_Use Standard Compute models with Docker Agent._

## Overview

[Standard Compute](https://standardcompute.com) is a multi-model LLM router
that serves models from multiple vendors behind one OpenAI-compatible endpoint
(`https://api.stdcmpt.com/v1`). Docker Agent includes built-in support for
Standard Compute as an alias provider.

## Setup

1. Create an API key from your Standard Compute dashboard.
2. Set the environment variable:

   ```bash
   export STANDARDCOMPUTE_API_KEY=your-api-key
   ```

## Usage

### Inline Syntax

```yaml
agents:
  root:
    model: standardcompute/standardcompute
    description: Assistant using Standard Compute
    instruction: You are a helpful assistant.
```

### Named Model

```yaml
models:
  standardcompute_model:
    provider: standardcompute
    model: deepseek/deepseek-v4-pro
    temperature: 0.7
    max_tokens: 8192

agents:
  root:
    model: standardcompute_model
    description: Assistant using Standard Compute
    instruction: You are a helpful assistant.
```

## Available Models

The `standardcompute` model ID lets the router pick a model per request. To
pin a specific model, use its vendor-prefixed ID (for example
`deepseek/deepseek-v4-pro`). List the available model IDs with:

```bash
curl https://api.stdcmpt.com/v1/models
```

Use any ID from that response as the `model` value. Docker Agent's default for
Standard Compute is `standardcompute`.

## Usage Limits

Standard Compute plans include a monthly usage budget. Once it is used up,
requests fail with HTTP `402` until the budget renews.
