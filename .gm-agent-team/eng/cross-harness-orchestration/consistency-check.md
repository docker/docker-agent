# Consistency Check: PRD vs Arch Spec vs Impl Plan

**Subject:** Cross-harness orchestration
**Inputs:** `prd-v2.md`, `arch-spec.md`, `impl-plan.md`
**Method:** Per-FR coverage trace, per-unit backing trace, interface diff, dependency graph check, parallel-group file-overlap check, ten specific contract checks (config version, four runtime events, ACP base/profiles, fixtures, hooks).

---

## Verdict: **ISSUES FOUND**

Six gaps. None block Phase 0 kickoff. Three (FR-NEW-5, FR-NEW-10, FR-NEW-11) must be assigned to an existing unit or added as new units before P1-A lands. Two (OpenCode multi-turn, replay/record.go) are file-list omissions inside otherwise correct units. One (sandbox-stub ambiguity between P0-E and P2-D) is a scoping clarification.

The three core contract checks (config version sequencing, four required runtime events, ACP base + per-harness profile ordering) all PASS. Parallel-group file disjointness PASSES with one called-out sequencing point (`pkg/harness/all/all.go`).

---

## 1. Coverage gaps (FRs with no impl-plan unit)

| FR | Requirement | Status | Gap |
|---|---|---|---|
| **FR-NEW-5** | `run_skill` MUST reject harness-backed agents at validation time. | **NOT COVERED** | Arch spec §2.7 says "enforced where `run_skill` resolves its target agent, not in teamloader." No impl-plan unit touches the `run_skill` tool. Belongs in P0-F (teamloader's adjacent file) or a new P0 unit that edits the `run_skill` tool's target resolution. |
| **FR-NEW-10** | `Run` returning a non-nil error MUST be silently converted by the runtime to `ErrorEvent{code: harness_crashed}`. | **NOT EXPLICITLY COVERED** | P1-A description covers `RunError` events and `tools.ResultError` returns, but does not call out the case where `adapter.Run` itself returns a non-nil error (sink unreachable). Arch spec §3.1 documents the contract; impl-plan should make the runtime conversion explicit in P1-A's test list. |
| **FR-NEW-11** | An agent's harness session token is owned by one process at a time. Concurrent reuse of the same session token by two adapter instances MUST be detected and rejected with `RunError{code: protocol_error}`. | **NOT COVERED** | No impl-plan unit. Belongs in P1-A (the runtime is where token lifecycle is observable) or P2-C's `pool.go` if we treat it as a process-pool concern. PRD calls this out as concurrency safety for `@code-reviewer` x N. NFR-11 (NFR-10/-11 in PRD §5.4) intersects but does not subsume it. |
| **FR-25 (OpenCode CLI half)** | Simulated multi-turn MUST emit `Warning` event when prepending exceeds 60%, `RunError{code: context_exhausted}` at 100%. | **PARTIALLY COVERED** | P2-A (Codex) creates `multiturn.go`. P2-B (OpenCode) file list omits a multi-turn module — PRD §7.3 says "Multi-turn: Simulated, same as Codex," but P2-B does not list a `multiturn.go` or its testdata. Either share a `pkg/harness/internal/multiturn/` package (currently absent from arch spec §2.1) or duplicate the file in `pkg/harness/opencode/`. |
| **FR-NEW-13 (record half)** | Each adapter ships fixtures; `pkg/harness/replay/` provides the harness. Arch spec §2.1 lists both `replay.go` and `record.go`. | **PARTIALLY COVERED** | P0-E file list only mentions `pkg/harness/replay/replay.go`. `record.go` (the recording wrapper used during adapter dev) is missing. Without it, adapter authors cannot regenerate fixtures from a real binary run; they must hand-author JSONL, which defeats the point. |
| **FR-NEW-9 (concurrency wiring)** | Parallel fan-out rides on the existing bgAgents handler (`runtime.go:238`). | **IMPLICIT, NOT VERIFIED** | No impl-plan unit asserts the harness path goes through bgAgents. Arch spec §2.5's `runCollecting` branch is implicitly the path, but there is no test in P1-A that drives a parallel two-harness fan-out. JTBD 3 is the user-facing requirement; a missing test here lets the requirement slip silently. |

---

## 2. Orphan units (impl-plan units with no PRD/arch-spec backing)

**None.** Every P0/P1/P2/P3 unit traces to at least one FR or arch-spec section:

- P0-A → FR-6 (config v9 snapshot)
- P0-B → FR-1, FR-2, FR-4, FR-5, FR-6, FR-7, §2.3, §2.4
- P0-C → FR-3, §2.2
- P0-D → FR-26, §2.6
- P0-E → FR-9, FR-10, FR-11, FR-15, FR-16, FR-17, FR-18, FR-20, FR-23, FR-28, FR-38, FR-41, FR-NEW-13 (replay), §3
- P0-F → FR-4, FR-5, FR-8, §2.7
- P0-G → FR-NEW-12
- P1-A → FR-21, FR-26, FR-32, FR-NEW-4 (OTel span), §2.5, §4.1, §4.2, §4.4
- P1-B → FR-NEW-1, FR-NEW-3, §2.9, §2.10
- P1-C → §7.1, FR-13, FR-14
- P2-A → §7.2, FR-19, FR-25
- P2-B → §7.3
- P2-C → §7.4, FR-33, FR-34, FR-NEW-8, NFR-11 (pool), §3.7, §3.8, §5.3
- P2-D → FR-38, FR-39, FR-40, FR-41
- P3-A → §7.5
- P3-B → FR-22, §9.4
- P3-C → FR-13, NFR-5, NFR-6, §6.6
- P3-D → §6.4

---

## 3. Conflicts (contradictions between the three documents)

| # | Topic | PRD | Arch Spec | Impl Plan | Resolution |
|---|---|---|---|---|---|
| 1 | Canonical event count | "12 canonical events" (§4.3) | "14 concrete events" (§3.4) | Inherits §3.4 | **DOCUMENTED RECONCILIATION** in arch-spec §3.4 note. PRD groups `PermissionPending`/`PermissionResolved` as one "Permission" event with two phases; arch-spec splits them for type-switch exhaustiveness. Wire shape identical. Not a true conflict. |
| 2 | `EventSink.Emit` signature | `Emit(Event)` void (appendix A) | `Emit(Event) error` (§3.5) | Inherits §3.5 | **DOCUMENTED RECONCILIATION** in arch-spec §7. PRD appendix A explicitly defers to arch spec ("Final shapes live in the arch spec"). |
| 3 | Event interface methods | One method `isHarnessEvent()` (appendix A) | Four methods (`isHarnessEvent`, `GetSessionID`, `GetAgentName`, `GetTimestamp`) (§3.4) | Inherits §3.4 | **DOCUMENTED RECONCILIATION** in arch-spec §7. Arch spec is authoritative. |
| 4 | Sandbox impl maturity | "Sandbox enforcement (FR-38–41) is a security boundary. Bypass is P0." (NFR-7) | "shared `pkg/harness/sandbox/`, not per-adapter" (§3.7) | P0-E creates `pkg/harness/sandbox/` WITH non-trivial tests (`Resolve`, symlink, env filter); P2-D description says "promote the `pkg/harness/sandbox/` stubs from P0-E into a hardened implementation **if P0-E shipped only stubs**." | **AMBIGUOUS.** Two readings: (a) P0-E ships a real implementation; P2-D is a no-op safety net. (b) P0-E ships stubs; P2-D is the real implementation. The PRD treats sandbox as P0 security. Clarify: P2-D MUST land before any ACP adapter exercises real fs/terminal traffic, regardless of P0-E completeness. Recommend marking P2-D non-optional. |
| 5 | "12-event canonical set" vs "14 in code" naming | PRD §4.3 names 12 | Arch §3.4 ships 14 | Inherits §3.4 | Same as #1. Reconciled. |

No silent substitutions in Go interface signatures. The four arch-spec interfaces (`HarnessAdapter`, `EventSink`, `RawEventSink`, `ToolExecutor`, `PermissionRequester`) all appear verbatim in P0-E's `pkg/harness/harness.go` file list.

---

## 4. Parallel safety (overlapping file paths in parallel groups)

### Phase 0, Step 2 (parallel group A): PASS ✓

| Unit | Files modified |
|---|---|
| P0-B | `pkg/config/latest/types.go`, `pkg/config/latest/validate.go`, `pkg/config/load.go` |
| P0-C | `pkg/agent/agent.go`, `pkg/agent/opts.go`, `pkg/agent/harness_spec.go` (new) |
| P0-D | `pkg/session/session.go` |
| P0-G | none (issue tracker) |

Disjoint. No overlap.

### Phase 2 (parallel group B): SOFT CONFLICT (documented) ⚠

| Unit | Files modified |
|---|---|
| P2-A | `pkg/harness/codex/*` + 1-line append to `pkg/harness/all/all.go` |
| P2-B | `pkg/harness/opencode/*` + 1-line append to `pkg/harness/all/all.go` |
| P2-C | `pkg/harness/acp/*` (excluding `openclaw/`) + 1-line append to `pkg/harness/all/all.go` |
| P2-D | `pkg/harness/sandbox/*` |

Subdirectories disjoint. `pkg/harness/all/all.go` is shared across P2-A, P2-B, P2-C. Impl-plan acknowledges this as a "sequencing point: append-only single-line edits, resolved by rebase." This is technically a parallel-safety violation by strict reading, but the conflict is mechanical (rebase-trivial) and explicitly called out.

**Recommendation:** Move `pkg/harness/all/all.go` creation to P1-C ✓ (already done). Have each Phase 2 unit append to its own per-adapter init file (`pkg/harness/all/claude.go`, `…/codex.go`, etc.) and have `pkg/harness/all/all.go` only contain a build constraint / doc comment. This removes the rebase coupling entirely. Optional, not blocking.

### Phase 3, Step 2 (parallel group C): PASS ✓

| Unit | Files modified |
|---|---|
| P3-B | `pkg/harness/conformance/*` |
| P3-C | `pkg/harness/lifecycle/*` |
| P3-D | `cmd/docker-agent/cmd_harness*.go` |

Disjoint.

---

## 5. Specific contract checks

### 5.1 Config version bump (FR-6)
**PASS.** Arch-spec §2.4 mandates snapshot-then-bump. Impl-plan P0-A (snapshot v9, sequential) precedes P0-B (bump latest to v10, in parallel group). P0-B's test list includes "v9 file with no `harness:` upgrades cleanly to v10." Ordering correct.

### 5.2 Four required runtime events (FR-21)
**PASS.** Arch-spec §4.1 traces each of `StreamStartedEvent` (from `RunStart`), `MessageAddedEvent` (from `TextEnd`), `SubSessionCompletedEvent` (from `RunEnd`), `StreamStoppedEvent` (from `RunEnd`/`RunError`). Impl-plan P1-A explicitly cites FR-21 and lists test cases for all four runtime events with the expected canonical-event triggers. Translator lives in `pkg/runtime/harness_delegation.go` (`translateSink`), correctly in the runtime phase, after `pkg/harness/` skeleton (P0-E) and before any adapter.

### 5.3 ACP client adapter (base + Copilot + OpenClaw)
**PASS.** Arch-spec §2.1 layout shows `pkg/harness/acp/{base.go, capabilities.go, pool.go, copilot/, openclaw/}`. Impl-plan P2-C creates the base AND Copilot together (single unit, correct because Copilot is the first consumer that proves the base). P3-A creates OpenClaw, dependent on P2-C. Sequencing: `base → copilot → openclaw`. Arch-spec §5.3 explicitly distinguishes `pkg/harness/acp/` (client) from `pkg/acp/` (existing server); impl-plan P2-C reads `pkg/acp/agent.go` as "pattern reference only — do NOT import." Correct.

### 5.4 Record/replay fixtures per adapter (FR-NEW-13)
**PARTIAL.** Each adapter unit (P1-C, P2-A, P2-B, P2-C, P3-A) ships `testdata/*.jsonl`. ✓ However, `pkg/harness/replay/record.go` (the recording wrapper, arch-spec §2.1) is missing from P0-E's file list. See Gap #5 above.

### 5.5 Hooks integration (FR-NEW-1)
**PASS.** P1-B explicitly covers `on_agent_switch`, `subagent_stop` (fire) and `pre_tool_use`, `before_llm_call` (must NOT fire). Test list includes a fake hooks executor asserting `subagent_stop` fired and `pre_tool_use` did NOT. Correctly sequenced after P1-A (runtime branch) so the hook integration is wired into the harness path the moment that path exists.

### 5.6 Dependency graph
**PASS** with one note.

- P0-A → no deps (correct, pure copy)
- P0-B → P0-A ✓
- P0-C → no deps ✓ (parallel with P0-B/-D)
- P0-D → no deps ✓
- P0-E → P0-C ✓ (uses `agent.HarnessSpec`)
- P0-F → P0-B, P0-C, P0-E ✓
- P1-A → P0-C, P0-D, P0-E, P0-F ✓ (uses `agent`, `session`, `harness`, `teamloader`)
- P1-B → P1-A ✓ (modifies the file P1-A creates)
- P1-C → P0-E, P1-A ✓ (note in impl-plan that P1-B can run in parallel with P1-C is correct; different files)
- P2-A → P1-A, P1-C ✓
- P2-B → P1-A (impl-plan says "uses P2-A as reference"; the dependency is documentation-only, not type-level) ✓
- P2-C → P1-A ✓
- P2-D → P0-E ✓
- P3-A → P2-C ✓ (uses the ACP base)
- P3-B → P1-C, P2-A, P2-B, P2-C, P3-A ✓ (conformance runs all adapters)
- P3-C → P1-C, P2-A, P2-B, P2-C, P3-A ✓
- P3-D → P3-A (all adapters registered) ✓; P3-B (lint uses FSM logic) ✓

**Note:** P3-D's dependency on P3-B is via the FSM linter logic, which is actually in `pkg/harness/fsm.go` (P0-E), not in conformance (P3-B). The dependency could be downgraded to P0-E. Minor.

### 5.7 Interface consistency (no silent substitutions)
**PASS.** Each Go interface in arch-spec §3 maps cleanly to a file in P0-E:

| Arch spec interface | P0-E file | Notes |
|---|---|---|
| `HarnessAdapter` (§3.1) | `pkg/harness/harness.go` | ✓ |
| `AdapterCapabilities`, `HostRequirements`, `AdapterFeatures` (§3.2) | `pkg/harness/harness.go` | ✓ |
| `HarnessSessionRequest` (§3.3) | `pkg/harness/harness.go` | ✓ |
| `Event` + 14 concrete types + `EventMeta` (§3.4) | `pkg/harness/event.go` | ✓ |
| `EventSink`, `EventHandler` (§3.5) | `pkg/harness/harness.go` | ✓ |
| `RawEventSink` (§3.6) | `pkg/harness/harness.go` + `pkg/harness/raw.go` | ✓ |
| `ToolExecutor` (§3.7) | `pkg/harness/harness.go` | ✓ |
| `PermissionRequester` (§3.8) | `pkg/harness/harness.go` | ✓ |
| `HarnessSpec`, `PermissionPolicy`, `PermissionMode` (§2.2) | P0-C → `pkg/agent/harness_spec.go` | ✓ correctly placed in `pkg/agent/` not `pkg/harness/` per arch-spec dependency direction |
| `HarnessConfig`, `PermissionPolicyConfig` (§2.3) | P0-B → `pkg/config/latest/types.go` | ✓ |

No interface is silently renamed or restructured between arch-spec and impl-plan.

---

## 6. Required fixes (before P1-A lands)

Priority ordered. Each is small.

### Fix 1 (BLOCKER for P1-A) — Cover FR-NEW-10 in P1-A test list
Add a test case to P1-A: "adapter `Run` returns non-nil error → runtime emits `ErrorEvent{code: harness_crashed}` and never propagates the error to the orchestrator loop." Use the fake adapter with a forced sink-failure mode.

**Effort:** 1 line in the impl-plan; 1 test case (~30 LOC).

### Fix 2 (BLOCKER for P1-A) — Add FR-NEW-11 to P1-A or a new P1 unit
Detect concurrent reuse of the same `parent.HarnessSession[child.Name()]` token by two adapter instances. Reject the second use with `RunError{code: protocol_error}`. Most natural location: a small `tokenLockMap` on the runtime, set on `runHarnessForwarding` entry, cleared on `RunEnd`/`RunError`. Test: spawn two `@code-reviewer` instances concurrently with the same parent session, assert one succeeds and one fails with the documented error.

**Effort:** ~40 LOC + 1 test. Add as P1-A scope extension or a new P1-D unit gated before P2.

### Fix 3 (BLOCKER for FR-NEW-5 enforcement) — Add a unit for `run_skill` rejection
Either:
- Extend P0-F to also edit `pkg/runtime/loop.go` (or wherever `run_skill` resolves its target) to reject harness-backed targets at validation time. Add the test.
- Or add a new P0-H unit specifically for this.

PRD treats this as a hard rule. Without enforcement, `run_skill` would silently pass a skill system prompt to a harness with no place to land.

**Effort:** ~20 LOC + 1 test.

### Fix 4 (BLOCKER for OpenCode multi-turn) — Add multi-turn module to P2-B
Either:
- Lift Codex's `multiturn.go` into a shared `pkg/harness/internal/multiturn/` package (extend arch-spec §2.1).
- Or list `pkg/harness/opencode/multiturn.go` in P2-B's file-creation list, mirroring Codex.

Per PRD §7.3, OpenCode CLI uses simulated multi-turn "same as Codex" with the same 60%/100% budget rules.

**Effort:** ~30 LOC + 1 test, OR a 5-line refactor to share.

### Fix 5 (BLOCKER for adapter author productivity) — Add `pkg/harness/replay/record.go` to P0-E
Without it, adapter authors hand-author fixtures from scratch. Arch-spec §2.1 lists this file; P0-E omits it. Adds ~80 LOC for a recording wrapper that intercepts adapter output and writes JSONL.

**Effort:** 1 line in impl-plan; ~80 LOC in P0-E.

### Fix 6 (NICE-TO-HAVE) — Resolve sandbox stub vs hardened ambiguity
Clarify P0-E's deliverable: real implementation (matching its non-trivial test list) or stubs. If real, downgrade P2-D to a hardening/fuzz-only pass. If stubs, expand P2-D scope explicitly. Either way, P2-D MUST land before P2-C ships to any real ACP traffic. Currently P2-D is in the same parallel group as P2-C — that's fine for code, but the deploy ordering deserves a callout.

**Effort:** 1 paragraph in impl-plan.

### Fix 7 (NICE-TO-HAVE) — Add bgAgents wiring test to P1-A
FR-NEW-9 says concurrency rides on the existing bgAgents handler. Add to P1-A's test list: drive two harness subagents in parallel from one orchestrator turn (JTBD 3 scenario, fake adapters) and assert no event interleaving across `SessionID`s.

**Effort:** 1 test (~40 LOC).

### Fix 8 (POLISH) — Decouple `pkg/harness/all/all.go` from Phase 2 rebase
Move blank imports from a single `all.go` to per-adapter init files (`pkg/harness/all/claude.go`, `…/codex.go`, …). Removes the only file-overlap in Phase 2.

**Effort:** 5 minutes.

---

## Summary

- **Coverage:** 3 hard gaps (FR-NEW-5, FR-NEW-10, FR-NEW-11), 2 soft gaps (FR-25 OpenCode half, FR-NEW-13 record.go), 1 implicit (FR-NEW-9).
- **Orphans:** None.
- **Conflicts:** None unresolved. All PRD/arch-spec divergences are documented in arch-spec §3.4 and §7.
- **Parallel safety:** Phase 0 group A and Phase 3 group C disjoint. Phase 2 group B has one rebase-coupled file (`pkg/harness/all/all.go`), acknowledged.
- **Three core contracts** (config version sequencing, four required runtime events, ACP base+profiles sequencing): all PASS.

Fix the 5 blockers above (estimated total: ~200 LOC, half a day) and re-run this check. Phase 0 can start in parallel with the fixes since none of the gaps touch P0-A through P0-E's file lists.
