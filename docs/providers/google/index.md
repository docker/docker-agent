---
title: "Google Gemini"
description: "Use Gemini 2.5 Flash, Gemini 3.1 Pro, and other Google models with Docker Agent."
keywords: docker agent, ai agents, model providers, llm, google gemini
weight: 120
canonical: https://docs.docker.com/ai/docker-agent/providers/google/
---

_Use Gemini 2.5 Flash, Gemini 3.1 Pro, and other Google models with Docker Agent._

## Setup

Docker Agent reads the first credential it finds from these environment variables (see `pkg/model/provider/gemini/client.go`):

| Variable                    | Purpose                                                                             |
| --------------------------- | ----------------------------------------------------------------------------------- |
| `GOOGLE_API_KEY`            | Primary Gemini API key.                                                             |
| `GEMINI_API_KEY`            | Alternative name for the Gemini API key (also used by the official Google SDK).     |
| `GOOGLE_GENAI_USE_VERTEXAI` | When set (any value), routes through Vertex AI instead of the Gemini Developer API. |
| `GOOGLE_CLOUD_PROJECT`      | GCP project used when `GOOGLE_GENAI_USE_VERTEXAI` is set or for Vertex AI Model Garden. |
| `GOOGLE_CLOUD_LOCATION`     | GCP region for Vertex AI (defaults to the SDK default).                             |

On the Gemini Developer API, a model or [custom provider](../custom/index.md) that sets `token_key` reads its key from that variable instead of `GOOGLE_API_KEY` / `GEMINI_API_KEY`. The Vertex AI backends use Application Default Credentials and ignore `token_key`.

```bash
# Gemini Developer API
export GOOGLE_API_KEY="AI..."   # or GEMINI_API_KEY

# Vertex AI (no API key; uses Application Default Credentials)
gcloud auth application-default login
export GOOGLE_GENAI_USE_VERTEXAI=1
export GOOGLE_CLOUD_PROJECT="my-gcp-project"
export GOOGLE_CLOUD_LOCATION="us-central1"
```

## Configuration

### Inline

```yaml
agents:
  root:
    model: google/gemini-3.8-flash
```

### Named Model

```yaml
models:
  gemini:
    provider: google
    model: gemini-3.8-flash
    temperature: 0.5
```

## Available Models

| Model                     | Best For                        |
| -------------------------- | ------------------------------- |
| `gemini-3.1-pro-preview`  | Most capable Gemini model       |
| `gemini-3.8-flash`        | Fast, efficient, good balance   |
| `gemini-2.5-flash`        | Fast inference, cost-effective  |
| `gemini-2.5-pro`          | Strong reasoning, large context |

## Generated Images

Some Gemini models (e.g. `gemini-2.5-flash-image`) are designed to generate
an image directly as part of their reply, not just describe one. When the
model's image output capability resolves as enabled — see
[Output capabilities](../../configuration/models/index.md#output-capabilities)
for the `output_capabilities.image` / models.dev precedence rules — Docker
Agent requests that image output on supported Google surfaces: the models
gateway, direct Gemini API, and Vertex AI. Each eligible ordinary chat request
asks for text *and* image output. Vertex AI has deterministic guard/predicate
coverage; live image-generation validation is deferred.

```yaml
models:
  gemini-image:
    provider: google
    model: gemini-2.5-flash-image
```

`output_capabilities.image` can be omitted for models that models.dev lists
with image output; set it explicitly for custom models or to override
incorrect catalogue data. Capability is never guessed from the model name.

Session-title and compaction requests omit image response modalities and
bypass the guard even for image-output-capable models. They do not explicitly
force TEXT-only output. Ordinary image-output requests with custom function tools or
structured output are rejected locally before any request is sent. Google
Search, Maps, and code-execution built-ins remain available. When custom
tools conflict, Docker Agent uses models.dev's `tool_call` capability to clarify
whether the model cannot call tools at all or supports tools only outside an
image-output request. Unknown catalogue data keeps the conservative generic
message.

A few provider-side behaviors to know:

- **The provider decides the image format** (typically PNG). Asking for a
  `.gif` or `.svg` filename does not transcode anything — the saved file's
  extension is corrected to match the data actually returned.
- **An image is not guaranteed.** Even a correctly configured image model
  can answer with text only and generate no image; that is provider behavior,
  so reword or repeat the prompt. At a text-only stop, Docker Agent may add a
  nonfatal phrase-based warning to the turn — see
  [Generated Media](../../features/tui/index.md#generated-media) for the exact
  check and its limitations.

Docker Agent attempts to save each returned image into the session workspace,
keep a portable copy in the session database, and display it in the TUI. See
[Generated Media Files](../../features/sessions/index.md#generated-media-files)
for persistence and portability, and
[Generated Media](../../features/tui/index.md#generated-media) for file
naming, collision handling, and rendering.

## Thinking Budget

Gemini supports two approaches depending on the model version:

> [!WARNING]
> **Different thinking formats**
>
> Gemini 2.5 uses **token-based** budgets (integers). Gemini 3 uses **level-based** budgets (strings like `low`, `high`). Make sure you use the right format for your model version.

### Gemini 2.5 (Token-based)

```yaml
models:
  gemini-no-thinking:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: 0 # disable thinking

  gemini-dynamic:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: -1 # dynamic (model decides) — default

  gemini-fixed:
    provider: google
    model: gemini-2.5-flash
    thinking_budget: 8192 # fixed token budget
```

### Gemini 3 (Level-based)

```yaml
models:
  gemini-pro:
    provider: google
    model: gemini-3.1-pro-preview
    thinking_budget: high # default for Pro: low | high

  gemini-flash:
    provider: google
    model: gemini-3.8-flash
    thinking_budget: medium # default for Flash: minimal | low | medium | high
```

## Built-in Tools (Grounding)

Gemini models support built-in tools that let the model access Google Search and Google Maps
directly during generation. Enable them via `provider_opts`:

```yaml
models:
  gemini-grounded:
    provider: google
    model: gemini-2.5-flash
    provider_opts:
      google_search: true
      google_maps: true
      code_execution: true
```

| Option           | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `google_search`  | Enables Google Search grounding for up-to-date info  |
| `google_maps`    | Enables Google Maps grounding for location queries   |
| `code_execution` | Enables server-side code execution for computations  |

## Vertex AI Model Garden

You can use non-Gemini models (e.g. Claude, Llama) hosted on Google Cloud's
[Vertex AI Model Garden](https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/use-partner-models)
through the `google` provider. When a `publisher` is specified in `provider_opts`,
requests are routed through the appropriate Vertex AI endpoint instead of the
Gemini SDK:

- **Anthropic Claude** (`publisher: anthropic`) uses the Anthropic-native
  `:rawPredict` / `:streamRawPredict` endpoints. Claude models on Vertex AI do
  not support the OpenAI `/chat/completions` path.
- **Other publishers** (e.g. `meta`, `mistral`) use Vertex AI's
  OpenAI-compatible `/chat/completions` endpoint.

### Authentication

Vertex AI uses Google Cloud Application Default Credentials (ADC). Make sure you
are authenticated:

```bash
gcloud auth application-default login
```

### Configuration

```yaml
models:
  claude-on-vertex:
    provider: google
    model: claude-sonnet-5
    provider_opts:
      project: my-gcp-project       # GCP project ID (or set GOOGLE_CLOUD_PROJECT)
      location: us-east5             # GCP region (or set GOOGLE_CLOUD_LOCATION)
      publisher: anthropic           # Model publisher (anthropic, meta, etc.)
```

| Option      | Description                                                                          |
| ----------- | ------------------------------------------------------------------------------------ |
| `project`   | GCP project ID. Falls back to `GOOGLE_CLOUD_PROJECT` env var                         |
| `location`  | GCP region (e.g. `us-east5`, `us-central1`). Falls back to `GOOGLE_CLOUD_LOCATION`   |
| `publisher`  | Model publisher (e.g. `anthropic`, `meta`, `mistral`). Must not be `google`          |

> [!NOTE]
> **Gemini models on Vertex AI**
>
> Setting `publisher: google` (or omitting `publisher`) uses the native Gemini SDK path. The Model Garden endpoint is only used for non-Google publishers.
