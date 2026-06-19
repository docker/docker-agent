# Adaptive RAG Prefetcher Design

## Context

Issue: https://github.com/docker/docker-agent/issues/3164

Aether-Lang contains useful algorithms for sparse attention graphs, hierarchical block metadata, adaptive epsilon thresholds, and centroid drift detection. docker-agent should not import Aether-Lang or add Rust/runtime dependencies. The contribution should translate the useful ideas into small Go primitives that fit the existing RAG manager and strategy interfaces.

## Goal

Add an opt-in adaptive RAG prefetcher that reduces repeated retrieval latency and warms likely follow-up queries without blocking the active user turn.

## Non-Goals

- No Aether-Lang dependency.
- No new DSL, kernel, or model provider.
- No replacement of existing RAG strategies, fusion, or reranking.
- No hidden behavior when config does not enable the feature.

## Design

The RAG manager gets an optional prefetcher configured under `results.prefetch`. When disabled, `Manager.Query` keeps current behavior.

When enabled:

1. `Manager.Query` checks a small in-memory cache keyed by normalized query text.
2. Cache hit returns the final post-processed results from the earlier query.
3. Cache miss runs the normal strategy/fusion/reranking pipeline.
4. The prefetcher observes the query and result metadata.
5. If topology is stable enough, it schedules a bounded background prefetch for one or more deterministic follow-up candidates.

The prefetcher computes a lightweight state vector from query/result metadata:

- query length
- token count
- result count
- average similarity

It tracks a centroid and drift score over recent observations. A simple adaptive threshold accepts stable sessions and suppresses prefetch during strong topic shifts.

## Configuration

Add to the latest config only:

```yaml
results:
  prefetch:
    enabled: true
    max_entries: 32
    max_candidates: 2
    min_similarity: 0.5
    drift_threshold: 0.8
    timeout: 10s
```

Defaults are conservative. `enabled` defaults false.

## Components

- `pkg/rag/prefetch`: owns cache, topology tracker, candidate generation, and background scheduling.
- `pkg/rag/manager.go`: wires cache lookup, observation, and background scheduling around existing query behavior.
- `pkg/config/latest/types.go`: latest-only config structs.
- `agent-schema.json`: schema sync for config fields.
- `docs/tools/rag/index.md`: user-facing docs.
- `examples/rag/adaptive_prefetch.yaml`: runnable example config.

## Safety

- Background prefetch uses a timeout and a max in-flight limit.
- Prefetch errors are logged at debug level and never fail the active query.
- Results cache is bounded and evicts oldest entries.
- Query work is skipped while drift exceeds threshold.
- Prefetch stores only normal RAG search results already available to the running process.

## Testing

- Unit tests for cache eviction, candidate generation, drift suppression, and scheduling.
- Manager integration test for cache hit avoiding a second strategy call.
- Config/schema test coverage through existing schema sync tests.
