---
title: "OpenAI"
description: "Use GPT-5.6, GPT-4o, GPT-4.1, and other OpenAI models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, openai
weight: 200
canonical: https://docs.docker.com/ai/docker-agent/providers/openai/
---

_Use GPT-5.6, GPT-4o, GPT-4.1, and other OpenAI models with Docker Agent._

## Setup

```bash
# Set your API key
export OPENAI_API_KEY="sk-..."
```

> [!TIP]
> No API key? A ChatGPT Plus/Pro/Business subscription can be used instead
> through the [`chatgpt` provider](../chatgpt/index.md): sign in once with
> `docker agent setup` (pick chatgpt).

## Configuration

### Inline

```yaml
agents:
  root:
    model: openai/gpt-5.6
```

### Named Model

```yaml
models:
  gpt:
    provider: openai
    model: gpt-5.6
    max_tokens: 4000
```

## Available Models

| Model            | Best For                                             |
| ---------------- | ----------------------------------------------------- |
| `gpt-5.6`         | Alias for `gpt-5.6-sol`; tracks the flagship model    |
| `gpt-5.6-sol`     | Frontier model, most capable, complex reasoning       |
| `gpt-5.6-terra`   | Everyday workhorse; successor to the `-mini` tier     |
| `gpt-5.6-luna`    | High-volume, cost-efficient; successor to `-nano` tier |
| `gpt-4.1`         | Previous-generation flagship                          |
| `gpt-4.1-mini`    | Previous-generation fast, cost-effective model        |
| `gpt-4o`          | Multimodal, balanced performance                      |
| `gpt-4o-mini`     | Cheapest, fast for simple tasks                       |

Starting with GPT-5.6, OpenAI renamed the `-mini`/`-nano` size tiers to `-terra`/`-luna` (with `-sol` denoting the frontier tier previously left unsuffixed).

Find more model names at [modelnames.ai](https://modelnames.ai/) or in the [official OpenAI docs](https://platform.openai.com/docs/models).

## Service Tier (Fast Mode)

Set `provider_opts.service_tier` to request OpenAI's [Fast mode](https://developers.openai.com/api/docs/guides/fast-mode):

```yaml
models:
  fast-gpt:
    provider: openai
    model: gpt-5.6
    provider_opts:
      service_tier: fast
```

OpenAI also accepts `priority` for Fast mode. It provides faster processing at premium pricing on supported models, without reducing reasoning effort. This is independent of `thinking_budget` and applies to all requests using the configured model, including internal calls such as title generation and compaction.

The value is forwarded unchanged to Chat Completions (including reranking) and Responses requests, over either SSE or WebSocket. OpenAI-compatible providers using these APIs also receive the option when set; the endpoint must support it. Other tiers, such as `auto`, `default`, and `flex`, can also be requested; availability and valid values depend on the API and model. When omitted or empty, no `service_tier` is sent, leaving the API's default behavior unchanged. Non-string values are ignored.

> [!WARNING]
> Docker Agent's cost estimates do not automatically adjust for `service_tier`. By default, they use catalogue pricing, which can underestimate premium-tier charges. Set the model's [`cost` override](../../configuration/models/index.md#custom-token-pricing) to the applicable input, output, and cache token rates for your tier.

See [`examples/openai-service-tier.yaml`](https://github.com/docker/docker-agent/blob/main/examples/openai-service-tier.yaml) for a complete example.

## Thinking Budget

OpenAI reasoning models (o-series, gpt-5, gpt-5-mini, gpt-5.6 family) support extended thinking through the `reasoning_effort` API parameter. Set `thinking_budget` to control the effort level:

```yaml
models:
  gpt-thinker:
    provider: openai
    model: gpt-5.6
    thinking_budget: high   # none | minimal | low | medium | high | xhigh | max
```

**Effort levels:**

| Level     | Description                                              |
| --------- | -------------------------------------------------------- |
| `none`    | No reasoning. On `gpt-5.6`+ this is a real API value that is sent as-is; on older models it just disables the local `thinking_budget` (the API's own default still applies). |
| `minimal` | Fastest; lightest reasoning pass. Not accepted on `gpt-5.6`+ (dropped from the API). |
| `low`     | Quick reasoning for straightforward tasks.               |
| `medium`  | Balanced default.                                        |
| `high`    | More thorough; recommended for complex tasks.            |
| `xhigh`   | Near-maximum effort; slower but most accurate. Requires `gpt-5.2`+. |
| `max`     | Maximum effort. Requires `gpt-5.6`+ (Sol/Terra/Luna).    |

Token counts, `adaptive`, and `adaptive/<effort>` are rejected with a configuration error at request time. Older models (o1, o3-mini) only accept `low`/`medium`/`high`; `xhigh` requires `gpt-5.2`+; `none` and `max` require `gpt-5.6`+; `minimal` is not accepted on `gpt-5.6`+.

> [!WARNING]
> **Hidden reasoning tokens**
>
> OpenAI reasoning models always produce hidden reasoning tokens that count against `max_tokens` — even with `thinking_budget: none` on older models. Docker Agent automatically raises the output-token floor for its internal low-effort calls so reasoning cannot starve visible text output.

See the [Thinking / Reasoning guide](../../guides/thinking/index.md) for a cross-provider overview.

> [!TIP]
> **Custom endpoints**
>
> Use `base_url` for proxies and OpenAI-compatible services. See [Custom Providers](../custom/index.md) for full setup.

## Custom Endpoint

Use `base_url` to connect to OpenAI-compatible APIs:

```yaml
models:
  custom:
    provider: openai
    model: gpt-5.6-terra
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
