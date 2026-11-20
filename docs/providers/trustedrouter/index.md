---
title: "TrustedRouter"
description: "Use TrustedRouter models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, trustedrouter
weight: 255
canonical: https://docs.docker.com/ai/docker-agent/providers/trustedrouter/
---

_Use TrustedRouter models with Docker Agent._

## Overview

[TrustedRouter](https://trustedrouter.com) is an OpenAI-compatible router that
serves models from multiple upstream providers through attested
(confidential-computing) gateways. Docker Agent includes built-in support for
TrustedRouter as an alias provider. Current model IDs are on the public
[model list](https://api.trustedrouter.com/v1/models).

## Setup

1. Create an API key at [trustedrouter.com](https://trustedrouter.com). Keys are
   prefixed with `sk-tr-v1-`.
2. Set the environment variable:

   ```bash
   export TRUSTEDROUTER_API_KEY=sk-tr-v1-your-api-key
   ```

## Usage

### Inline Syntax

The first `trustedrouter/` selects the provider; the rest is the TrustedRouter
model ID, which is always vendor-prefixed:

```yaml
agents:
  root:
    model: trustedrouter/trustedrouter/auto
    description: Assistant using TrustedRouter
    instruction: You are a helpful assistant.
```

Upstream models keep their prefixed IDs, for example
`trustedrouter/anthropic/claude-sonnet-4.5`.

### Named Model

```yaml
models:
  trustedrouter_model:
    provider: trustedrouter
    model: trustedrouter/auto

agents:
  root:
    model: trustedrouter_model
    instruction: You are a helpful assistant.
```

When TrustedRouter is selected automatically from available credentials, the
default model is `trustedrouter/auto`. Other routes include
`trustedrouter/cheap` and `trustedrouter/fast`.

## How it works

Requests use OpenAI-compatible Chat Completions at
`https://api.trustedrouter.com/v1` with `TRUSTEDROUTER_API_KEY`. Context limits
and capabilities come from the embedded models.dev catalog. The catalog has no
pricing for TrustedRouter routes, so set `cost` on a model to track spend (see
[Custom Token Pricing](../../configuration/models/index.md#custom-token-pricing)).
When a Docker Agent models gateway is configured, the built-in alias
routes through that gateway instead; set `bypass_models_gateway: true` on a named
model to call TrustedRouter directly. See
[Gateway Bypass](../../configuration/models/index.md#gateway-bypass).
