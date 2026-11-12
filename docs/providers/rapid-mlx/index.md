---
title: "Rapid-MLX"
description: "Run Docker Agent with local MLX models served by Rapid-MLX on Apple Silicon."
keywords: docker agent, ai agents, model providers, local models, rapid-mlx, mlx, apple silicon
weight: 247
canonical: https://docs.docker.com/ai/docker-agent/providers/rapid-mlx/
---

_Run Docker Agent with local MLX models served by Rapid-MLX on Apple Silicon._

## Overview

[Rapid-MLX](https://github.com/raullenchai/Rapid-MLX) is an open-source
command-line server for running MLX models locally on Apple Silicon. Docker
Agent connects to its OpenAI-compatible API through a
[provider definition](../custom/index.md). No built-in `rapid-mlx` alias or
additional provider plugin is required.

Rapid-MLX requires an Apple Silicon Mac. Model downloads require network
access; inference runs locally after the model is downloaded.

## Setup

1. Install Rapid-MLX with Homebrew or pip (Python 3.10 or newer):

   ```bash
   $ brew install rapid-mlx
   # or
   $ pip install rapid-mlx
   ```

2. Start the server with a model. For a small first download, use the
   `qwen3.5-4b-4bit` alias
   ([`mlx-community/Qwen3.5-4B-MLX-4bit`](https://huggingface.co/mlx-community/Qwen3.5-4B-MLX-4bit)):

   ```bash
   $ rapid-mlx serve qwen3.5-4b-4bit --port 8000
   ```

   Keep the server running while you use Docker Agent. It listens on
   `127.0.0.1` by default, and its OpenAI base URL is
   `http://127.0.0.1:8000/v1`. Without `--port`, Rapid-MLX picks the first
   free port in `8000`–`8009`; an explicit `--port` never falls back.
3. Check that the server responds:

   ```bash
   $ curl http://127.0.0.1:8000/health
   $ curl http://127.0.0.1:8000/v1/models
   ```

If you started the server with an API key, include it as a Bearer token in
requests. See [Authentication](#authentication) below.

## Configuration

Save this as `rapid-mlx.yaml`:

```yaml
providers:
  rapid-mlx:
    api_type: openai_chatcompletions
    base_url: http://127.0.0.1:8000/v1
    provider_opts:
      extra_body:
        chat_template_kwargs:
          enable_thinking: false

models:
  local:
    provider: rapid-mlx
    model: qwen3.5-4b-4bit
    temperature: 0
    max_tokens: 512

agents:
  root:
    model: local
    description: Local chat assistant using Rapid-MLX on Apple Silicon
    instruction: You are a concise helpful assistant. Answer directly.
```

Run a short chat request:

```bash
$ docker agent run rapid-mlx.yaml --exec "What is the capital of France? Answer with only the city name."
```

A copy is available in
[`examples/rapid-mlx.yaml`](https://github.com/docker/docker-agent/blob/main/examples/rapid-mlx.yaml).
If you serve a different model, use the ID reported by `/v1/models` in
`models.local.model`; `default` always resolves to the loaded model. Update
`base_url` if the server uses another port.

`provider_opts.extra_body.chat_template_kwargs.enable_thinking: false` turns
off reasoning for models that support it, so the output budget goes to the
answer. See [Extra Request Body](../../configuration/models/index.md#extra-request-body)
for other pass-through settings.

Rapid-MLX also exposes the OpenAI Responses API. To use it, change
`providers.rapid-mlx.api_type` to `openai_responses`; keep the same base URL.
Docker Agent does not forward `provider_opts.extra_body` with Responses
requests, so the thinking override above applies only to Chat Completions.

## Authentication

If you start the server with `--api-key` (or the `RAPID_MLX_API_KEY`
environment variable), add `token_key: RAPID_MLX_API_KEY` under
`providers.rapid-mlx` and set that environment variable to the same key:

```bash
$ export RAPID_MLX_API_KEY=your-server-api-key
$ rapid-mlx serve qwen3.5-4b-4bit --port 8000
$ curl http://127.0.0.1:8000/v1/models -H "Authorization: Bearer $RAPID_MLX_API_KEY"
$ docker agent run rapid-mlx.yaml --exec "Hello"
```

`token_key` names an environment variable, not a literal token. Omit it when
the server runs without an API key.

## Tool Calling

The example is chat-only. Rapid-MLX parses tool calls for the models it
serves, and the parser is selected automatically from the model alias, but
reliability still depends on the model, instructions, and tool schemas. A 4B
model is useful for testing the connection; choose a larger tool-capable model
for agentic coding work.

Before adding filesystem or shell tools, test that the model calls the tool
with correct arguments and uses the returned result. Keep tool descriptions and
parameter schemas clear, and retain tool approvals. See
[Tools](../../concepts/tools/index.md) for configuration.

## Troubleshooting

- **Connection refused:** Start `rapid-mlx serve` and check which port it
  printed. Without `--port`, it may have selected a port other than `8000`.
- **Model not found:** Check `/v1/models` for the loaded model ID, or use
  `default`.
- **Unauthorized:** Match `RAPID_MLX_API_KEY` to the server's `--api-key`, or
  remove `token_key` when authentication is disabled.
- **No answer before the token limit:** Keep thinking disabled, or increase
  `max_tokens` for a model that needs longer responses.
- **Docker Agent runs in a container:** Container-local `127.0.0.1` does not
  refer to your Mac. On Docker Desktop, use
  `http://host.docker.internal:8000/v1` and start the server with a host
  reachable from the container (for example `--host 0.0.0.0`). Set an API key
  before exposing the server beyond loopback; binding to `0.0.0.0` exposes it
  to your LAN.

For server options and endpoint details, see the Rapid-MLX
[CLI reference](https://github.com/raullenchai/Rapid-MLX/blob/main/docs/reference/cli.md).
