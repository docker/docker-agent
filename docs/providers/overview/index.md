---
title: "Model Providers"
description: "docker-agent supports multiple AI model providers. Choose the right one for your use case, or use multiple providers in the same configuration."
permalink: /providers/overview/
---

# Model Providers

_docker-agent supports multiple AI model providers. Choose the right one for your use case, or use multiple providers in the same configuration._

## Supported Providers

<div class="cards">
  <a class="card" href="{{ '/providers/openai/' | relative_url }}">
    <div class="card-icon">🟢</div>
    <h3>OpenAI</h3>
    <p>GPT-5, GPT-5-mini, GPT-4o. The most widely used AI models.</p>
  </a>
  <a class="card" href="{{ '/providers/anthropic/' | relative_url }}">
    <div class="card-icon">🟠</div>
    <h3>Anthropic</h3>
    <p>Claude Sonnet 4.5, Claude Opus 4.7. Excellent for coding and analysis.</p>
  </a>
  <a class="card" href="{{ '/providers/google/' | relative_url }}">
    <div class="card-icon">🔵</div>
    <h3>Google Gemini</h3>
    <p>Gemini 2.5 Flash, Gemini 3 Pro. Fast and cost-effective.</p>
  </a>
  <a class="card" href="{{ '/providers/bedrock/' | relative_url }}">
    <div class="card-icon">🟡</div>
    <h3>AWS Bedrock</h3>
    <p>Access Claude, Nova, Llama, and more through AWS infrastructure.</p>
  </a>
  <a class="card" href="{{ '/providers/dmr/' | relative_url }}">
    <div class="card-icon">🐳</div>
    <h3>Docker Model Runner</h3>
    <p>Run models locally with Docker. No API keys, no costs.</p>
  </a>
  <a class="card" href="{{ '/providers/custom/' | relative_url }}">
    <div class="card-icon">🔧</div>
    <h3>Provider Definitions</h3>
    <p>Define reusable provider configurations with shared defaults for any provider type.</p>
  </a>
</div>

## Quick Comparison

| Provider            | Key              | Local? | Strengths                                             |
| ------------------- | ---------------- | ------ | ----------------------------------------------------- |
| OpenAI              | `openai`         | No     | Broad model selection, tool calling, multimodal       |
| Anthropic           | `anthropic`      | No     | Strong coding, extended thinking, large context       |
| Google              | `google`         | No     | Fast inference, competitive pricing, multimodal       |
| AWS Bedrock         | `amazon-bedrock` | No     | Enterprise features, multiple models, AWS integration |
| Docker Model Runner | `dmr`            | Yes    | No API costs, data privacy, offline capable           |

## Additional Built-in Providers

docker-agent also includes built-in aliases for these providers:

| Provider       | Alias            | API Key / Env Variable              |
| -------------- | ---------------- | ----------------------------------- |
| Mistral        | `mistral`        | `MISTRAL_API_KEY`                   |
| xAI (Grok)     | `xai`            | `XAI_API_KEY`                       |
| Nebius         | `nebius`         | `NEBIUS_API_KEY`                    |
| MiniMax        | `minimax`        | `MINIMAX_API_KEY`                   |
| Requesty       | `requesty`       | `REQUESTY_API_KEY`                  |
| Azure OpenAI   | `azure`          | `AZURE_API_KEY` + `base_url`        |
| Ollama         | `ollama`         | None (local; optional `base_url`)   |
| GitHub Copilot | `github-copilot` | `GITHUB_TOKEN` (PAT with `copilot` scope) |

```bash
# Use built-in providers inline
agents:
  root:
    model: mistral/mistral-large-latest
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Multi-provider teams
</div>
  <p>Use expensive models for complex reasoning and cheaper/local models for routine tasks. See the example below.</p>

</div>

## Using Multiple Providers

Different agents can use different providers in the same configuration:

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    max_tokens: 64000
  gpt:
    provider: openai
    model: gpt-5-mini
  local:
    provider: dmr
    model: ai/qwen3

agents:
  root:
    model: claude # coordinator uses Claude
    sub_agents: [coder, helper]
  coder:
    model: gpt # coder uses GPT-5-mini
  helper:
    model: local # helper runs locally for free
```
