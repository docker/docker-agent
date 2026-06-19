# Adaptive RAG Prefetcher Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in adaptive RAG prefetcher that caches exact repeat queries and warms deterministic follow-up candidates when recent RAG topology is stable.

**Architecture:** Implement a small `pkg/rag/prefetch` package with bounded cache, topology tracker, and background scheduler. Wire it into `pkg/rag.Manager` as an optional layer around the existing query pipeline, leaving strategy implementations unchanged.

**Tech Stack:** Go, existing RAG strategy interfaces, existing config/latest schema, existing docs/examples.

## Global Constraints

- Do not add an Aether-Lang dependency.
- Only change latest config, not frozen config versions.
- Feature is opt-in with `results.prefetch.enabled`.
- Background prefetch must be bounded, cancellable, and non-blocking.
- Commits must use DCO sign-off via `git commit -s`.
- Validate with `task lint` and `task test` before PR.

---

### Task 1: Prefetch Package

**Files:**
- Create: `pkg/rag/prefetch/prefetch.go`
- Create: `pkg/rag/prefetch/prefetch_test.go`

**Interfaces:**
- Produces: `Config`, `Prefetcher`, `New(Config) *Prefetcher`
- Produces: `Get(query string) ([]database.SearchResult, bool)`, `Store(query string, results []database.SearchResult)`, `Observe(query string, results []database.SearchResult)`, `Candidates(query string, results []database.SearchResult) []string`, `Prefetch(ctx context.Context, query string, fn FetchFunc)`

- [ ] Write tests for disabled config, bounded cache eviction, stable candidate generation, and drift suppression.
- [ ] Implement config defaults and normalization.
- [ ] Implement topology tracker using query length, term count, result count, and average similarity.
- [ ] Implement candidate generation from query text and top source path basenames.
- [ ] Implement bounded background prefetch with timeout and in-flight de-duplication.
- [ ] Run `go test ./pkg/rag/prefetch`.
- [ ] Commit with `git commit -s -m "feat: add rag prefetch primitives"`.

### Task 2: Manager Integration

**Files:**
- Modify: `pkg/rag/manager.go`
- Modify: `pkg/rag/builder.go`
- Modify: `pkg/rag/manager_test.go`

**Interfaces:**
- Consumes: `prefetch.Config`, `prefetch.Prefetcher`
- Produces: optional manager-level prefetch behavior for `Manager.Query`

- [ ] Add prefetch config to `rag.Config` and create the prefetcher in `New`.
- [ ] Extract current query logic into an unexported `queryUncached(ctx, query string)` helper.
- [ ] Make `Query` check exact cache hits first when enabled.
- [ ] Store successful final results and schedule background candidate prefetches after cache misses.
- [ ] Add integration tests using a fake strategy to prove a second identical query is served from cache.
- [ ] Run `go test ./pkg/rag`.
- [ ] Commit with `git commit -s -m "feat: wire adaptive prefetch into rag manager"`.

### Task 3: Config, Schema, Docs, Example

**Files:**
- Modify: `pkg/config/latest/types.go`
- Modify: `agent-schema.json`
- Modify: `docs/tools/rag/index.md`
- Create: `examples/rag/adaptive_prefetch.yaml`

**Interfaces:**
- Produces: `latest.RAGPrefetchConfig`
- Wires: `latest.RAGResultsConfig.Prefetch *RAGPrefetchConfig`

- [ ] Add `RAGPrefetchConfig` with `enabled`, `max_entries`, `max_candidates`, `min_similarity`, `drift_threshold`, and `timeout`.
- [ ] Add schema definition/properties matching Go JSON tags.
- [ ] Document the feature and its conservative defaults in RAG docs.
- [ ] Add a runnable example using hybrid RAG plus `results.prefetch`.
- [ ] Run `go test ./pkg/config`.
- [ ] Commit with `git commit -s -m "docs: document adaptive rag prefetching"`.

### Task 4: Final Validation and PR

**Files:**
- Modify as needed from validation findings only.

- [ ] Run `task lint`.
- [ ] Run `task test`.
- [ ] Run `task build`.
- [ ] Inspect `git diff --stat main...HEAD`.
- [ ] Push branch to fork remote.
- [ ] Open draft PR against `docker/docker-agent:main` linking issue `#3164`.
