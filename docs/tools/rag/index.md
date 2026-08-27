---
title: "RAG Tool"
description: "Give your agents access to document knowledge bases with background indexing, multiple retrieval strategies, and hybrid search."
keywords: docker agent, ai agents, tools, toolsets, rag tool
linkTitle: "RAG"
weight: 110
canonical: https://docs.docker.com/ai/docker-agent/tools/rag/
aliases:
  - /ai/docker-agent/rag/
---

_Give your agents access to document knowledge bases with background indexing, multiple retrieval strategies, and hybrid search._

## Overview

The `rag` toolset lets agents search through your documents to find relevant information before responding. Knowledge bases are declared once at the top of the config under `rag:` and then referenced from any agent via `type: rag, ref: <name>`. Docker Agent supports:

- **Background indexing** — Files are indexed automatically and re-indexed on change
- **Multiple strategies** — Semantic embeddings, BM25 keyword search, and LLM-enhanced search
- **Hybrid search** — Combine strategies with result fusion for best results
- **Reranking** — Re-score results with specialized models for improved relevance

RAG is the strategy to reach for when a document collection is too large to inline directly, or gets queried repeatedly across turns/sessions — see [Choosing a Large-Input Strategy](../../guides/headless/index.md#choosing-a-large-input-strategy) for how it compares to `@`/`/attach` attachments and prompt files.

## Quick Start

```yaml
rag:
  my_docs:
    tool:
      description: "Technical documentation"
    docs: [./documents, ./some-doc.md]
    strategies:
      - type: chunked-embeddings
        embedding_model: openai/text-embedding-3-small
        database: ./docs.db
        vector_dimensions: 1536

agents:
  root:
    model: openai/gpt-4o
    instruction: |
      You have access to a knowledge base. Use it to answer questions.
    toolsets:
      - type: rag
        ref: my_docs
```

## Retrieval Strategies

### Chunked Embeddings (Semantic Search)

Uses embedding models to find semantically similar content. Best for understanding intent, synonyms, and paraphrasing.

```yaml
strategies:
  - type: chunked-embeddings
    embedding_model: openai/text-embedding-3-small
    database: ./vector.db
    vector_dimensions: 1536
    similarity_metric: cosine_similarity
    threshold: 0.5
    limit: 10
    embedding_batch_size: 50
    chunking:
      size: 1000
      overlap: 100
```

### Semantic Embeddings (LLM-Enhanced)

Uses an LLM to generate semantic summaries of each chunk before embedding, capturing meaning and intent. Best for code search and understanding implementations.

```yaml
strategies:
  - type: semantic-embeddings
    embedding_model: openai/text-embedding-3-small
    vector_dimensions: 1536
    chat_model: openai/gpt-4o-mini
    database: ./semantic.db
    ast_context: true # include AST metadata
    chunking:
      size: 1000
      code_aware: true # AST-aware chunking
```

> [!NOTE]
> **Trade-offs**
>
> Semantic embeddings provide higher quality retrieval but slower indexing (LLM call per chunk) and additional API costs.

### BM25 (Keyword Search)

Traditional keyword matching using the BM25 algorithm. Best for exact terms, technical jargon, and code identifiers.

```yaml
strategies:
  - type: bm25
    database: ./bm25.db
    k1: 1.5 # term frequency saturation
    b: 0.75 # length normalization
    threshold: 0.3
    limit: 10
    chunking:
      size: 1000
      overlap: 100
```

## Hybrid Search

Combine multiple strategies for best results. Strategies run in parallel and results are fused together:

```yaml
rag:
  hybrid:
    docs: [./docs]
    strategies:
      - type: chunked-embeddings
        embedding_model: openai/text-embedding-3-small
        database: ./vector.db
        vector_dimensions: 1536
        limit: 20
        chunking: { size: 1000, overlap: 100 }
      - type: bm25
        database: ./bm25.db
        limit: 15
        chunking: { size: 1000, overlap: 100 }
    results:
      fusion:
        strategy: rrf # Reciprocal Rank Fusion
        k: 60
      deduplicate: true
      limit: 5
```

## Fusion Strategies

| Strategy   | Best For                          | Description                                                        |
| ---------- | --------------------------------- | ------------------------------------------------------------------ |
| `rrf`      | General use (recommended)         | Reciprocal Rank Fusion — rank-based, no score normalization needed |
| `weighted` | Known performance characteristics | Weight strategies differently (e.g., embeddings: 0.7, BM25: 0.3)   |
| `max`      | Same scoring scale                | Takes the maximum score from any strategy                          |

## Reranking

Re-score retrieved documents with a specialized model to improve relevance:

```yaml
results:
  reranking:
    model: openai/gpt-4o-mini
    top_k: 10 # only rerank top 10
    threshold: 0.3 # minimum score after reranking
    criteria: |
      Prioritize official documentation over blog posts.
      Prefer recent information and practical examples.
  limit: 5
```

Supported reranking providers: **DMR** (native `/rerank` endpoint), **OpenAI**, **Anthropic**, **Gemini**.

## Code-Aware Chunking

For source code, enable AST-based chunking to keep functions and methods intact:

```yaml
chunking:
  size: 2000
  code_aware: true # Uses tree-sitter for AST-based chunking
```

> [!NOTE]
> **Language Support**
>
> Currently supports Go (`.go`) files. More languages will be added. Falls back to plain text chunking for unsupported file types.

## Indexing failures, retries and backoff

When a knowledge-base fails to start — because the embedding provider is rate-limiting
your requests or returning a transient server error — Docker Agent spaces out retry
attempts with bounded exponential backoff instead of hammering the provider on
every agent turn.

### What triggers backoff

Backoff applies only to **structured HTTP rate-limit (429), request timeout (408), and server error (5xx)**
responses from the embedding or model provider. These are the signals that mean
"slow down", and spacing them out gives the provider room to recover:

| Failure kind | Behaviour |
|---|---|
| HTTP 429 (rate limit) | Backoff: next attempt delayed |
| HTTP 5xx / overload (500, 502, 503, 504, 529) | Backoff: next attempt delayed |
| HTTP 408 (request timeout) | Backoff: next attempt delayed |
| Bad config, invalid model, wrong API key (other 4xx errors) | Fail fast: retried every turn with no added delay |
| Context cancellation or agent shutdown | Immediate: no delay |

### Retry policy and parameters

The backoff is **bounded exponential with additive jitter**:

- **Base delay**: 15 seconds
- **Maximum delay**: 5 minutes
- **Growth**: doubles after each consecutive retryable failure (15s → 30s → 1m → 2m → 4m → 5m)
- **Jitter**: each wait is a random value in `[nominal, 1.2×nominal]` (additive 0–20%)
  so concurrent knowledge-base sources spread their retries and avoid
  hammering the provider together

The gate is a lightweight wall-clock check — it creates no background threads or
timers. A Stop command or agent shutdown takes effect immediately regardless of
how much of the backoff window remains.

### Operational impact

**Before**: a rate-limited knowledge base was re-indexed on every agent turn —
`max_indexing_concurrency × max_embedding_concurrency` concurrent provider calls
could relaunch within milliseconds, easily tripping rate limits for both the
knowledge base and the agent's own model calls.

**After**: retries are spaced out and jittered so the provider has room to recover
before the next attempt. The agent continues working with any other toolsets that
are not affected.

### What you will see

- Docker Agent logs a single warning when a knowledge base first fails to start.
  Repeated failures in between are logged at debug level only, so you are not
  flooded with alerts on every turn. Recovery is intentionally silent — the
  tool appearing in the agent's tool list is the signal that indexing succeeded.
- The knowledge-base tool does not appear in the agent's tool list until indexing
  succeeds. A successful start is silent — the tool is listed and the agent uses it.

### Troubleshooting repeated 429 errors

If you see persistent `429` errors in the logs:

1. **Check provider rate limits.** Your embedding API key may have a low requests-per-minute
   quota. Upgrading the plan or using a different API key can help.
2. **Reduce concurrency.** The chunked-embeddings and semantic-embeddings strategies
   accept `max_indexing_concurrency` (default `3`) and `max_embedding_concurrency`
   (default `3`) parameters. Lowering these reduces simultaneous requests:

   ```yaml
   rag:
     name: docs
     path: ./knowledge-base
     strategy:
       type: chunked-embeddings
       params:
         max_indexing_concurrency: 1
         max_embedding_concurrency: 1
   ```

3. **Use a model with a higher quota.** Some providers offer higher rate limits on
   specific embedding model tiers.

## Debugging RAG

Enable debug logging to see retrieval details:

```bash
$ docker agent run config.yaml --debug --log-file debug.log
```

Look for log tags: `[RAG Manager]`, `[Chunked-Embeddings Strategy]`, `[BM25 Strategy]`, `[RRF Fusion]`, `[Reranker]`.

**Permanent model errors abort early.** If the embedding model, semantic-LLM model, or reranking model returns a permanent error (HTTP 400, 401, 404, or 429 — invalid config, bad auth, unknown model, or rate limit), Docker Agent treats the model configuration as invalid and stops immediately rather than retrying doomed requests:

- **Indexing** — the entire indexing run is aborted after the first permanent failure (including 429). The error is surfaced in the logs so you know immediately if a model name or API key is wrong, rather than silently producing incomplete results.
- **Reranking** — a permanent error (including 429) permanently disables the reranker for the lifetime of the manager. Subsequent queries fall back to un-reranked results. Only transient errors (5xx, timeouts) fall back and retry on the next query.

> [!TIP]
> **Examples**
>
> See the [RAG examples](https://github.com/docker/docker-agent/tree/main/examples/rag) in the GitHub repo for complete, runnable configurations.

## Configuration Reference

### Top-Level RAG Fields

| Field         | Type     | Default | Description                                                    |
| ------------- | -------- | ------- | -------------------------------------------------------------- |
| `docs`        | []string | —       | Document paths/directories (shared across strategies)          |
| `description` | string   | —       | Human-readable description of this RAG source                  |
| `respect_vcs` | boolean  | `true`  | Respect `.gitignore` files when indexing documents             |
| `strategies`  | []object | —       | Array of retrieval strategy configurations                     |
| `results`     | object   | —       | Post-processing: fusion, reranking, deduplication, final limit |

### Chunked-Embeddings Strategy

| Field                       | Type   | Default             | Description                                                  |
| --------------------------- | ------ | ------------------- | ------------------------------------------------------------ |
| `embedding_model`           | string | —                   | **Required.** Embedding model reference                      |
| `database`                  | string | —                   | Path to local SQLite database                                |
| `vector_dimensions`         | int    | —                   | Embedding dimensions (e.g., 1536 for text-embedding-3-small) |
| `similarity_metric`         | string | `cosine_similarity` | Similarity metric                                            |
| `threshold`                 | float  | `0.5`               | Minimum similarity score (0–1)                               |
| `limit`                     | int    | `5`                 | Max results from this strategy                               |
| `embedding_batch_size`      | int    | `50`                | Chunks per embedding request                                 |
| `max_embedding_concurrency` | int    | `3`                 | Max concurrent embedding requests                            |
| `chunking.size`             | int    | `1500`              | Chunk size in characters (`4000` when `code_aware` is set)   |
| `chunking.overlap`          | int    | `75`                | Overlap between chunks in characters                         |
| `chunking.code_aware`       | bool   | `false`             | AST-based chunking (Go files only)                           |

### Semantic-Embeddings Strategy

| Field                      | Type   | Default    | Description                                                        |
| -------------------------- | ------ | ---------- | ------------------------------------------------------------------ |
| `embedding_model`          | string | —          | **Required.** Embedding model reference                            |
| `chat_model`               | string | —          | **Required.** LLM for generating semantic summaries                |
| `vector_dimensions`        | int    | —          | **Required.** Embedding dimensions                                 |
| `database`                 | string | —          | Path to local SQLite database                                      |
| `semantic_prompt`          | string | (built-in) | Custom prompt template (`${path}`, `${content}`, `${ast_context}`) |
| `ast_context`              | bool   | `false`    | Include tree-sitter AST metadata in prompts                        |
| `threshold`                | float  | `0.5`      | Minimum similarity score (0–1)                                     |
| `limit`                    | int    | `5`        | Max results                                                        |
| `max_indexing_concurrency` | int    | `3`        | Max concurrent file indexing                                       |
| `chunking.size`            | int    | `1500`     | Chunk size in characters (`4000` when `code_aware` is set)         |
| `chunking.overlap`         | int    | `75`       | Overlap between chunks                                             |
| `chunking.code_aware`      | bool   | `false`    | AST-based chunking                                                 |

### BM25 Strategy

| Field              | Type   | Default | Description                                     |
| ------------------ | ------ | ------- | ----------------------------------------------- |
| `database`         | string | —       | Path to local SQLite database                   |
| `k1`               | float  | `1.5`   | Term frequency saturation (1.2–2.0 recommended) |
| `b`                | float  | `0.75`  | Length normalization (0–1)                      |
| `threshold`        | float  | `0.0`   | Minimum BM25 score                              |
| `limit`            | int    | `5`     | Max results                                     |
| `chunking.size`    | int    | `1500`  | Chunk size in characters                        |
| `chunking.overlap` | int    | `75`    | Overlap between chunks                          |

### Results (Post-Processing)

| Field                 | Type   | Default | Description                                                 |
| --------------------- | ------ | ------- | ----------------------------------------------------------- |
| `fusion.strategy`     | string | `rrf`   | Fusion method: `rrf`, `weighted`, or `max`                  |
| `fusion.k`            | int    | `60`    | RRF rank constant                                           |
| `deduplicate`         | bool   | `true`  | Remove duplicate results                                    |
| `limit`               | int    | `15`    | Final number of results                                     |
| `include_score`       | bool   | `false` | Include relevance scores in results                         |
| `return_full_content` | bool   | `false` | Return full document content instead of just matched chunks |
| `reranking.model`     | string | —       | Reranking model reference                                   |
| `reranking.top_k`     | int    | (`limit`) | Only rerank top K results. Defaults to the results `limit` when set.  |
| `reranking.threshold` | float  | `0.5`   | Minimum relevance score after reranking                     |
| `reranking.criteria`  | string | —       | Custom relevance guidance for the reranking model           |
