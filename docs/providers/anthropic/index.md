---
title: "Anthropic"
description: "Use Claude Sonnet 4, Claude Sonnet 4.5, and other Anthropic models with docker-agent."
permalink: /providers/anthropic/
---

# Anthropic

_Use Claude Sonnet 4, Claude Sonnet 4.5, and other Anthropic models with docker-agent._

## Setup

```bash
# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."
```

### Workload Identity Federation (no API key)

Authenticate with short-lived tokens minted from your own OIDC identity
provider instead of a long-lived API key. See Anthropic's
[Workload Identity Federation guide](https://platform.claude.com/docs/en/build-with-claude/workload-identity-federation)
to provision a Federation Rule, then configure docker-agent with a typed
`auth:` block:

```yaml
providers:
  anthropic-wif:
    provider: anthropic
    auth:
      type: workload_identity_federation
      workload_identity_federation:
        federation_rule_id: fdrl_REPLACE_ME
        organization_id: 00000000-0000-0000-0000-000000000000
        # Optional: only required for target_type=SERVICE_ACCOUNT rules.
        service_account_id: svac_REPLACE_ME
        identity_token:
          # Pick exactly one of: file, env, command, url
          file: /var/run/secrets/anthropic.com/token

models:
  claude:
    provider: anthropic-wif
    model: claude-sonnet-4-5
```

`identity_token` accepts four mutually exclusive sources:

| Source    | When to use                                                                                                              |
| --------- | ------------------------------------------------------------------------------------------------------------------------ |
| `file`    | Kubernetes projected service-account tokens, SPIFFE/SPIRE helpers, Vault sidecars — anything that rotates a file on disk |
| `env`     | The token is already exported in an environment variable                                                                 |
| `command` | Shell out to a CLI on every refresh (`gcloud auth print-identity-token`, `az account get-access-token`, ...)              |
| `url`     | Fetch from an HTTP(S) endpoint (cloud metadata servers, GitHub Actions OIDC token URL, ...)                              |

For `url`, both the URL and any header values support `${VAR}` expansion
against the runtime environment, which lets you wire the GitHub Actions OIDC
token endpoint without putting secrets in YAML:

```yaml
identity_token:
  url: ${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=https://api.anthropic.com
  headers:
    Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}
  response_field: value
```

`auth:` is mutually exclusive with `--gateway`. Token-refresh failures are
surfaced through the normal error path with a clear `anthropic workload
identity federation: failed to refresh identity token from <kind> source
(federation_rule=fdrl_…): ...` message in the TUI.

A complete walkthrough of all four sources lives in
[`examples/anthropic_wif.yaml`]({{ site.examples_url }}/anthropic_wif.yaml).

## Configuration

### Inline

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
```

### Named Model

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    max_tokens: 64000
```

## Available Models

| Model ID            | Description                                                 |
| ------------------- | ----------------------------------------------------------- |
| `claude-opus-4-8`   | Highest-capability Opus model; uses adaptive thinking       |
| `claude-opus-4-7`   | Opus model with adaptive thinking and task budget support   |
| `claude-sonnet-4-5` | Most capable Sonnet; supports extended thinking             |
| `claude-sonnet-4-0` | Previous Sonnet generation, still supported                 |
| `claude-haiku-4-5`  | Fast and inexpensive, good for tight loops                  |

## Thinking Budget

Anthropic supports both token budgets and adaptive thinking. Thinking is off unless you set `thinking_budget`; when set, interleaved thinking is auto-enabled.

Use numeric token budgets with Claude Sonnet and older Opus models:

```yaml
models:
  claude-deep:
    provider: anthropic
    model: claude-sonnet-4-5
    thinking_budget: 16384 # must be < max_tokens
```

Use adaptive thinking with Claude Opus 4.7+ (including Opus 4.8):

```yaml
models:
  opus:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive/high # adaptive | adaptive/low | adaptive/medium | adaptive/high | adaptive/xhigh | adaptive/max
```

Claude Opus 4.7+ rejects token-based thinking requests (`thinking.type=enabled`). docker-agent converts numeric budgets on these models to adaptive thinking, but new configs should prefer `adaptive` or `adaptive/<level>` directly.

## Interleaved Thinking

Auto-enabled whenever a thinking budget is configured on a Claude model. Allows tool calls during model reasoning for more integrated problem-solving:

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    provider_opts:
      interleaved_thinking: false # disable if needed
```

## Task Budget

`task_budget` caps the **total** number of tokens the model may spend across a
multi-step agentic task — combined thinking, tool calls, and final output. It
is forwarded as
[`output_config.task_budget`](https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7)
and is ideal for letting long-running agents self-regulate effort without
tightening `max_tokens` on every call.

docker-agent automatically attaches the required `task-budgets-2026-03-13`
beta header whenever this field is set. You can configure `task_budget` on
**any** Claude model — docker-agent never gates it by model name. At the time
of writing, only **Claude Opus 4.7** actually honors the field; other Claude
models (Sonnet 4.5, Opus 4.5 / 4.6, etc.) are expected to reject requests
that include it. Check the Anthropic release notes linked above for the
current list of supported models.

```yaml
models:
  opus:
    provider: anthropic
    model: claude-opus-4-7
    task_budget: 128000 # integer shorthand → { type: tokens, total: 128000 }
    thinking_budget: adaptive
```

Object form (forward-compatible with future budget types):

```yaml
  opus:
    provider: anthropic
    model: claude-opus-4-7
    task_budget:
      type: tokens
      total: 128000
```

See the full schema on the [Model Configuration]({{ '/configuration/models/#task-budget' | relative_url }}) page.

## Server-Side Fallbacks

When the primary model refuses a request (e.g. Claude Fable 5's safety
classifiers ending the turn with stop reason `refusal`), Anthropic can retry
the request with backup models in a single round trip. Set `fallbacks` in
`provider_opts` to a list of model IDs, in priority order:

```yaml
models:
  fable:
    provider: anthropic
    model: claude-fable-5
    provider_opts:
      fallbacks:
        - claude-opus-4-8
        - claude-sonnet-4-6
```

docker-agent automatically attaches the required
`server-side-fallback-2026-06-01` beta header and forwards the option as
`fallbacks: [{"model": "..."}]`. The response's `model` field reports which
model actually served the request.

Fallback models receive the exact same request as the primary model
(thinking configuration, task budget, beta features, ...), so list only
models that accept the same request shape. Not available on Bedrock, Vertex
AI, or the Message Batches API.

## Thinking Display

Controls whether thinking blocks are returned in responses when thinking is enabled. Claude Opus 4.7+ hides thinking content by default (`omitted`); earlier Claude 4 models default to `summarized`. Set `thinking_display` in `provider_opts` to override:

```yaml
models:
  claude-opus-4-8:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive
    provider_opts:
      thinking_display: summarized # "summarized", "display", or "omitted"
```

Valid values:

- `summarized`: thinking blocks are returned with summarized thinking text (default for Claude 4 models prior to Opus 4.7).
- `display`: thinking blocks are returned for display (use this to re-enable thinking output on Opus 4.7).
- `omitted`: thinking blocks are returned with an empty thinking field; the signature is still returned for multi-turn continuity (default for Opus 4.7+). Useful to reduce time-to-first-text-token when streaming.

Note: `thinking_display` applies to both `thinking_budget` with token counts and adaptive/effort-based budgets. Full thinking tokens are billed regardless of the `thinking_display` value.

<div class="callout callout-info" markdown="1">
<div class="callout-title">Note
</div>
  <p>Anthropic thinking budget values below 1024 or greater than or equal to <code>max_tokens</code> are ignored (a warning is logged).</p>

</div>
