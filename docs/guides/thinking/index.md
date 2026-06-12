---
title: "Thinking / Reasoning"
description: "Control how much a model reasons before responding. Works across OpenAI, Anthropic, Google Gemini, and AWS Bedrock."
permalink: /guides/thinking/
---

# Thinking / Reasoning

_Control how much a model reasons before responding. Works across OpenAI, Anthropic, Google Gemini, and AWS Bedrock._

## What Is Thinking?

Several modern models support an extended reasoning phase that happens before they produce visible output. During this phase the model plans, evaluates options, and works through the problem — internally, not shown in the response by default. This typically improves accuracy on complex tasks like coding, math, and multi-step planning, at the cost of higher token usage and latency.

docker-agent exposes this through a single `thinking_budget` field on any named model. The value format differs slightly by provider, but the semantics are the same: higher effort means more thorough reasoning.

<div class="callout callout-info" markdown="1">
<div class="callout-title">Think tool vs. thinking budget
</div>
  <p>The <a href="{{ '/tools/think/' | relative_url }}">think tool</a> is a scratchpad for models that lack native reasoning. If your model supports <code>thinking_budget</code>, you do not need the think tool.</p>
</div>

## Quick Reference

| Provider       | Format     | Values                                                       | Default      |
| -------------- | ---------- | ------------------------------------------------------------ | ------------ |
| OpenAI         | string     | `minimal`, `low`, `medium`, `high`, `xhigh`, `none`, `adaptive/<level>` (`max` only via `adaptive/max`) | `medium`     |
| Anthropic      | int or str | 1024–32768 tokens (Sonnet / older Opus), or `adaptive`, `adaptive/<level>`, `low`–`max`, `none` | off          |
| Gemini 2.5     | int        | `0` (off), `-1` (dynamic), or token count (max 24576 / 32768) | `-1` (dynamic)|
| Gemini 3       | string     | `minimal`, `low`, `medium`, `high`                           | model-dependent |
| AWS Bedrock    | int or str | 1024–32768 tokens for Claude token-budget models; `adaptive` / `adaptive/<level>` for Opus 4.7+ | off          |
| xAI / Mistral  | string     | `minimal`, `low`, `medium`, `high`, `xhigh`, `none`          | off          |

## OpenAI

OpenAI reasoning models (o-series, gpt-5, gpt-5-mini) use a string effort level that maps to their `reasoning_effort` API parameter.

```yaml
models:
  gpt-thinker:
    provider: openai
    model: gpt-5-mini
    thinking_budget: high   # minimal | low | medium | high | xhigh
```

**Effort levels:**

| Level     | Description                                              |
| --------- | -------------------------------------------------------- |
| `none`    | Disable thinking (alias for `0`). Minimum output floor still applies. |
| `minimal` | Fastest; lightest reasoning pass.                        |
| `low`     | Quick reasoning for straightforward tasks.               |
| `medium`  | Balanced default.                                        |
| `high`    | More thorough; recommended for complex tasks.            |
| `xhigh`   | Near-maximum effort; slower but most accurate.           |

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Tokens and max_tokens
</div>
  <p>OpenAI reasoning models always reason internally — even with <code>thinking_budget: none</code> there are hidden reasoning tokens that count against <code>max_tokens</code>. docker-agent automatically raises the output-token floor so hidden reasoning cannot starve visible text output.</p>
</div>

### Adaptive thinking (OpenAI)

Use `adaptive/<level>` to let the model decide when to apply extended reasoning and scale effort dynamically:

```yaml
models:
  gpt-adaptive:
    provider: openai
    model: gpt-5-mini
    thinking_budget: adaptive/medium  # adaptive/low | adaptive/medium | adaptive/high | adaptive/xhigh | adaptive/max
```

> `max` is only valid inside the `adaptive/` prefix — `thinking_budget: max` (bare) is not accepted by OpenAI.

## Anthropic

Anthropic Claude supports two thinking modes: a **token budget** (older models) and **adaptive / effort-based** thinking (newer models).

### Token budget (Claude Sonnet and older Opus models)

Set an explicit number of thinking tokens (1024–32768). This must be less than `max_tokens`:

```yaml
models:
  claude-thinker:
    provider: anthropic
    model: claude-sonnet-4-5
    thinking_budget: 16384   # tokens reserved for internal reasoning
```

docker-agent auto-adjusts `max_tokens` when you set a thinking budget but leave `max_tokens` at its default. If you set `max_tokens` explicitly, it must be greater than `thinking_budget`.

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Opus 4.7+ uses adaptive thinking</div>
  <p>Claude Opus 4.7 and newer, including Opus 4.8, reject token-based thinking requests. Prefer <code>thinking_budget: adaptive</code> or <code>thinking_budget: adaptive/high</code>. docker-agent converts numeric budgets on these models to adaptive thinking for compatibility.</p>
</div>

### Adaptive thinking (Claude Opus 4.6+)

Newer Claude models support adaptive thinking, where the model decides how much to think. Use `adaptive` or pair it with an effort level:

```yaml
models:
  claude-adaptive:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive          # model decides effort

  claude-adaptive-low:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive/low      # adaptive with low effort: low | medium | high | xhigh | max
```

**Adaptive effort levels:**

| Level     | Description                                       |
| --------- | ------------------------------------------------- |
| `low`     | Minimal thinking; fastest adaptive mode.          |
| `medium`  | Moderate effort.                                  |
| `high`    | Thorough reasoning; default for `adaptive`.       |
| `xhigh`   | Extra-high reasoning effort.                      |
| `max`     | Maximum effort.                                   |

### Disabling thinking

```yaml
thinking_budget: none   # or 0
```

### Interleaved thinking

Interleaved thinking lets the model reason between tool calls — useful for complex agentic tasks. docker-agent auto-enables it whenever a thinking budget is configured on a Claude model, so you only need to set it explicitly to turn it off:

```yaml
models:
  claude-interleaved:
    provider: anthropic
    model: claude-sonnet-4-5
    thinking_budget: 16384
    # interleaved_thinking is auto-enabled; disable it explicitly if needed:
    provider_opts:
      interleaved_thinking: false
```

<div class="callout callout-info" markdown="1">
<div class="callout-title">Temperature and top_p
</div>
  <p>When extended thinking is enabled, Anthropic requires <code>temperature=1.0</code>. docker-agent automatically suppresses any <code>temperature</code> or <code>top_p</code> settings you have configured — they are silently ignored while thinking is active.</p>
</div>

### Thinking display

Claude Opus 4.7+ hides thinking content by default. Use `thinking_display` in `provider_opts` to control what you receive:

```yaml
models:
  opus-48:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive
    provider_opts:
      thinking_display: summarized   # summarized | display | omitted
```

| Value        | Behavior                                                                              |
| ------------ | ------------------------------------------------------------------------------------- |
| `summarized` | Thinking blocks returned with a text summary (default for Claude 4 models pre-4.7).  |
| `display`    | Full thinking blocks returned for display.                                            |
| `omitted`    | Thinking blocks hidden — only the signature is returned (default for Opus 4.7+).      |

Full thinking tokens are billed regardless of `thinking_display`.

### Task budget (Anthropic)

`task_budget` caps total tokens across an entire multi-step agentic task (thinking + tool calls + output combined):

```yaml
models:
  opus-bounded:
    provider: anthropic
    model: claude-opus-4-7
    thinking_budget: adaptive
    task_budget: 128000   # total token ceiling for the whole task
```

See the [Anthropic provider page]({{ '/providers/anthropic/#task-budget' | relative_url }}) for details.

## Google Gemini

Gemini 2.5 and Gemini 3 use different formats.

### Gemini 2.5 (token budget)

```yaml
models:
  gemini-off:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: 0      # disable thinking

  gemini-dynamic:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: -1     # let the model decide (default)

  gemini-fixed:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: 8192   # fixed token budget (max 24576 for Flash, 32768 for Pro)
```

### Gemini 3 (level-based)

```yaml
models:
  gemini3-flash:
    provider: google
    model: gemini-3-flash
    thinking_budget: medium   # minimal | low | medium | high

  gemini3-pro:
    provider: google
    model: gemini-3-pro
    thinking_budget: high     # low | high (Pro supports fewer levels)
```

## AWS Bedrock (Claude)

Bedrock Claude supports extended thinking — an internal reasoning phase before the model produces its response. Most Claude models use a token budget; Claude Opus 4.7+ uses adaptive thinking instead:

```yaml
models:
  bedrock-claude-adaptive:
    provider: amazon-bedrock
    model: global.anthropic.claude-opus-4-8-20260601-v1:0
    thinking_budget: adaptive/high   # adaptive | adaptive/low | adaptive/medium | adaptive/high | adaptive/xhigh | adaptive/max
    provider_opts:
      region: us-east-1
```

For models that still accept token budgets, use an integer token count (1024–32768) or an effort level string that maps automatically:

| Effort level | Token budget |
| ------------ | ------------ |
| `minimal`    | 1,024        |
| `low`        | 2,048        |
| `medium`     | 8,192        |
| `high`       | 16,384       |
| `xhigh`/`max`| 32,768       |

```yaml
models:
  bedrock-claude-thinker:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    thinking_budget: 8192   # or use an effort level: medium
    provider_opts:
      region: us-east-1

  bedrock-claude-interleaved:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    thinking_budget: high
    provider_opts:
      region: us-east-1
      # interleaved_thinking is auto-enabled when thinking_budget is set
```

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Bedrock thinking requirements
</div>
  <p>Bedrock Claude requires <code>thinking_budget</code> to be ≥ 1024 and less than <code>max_tokens</code>. docker-agent logs a warning and ignores the budget if either condition is violated. Interleaved thinking requires the <code>interleaved-thinking-2025-05-14</code> beta header, which docker-agent adds automatically; it is auto-enabled whenever a thinking budget is set on a Bedrock-hosted Claude model.</p>
</div>

## xAI (Grok) and Mistral

Both providers use the OpenAI-compatible API and accept the same effort strings:

```yaml
models:
  grok-thinker:
    provider: xai
    model: grok-3
    thinking_budget: high   # minimal | low | medium | high | xhigh | none

  mistral-thinker:
    provider: mistral
    model: mistral-large-latest
    thinking_budget: medium  # minimal | low | medium | high | xhigh | none
```

Thinking is **off by default** for these providers. Set `thinking_budget` explicitly to enable it.

## Disabling Thinking

Use `none` or `0` to disable thinking on any provider:

```yaml
models:
  fast-model:
    provider: openai
    model: gpt-5-mini
    thinking_budget: none

  gemini-no-think:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: 0
```

## Choosing an Effort Level

| Task complexity                  | Recommended level       |
| -------------------------------- | ----------------------- |
| Simple factual Q&A               | `none` / `minimal`      |
| General-purpose chat             | `low` / `medium`        |
| Coding, debugging, analysis      | `medium` / `high`       |
| Complex reasoning, planning      | `high` / `xhigh`        |
| Research, difficult math/logic   | `xhigh` / `max`         |
| Long agentic tasks (Anthropic)   | `adaptive`              |

## Changing Thinking Level at Runtime

While running in the TUI, press **Shift+Tab** to cycle the thinking effort level for the current model without editing your YAML config:

- The level steps through the model's supported range (provider-specific), wrapping around — for example `none → minimal → low → medium → high → xhigh → none` on OpenAI.
- The current level is shown in the sidebar next to the model name (e.g. `openai/gpt-5 • high`).
- This applies as a session override — it is **not** saved to the config file. The next session starts from the level defined in your YAML.
- For models that don't support reasoning, and for remote runtimes, Shift+Tab is a no-op and an informational message is displayed.

## Sharing Thinking Config Across Models

Define a provider with a default `thinking_budget` and all models that reference it inherit it:

```yaml
providers:
  deep-anthropic:
    provider: anthropic
    thinking_budget: high
    max_tokens: 32768

models:
  claude-smart:
    provider: deep-anthropic
    model: claude-sonnet-4-5   # inherits thinking_budget: high

  claude-faster:
    provider: deep-anthropic
    model: claude-haiku-4-5
    thinking_budget: low       # overrides to low
```

## Full Example

See [`examples/thinking_budget.yaml`](https://github.com/docker/docker-agent/blob/main/examples/thinking_budget.yaml) for a runnable config covering all providers.
