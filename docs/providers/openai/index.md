---
title: "OpenAI"
description: "Use GPT-4o, GPT-5, GPT-5-mini, and other OpenAI models with docker-agent."
permalink: /providers/openai/
---

# OpenAI

_Use GPT-4o, GPT-5, GPT-5-mini, and other OpenAI models with docker-agent._

## Setup

```bash
# Set your API key
export OPENAI_API_KEY="sk-..."
```

## Configuration

### Inline

```yaml
agents:
  root:
    model: openai/gpt-5-mini
```

### Named Model

```yaml
models:
  gpt:
    provider: openai
    model: gpt-5-mini
    temperature: 0.7
    max_tokens: 4000
```

## Available Models

| Model         | Best For                             |
| ------------- | ------------------------------------ |
| `gpt-5`       | Most capable, complex reasoning      |
| `gpt-5-mini`  | Fast, cost-effective, good reasoning |
| `gpt-4o`      | Multimodal, balanced performance     |
| `gpt-4o-mini` | Cheapest, fast for simple tasks      |

Find more model names at [modelnames.ai](https://modelnames.ai/) or in the [official OpenAI docs](https://platform.openai.com/docs/models).

## Thinking Budget

OpenAI uses effort level strings:

```yaml
models:
  gpt-thinking:
    provider: openai
    model: gpt-5-mini
    thinking_budget: low # minimal | low | medium (default) | high | xhigh | max | none | adaptive/<level>
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Custom endpoints
</div>
  <p>Use <code>base_url</code> for proxies and OpenAI-compatible services. See <a href="{{ '/providers/custom/' | relative_url }}">Custom Providers</a> for full setup.</p>

</div>

## Custom Endpoint

Use `base_url` to connect to OpenAI-compatible APIs:

```yaml
models:
  custom:
    provider: openai
    model: gpt-5-mini
    base_url: https://your-proxy.example.com/v1
```

## WebSocket Transport

For OpenAI Responses API models (gpt-4.1+, o-series, gpt-5), you can use WebSocket streaming instead of the default SSE (Server-Sent Events):

```yaml
models:
  fast-gpt:
    provider: openai
    model: gpt-4.1
    provider_opts:
      transport: websocket  # Use WebSocket instead of SSE
```

### Benefits

- **~40% faster** for workflows with 20+ tool calls
- **Persistent connection** reduces per-turn overhead
- **Server-side caching** of connection state
- **Automatic fallback** to SSE if WebSocket fails

### Requirements

- Only works with Responses API models: `gpt-4.1+`, `o1`, `o3`, `o4`, `gpt-5`
- NOT compatible with the `--models-gateway` flag (automatically falls back to SSE when a gateway is configured)
- Requires `OPENAI_API_KEY` environment variable

### Example

See [`examples/websocket_transport.yaml`](https://github.com/docker/docker-agent/blob/main/examples/websocket_transport.yaml) for a complete example.
