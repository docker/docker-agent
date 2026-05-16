---
title: "RAG (Retrieval-Augmented Generation)"
description: "Give your agents access to document knowledge bases with background indexing, multiple retrieval strategies, and hybrid search."
permalink: /features/rag/
---

# RAG (Retrieval-Augmented Generation)

_Give your agents access to document knowledge bases with background indexing, multiple retrieval strategies, and hybrid search._

## Overview

RAG lets agents search through your documents to find relevant information before responding. docker-agent supports:

- **Background indexing** — Files are indexed automatically and re-indexed on change
- **Multiple strategies** — Semantic embeddings, BM25 keyword search, and LLM-enhanced search
- **Hybrid search** — Combine strategies with result fusion for best results
- **Reranking** — Re-score results with specialized models for improved relevance

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
    batch_size: 50
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

<div class="callout callout-info" markdown="1">
<div class="callout-title">Trade-offs
</div>
  <p>Semantic embeddings provide higher quality retrieval but slower indexing (LLM call per chunk) and additional API costs.</p>

</div>

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

<div class="callout callout-info" markdown="1">
<div class="callout-title">Language Support
</div>
  <p>Currently supports Go (<code>.go</code>) files. More languages will be added. Falls back to plain text chunking for unsupported file types.</p>

</div>

## Debugging RAG

Enable debug logging to see retrieval details:

```bash
$ docker agent run config.yaml --debug --log-file debug.log
```

Look for log tags: `[RAG Manager]`, `[Chunked-Embeddings Strategy]`, `[BM25 Strategy]`, `[RRF Fusion]`, `[Reranker]`.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Examples
</div>
  <p>See the <a href="https://github.com/docker/docker-agent/tree/main/examples/rag">RAG examples</a> in the GitHub repo for complete, runnable configurations.</p>

</div>

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
| `batch_size`                | int    | `50`                | Chunks per embedding request                                 |
| `max_embedding_concurrency` | int    | `3`                 | Max concurrent embedding requests                            |
| `chunking.size`             | int    | `1000`              | Chunk size in characters                                     |
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
| `chunking.size`            | int    | `1000`     | Chunk size in characters                                           |
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
| `chunking.size`    | int    | `1000`  | Chunk size in characters                        |
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
| `reranking.top_k`     | int    | (all)   | Only rerank top K results                                   |
| `reranking.threshold` | float  | `0.5`   | Minimum relevance score after reranking                     |
| `reranking.criteria`  | string | —       | Custom relevance guidance for the reranking model           |
