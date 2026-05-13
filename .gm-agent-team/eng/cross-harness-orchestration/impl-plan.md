# Implementation Plan: Cross-Harness Orchestration

**Source PRD:** `prd-v2.md`
**Architecture spec:** `arch-spec.md`
**Branch:** `gm/cross-harness-orchestration`
**Baseline:** builds=true, tests=pre-existing failures in `pkg/config TestCheckRequiredEnvVars` and `pkg/teamloader TestLoadExamples (dmr/unload_on_switch)` — do not fix in this branch.

This plan turns the arch spec into ordered work units. Each unit is sized S/M/L and lists its dependencies, exact files to read for context, exact files to create or modify, and the build/test commands the engineer must run before declaring the unit done.

**Parallel-group invariant:** every unit inside a parallel group touches disjoint files. The "Files modified" lists below are normative — adding a file to a unit means re-checking the parallel group.

---

## Phase 0 — Foundations

Single-engineer, sequential where coupled, parallel where disjoint. Goal: every type and accessor that downstream phases need exists, validates, and is exercised by at least one test. No adapter, no runtime branch yet.

### Unit P0-A — Config snapshot: freeze `pkg/config/v9/`

Description: Copy the current `pkg/config/latest/` tree to `pkg/config/v9/`, change package name to `v9`, keep `Version = "9"`. This is a pure copy; no logic changes. Required before P0-B touches `latest/`.

Complexity: **S**

Files to create:
- `pkg/config/v9/types.go` (copy of `pkg/config/latest/types.go`, `package v9`)
- `pkg/config/v9/validate.go` (copy of `pkg/config/latest/validate.go`, `package v9`)
- Plus every other file in `pkg/config/latest/` (full directory snapshot)

Files to read:
- `pkg/config/latest/` (full directory listing — read each file once during the copy)
- `pkg/config/v8/` (one file, to confirm the snapshot pattern)

Dependencies: none.

Build: `go build ./pkg/config/v9/...`
Test: `go test ./pkg/config/v9/...`

---

### Unit P0-B — Config schema: `HarnessConfig` + validation + version bump

Description: In `pkg/config/latest/`, bump `Version` to `"10"`. Add `HarnessConfig` and `PermissionPolicyConfig` types (arch spec §2.3). Add cross-field validation rules: `Model` and `Harness` mutually exclusive; `Harness` agents have no `SubAgents` or `Handoffs`; `permission_policy.i_understand_the_risk` cross-field rule (FR-7). No filesystem I/O. Wire `pkg/config/upgrade/` (or wherever v9 → v10 conversion lives) as a no-op upgrade for configs without `harness:`.

Complexity: **M**

Files to modify:
- `pkg/config/latest/types.go` — add `HarnessConfig`, `PermissionPolicyConfig`, `Harness *HarnessConfig` field on `AgentConfig`, bump `Version`.
- `pkg/config/latest/validate.go` — add validation block in `Config.Validate` and a new `validateHarness` helper on `AgentConfig`.
- (potentially) `pkg/config/upgrade.go` or `pkg/config/load.go` — add v9 → v10 step. If a generic upgrade chain already handles missing fields, only a version-mapping table entry is needed.

Files to read:
- `pkg/config/latest/types.go` (full file — see existing `AgentConfig`, `FallbackConfig`, `HooksConfig` for the pattern)
- `pkg/config/latest/validate.go` (full file)
- `pkg/config/load.go` or equivalent dispatch (grep for `Version == "9"` to find the upgrade path)
- `prd-v2.md` §6 (config schema reference)
- `arch-spec.md` §2.3 and §2.4

Dependencies: P0-A (the v9 snapshot must exist so the upgrade has a source type).

Build: `go build ./pkg/config/...`
Test: `go test ./pkg/config/...` — add table-driven cases: (1) `model:` only valid, (2) `harness:` only valid, (3) both → error, (4) neither → error, (5) `harness:` with `sub_agents` → error, (6) `harness:` with unknown `type` → error, (7) `i_understand_the_risk: true` without auto_allow → error, (8) v9 file with no `harness:` upgrades cleanly to v10. Also rerun `pkg/teamloader/testdata/*.yaml` parse to confirm existing configs unchanged.

---

### Unit P0-C — Agent harness field and opts

Description: Add `harness *HarnessSpec` field on `*agent.Agent`. Add `HasHarness()` and `Harness()` accessors. Add `WithHarness(spec *HarnessSpec) Opt`. Define `HarnessSpec`, `PermissionPolicy`, `PermissionMode` in `pkg/agent/` (arch spec §2.2).

Complexity: **S**

Files to modify:
- `pkg/agent/agent.go` — add field, two accessors.
- `pkg/agent/opts.go` — add `WithHarness`.

Files to create:
- `pkg/agent/harness_spec.go` — `HarnessSpec`, `PermissionPolicy`, `PermissionMode` types.

Files to read:
- `pkg/agent/agent.go` (full file)
- `pkg/agent/opts.go` (full file) — see `WithModel`, `WithFallbackModel` for the pattern
- `arch-spec.md` §2.2

Dependencies: none (independent of P0-A/B at the file level).

Build: `go build ./pkg/agent/...`
Test: `go test ./pkg/agent/...` — add a unit test that constructs an agent with `WithHarness(&HarnessSpec{AdapterName: "claude-code"})` and asserts `HasHarness() == true`, `Harness().AdapterName == "claude-code"`.

---

### Unit P0-D — Session `HarnessSession` field

Description: Add `HarnessSession map[string]string` field on `*session.Session` (arch spec §2.6). Verify it round-trips through the existing JSON serialization (no schema migration). Add a small lock-aware setter/getter pair if reads happen off the request goroutine; otherwise leave map access bare (matches `AgentModelOverrides`).

Complexity: **S**

Files to modify:
- `pkg/session/session.go` — add field with `json:"harness_session,omitempty"` tag.

Files to read:
- `pkg/session/session.go` (first 200 lines, plus the `Item` marshaling code further down)
- `pkg/session/store.go` if it exists, to confirm JSON serialization path is the only path
- `arch-spec.md` §2.6

Dependencies: none.

Build: `go build ./pkg/session/...`
Test: `go test ./pkg/session/...` — add a JSON round-trip test that confirms an empty map omits, a populated map persists, and a session loaded from disk with no `harness_session` key works.

---

### Unit P0-E — Harness package skeleton

Description: Create `pkg/harness/` with the interfaces, event types, and stub registry from arch spec §3. No adapter implementations. The package compiles, exports every type the runtime and teamloader will need, and ships a zero-adapter unit test (`registry.Lookup` returns "unknown adapter").

Complexity: **M**

Files to create:
- `pkg/harness/harness.go` — `HarnessAdapter`, `HarnessSessionRequest`, `AdapterCapabilities`, `HostRequirements`, `AdapterFeatures`, `ProtocolClass`, `EventSink`, `EventHandler`, `RawEventSink`, `ToolExecutor`, `PermissionRequester`, `PermissionDecision`, `PermissionScope`, `PermissionRequest` (arch spec §3.1–§3.8).
- `pkg/harness/event.go` — `Event` interface, `EventMeta`, the 14 concrete event types and their `isHarnessEvent` markers, JSON Marshal/Unmarshal helpers keyed off a `Kind` field on the wire (arch spec §3.4).
- `pkg/harness/errors.go` — `ErrorCode` typed string and the 13 canonical codes (PRD §4.5 + appendix A).
- `pkg/harness/registry.go` — `Register(name string, factory func() HarnessAdapter)`, `LookupAdapter(name string) (HarnessAdapter, error)`, plus typed-config registration (`RegisterConfig(name string, zero func() any)` and `UnmarshalConfig(name string, raw any) (any, error)`).
- `pkg/harness/fsm.go` — `NewEnforcer(downstream EventSink) EventSink` that validates lifecycle/balance rules (FR-17, FR-18). In dev builds panics; in prod logs and drops.
- `pkg/harness/heartbeat.go` — `NewTicker(ctx, interval, sink, meta) func()` returning a cancel func that emits synthetic `Heartbeat` events (FR-20).
- `pkg/harness/raw.go` — `Source*` constants for `RawEventSink`.

Files to create (subpackages, stubs only):
- `pkg/harness/sandbox/sandbox.go` — `Resolve(root, path string) (string, error)`, `ErrEscape` sentinel, `AllowedEnv()` returning the default allowlist. Implementation per FR-38/41.
- `pkg/harness/sandbox/env.go` — env allowlist + `Filter(env map[string]string) map[string]string`.
- `pkg/harness/sandbox/terminal.go` — `GuardTerminalCommand(cmd string) error` (FR-39 best-effort `cd` check).
- `pkg/harness/fake/adapter.go` — `New(events []harness.Event) harness.HarnessAdapter` for in-process tests.
- `pkg/harness/replay/replay.go` — `PlayFixture(t *testing.T, path string) []harness.Event` (FR-NEW-13).
- `pkg/harness/example/adapter.go` — minimal no-op adapter, the template referenced by §9.1 of the PRD.

Files to read:
- `arch-spec.md` §3 in full
- `prd-v2.md` §4.2, §4.3, appendix A
- `pkg/runtime/event.go` (full file — to understand which fields the runtime translator will need from each canonical event)
- `pkg/agent/agent.go` (`Agent` shape, used by `HarnessSessionRequest.Spec`)
- `pkg/chat/` (just enough to know `chat.Message` shape for `PriorTurns`)

Dependencies: P0-C (for `agent.HarnessSpec`).

Build: `go build ./pkg/harness/...`
Test: `go test ./pkg/harness/...` — must include:
- FSM enforcer rejects duplicate `RunStart`, terminal-after-terminal, unbalanced `Start`/`End` pairs.
- Registry round-trips: `Register("x", factory)` then `LookupAdapter("x")` returns the adapter; unknown name returns error.
- Sandbox: `Resolve("/tmp/root", "/tmp/root/sub/../sub/file")` → `/tmp/root/sub/file`; `Resolve("/tmp/root", "/etc/passwd")` → `ErrEscape`; `Resolve("/tmp/root", "/tmp/root/link")` where link → outside → `ErrEscape`.
- Env filter drops `ANTHROPIC_API_KEY` unless explicitly listed.

---

### Unit P0-F — Teamloader: harness-backed agent construction

Description: In `pkg/teamloader/teamloader.go`, branch on `agentConfig.Harness != nil` (arch spec §2.7). Look up adapter via `harness.LookupAdapter`. Unmarshal `Harness.Config` into the adapter's registered typed struct with `DisallowUnknownField`. PATH-check the binary; surface a clear error on missing binary. Build `*agent.HarnessSpec`, construct `*agent.Agent` with `WithHarness`. Skip model construction and toolset construction for harness agents.

Complexity: **M**

Files to modify:
- `pkg/teamloader/teamloader.go` — add the harness branch around the existing per-agent loop (~line 146).

Files to read:
- `pkg/teamloader/teamloader.go` (first 250 lines — the agent-build loop and helpers)
- `pkg/teamloader/agents.go` or wherever `buildAgent` lives (grep for `agent.New(`)
- `arch-spec.md` §2.7
- `pkg/harness/harness.go` (after P0-E lands)

Dependencies: P0-B (Harness config types), P0-C (agent.HarnessSpec, WithHarness), P0-E (harness.LookupAdapter, sandbox.AllowedEnv).

Build: `go build ./pkg/teamloader/...`
Test: `go test ./pkg/teamloader/...` — add fixture YAML in `pkg/teamloader/testdata/` (e.g., `harness-claude.yaml`) and assert the loaded team has one agent with `HasHarness() == true`. Add a second fixture with `harness.config: { unknown_key: 42 }` and assert the load fails with a message naming the offending key. Add a third with a non-existent binary on PATH and assert the error names the binary plus an install hint.

---

### Unit P0-G — CI prerequisite: surface harness-binary provisioning to platform team

Description: NOT code. File a tracking issue with the platform team requesting CI runner images that include `claude`, `codex`, `opencode`, `copilot`, `openclaw`, plus secrets for `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GITHUB_TOKEN`, plus a per-call cost budget approval. PRD §10 critical-path dependency for Phase 2.

Complexity: **S** (no code).

Files: none (issue tracker).

Dependencies: none.

Output: issue link saved in `.gm-agent-team/eng/cross-harness-orchestration/ci-prerequisites.md`.

---

## Phase 1 — Runtime branch + Claude Code adapter

Single-engineer, mostly sequential. The translator and FSM-wrapped sink land before any adapter so Claude Code immediately exercises the boundary. Hooks/telemetry land alongside the runtime branch.

### Unit P1-A — Runtime translator and `runHarnessForwarding` skeleton

Description: Create `pkg/runtime/harness_delegation.go`. Implement `runHarnessForwarding` and `runHarnessCollecting` per arch spec §2.5 and §4.1. Implement the `translateSink` that converts canonical events into runtime events per FR-21 table. Wire the FSM enforcer in front of the translator. Open the `runtime.harness_session` OTel span. Persist `SessionToken` from `RunEnd` into `parent.HarnessSession[child.Name()]`. Fire `subagent_stop` hook. Return `tools.ResultSuccess`/`tools.ResultError` per the canonical terminal event.

Modify `runForwarding` and `runCollecting` in `pkg/runtime/agent_delegation.go` to branch on `child.HasHarness()`. Rename the existing function bodies to `runModelForwarding` / `runModelCollecting` (no logic change to the model path).

Complexity: **L**

Files to modify:
- `pkg/runtime/agent_delegation.go` — split `runForwarding` / `runCollecting` per arch spec §2.5; rename existing bodies.

Files to create:
- `pkg/runtime/harness_delegation.go` — `runHarnessForwarding`, `runHarnessCollecting`, `translateSink`, `runtimePermissionRequester` (with the team→agent→TUI gate from arch spec §4.4).

Files to read:
- `pkg/runtime/agent_delegation.go` (full file — the existing `runForwarding` is the model the harness path mirrors)
- `pkg/runtime/runtime.go` (lines 1–300 — `LocalRuntime` struct and key methods)
- `pkg/runtime/event.go` (full file — every runtime event the translator emits)
- `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/fsm.go` (after P0-E)
- `pkg/team/team.go` — `Permissions()` (for the team-level gate)
- `arch-spec.md` §2.5, §2.9, §2.10, §4.1, §4.2, §4.4

Dependencies: P0-C, P0-D, P0-E, P0-F.

Build: `go build ./pkg/runtime/...`
Test: `go test ./pkg/runtime/...` — add tests that use `pkg/harness/fake` as the adapter, drive a scripted event sequence through `runHarnessForwarding`, and assert: (1) `StreamStartedEvent` on `RunStart`, (2) `MessageAddedEvent` on each `TextEnd` with the right `Content`, (3) `SubSessionCompletedEvent` + `StreamStoppedEvent` on clean `RunEnd`, (4) `ErrorEvent` with mapped code on `RunError`, (5) parent `HarnessSession[child.Name()]` is set from `RunEnd.SessionToken`. Assert the FSM enforcer rejects a scripted broken sequence.

---

### Unit P1-B — Hooks + telemetry integration

Description: Wire `on_agent_switch` and `subagent_stop` hooks on the harness path (FR-NEW-1). They reuse the runtime's existing hooks executor; the `defer r.executeSubagentStopHooks` call inside `runHarnessForwarding` is added here (or refactored from P1-A if it slipped in). Wire `r.telemetry.RecordHarnessStart` / `Finish` / `Event` (FR-NEW-3) and OTel attributes on the `runtime.harness_session` span (FR-NEW-4). Confirm `pre_tool_use` and `before_llm_call` are NOT fired on the harness path.

Complexity: **M**

Files to modify:
- `pkg/runtime/harness_delegation.go` — add hook calls and telemetry instrumentation.
- `pkg/runtime/telemetry.go` (or wherever `Telemetry` interface lives) — add the three `RecordHarness*` methods to the interface and `defaultTelemetry`.

Files to read:
- `pkg/runtime/telemetry.go` or grep `type Telemetry` to find it
- `pkg/runtime/hooks.go` or grep `executeOnAgentSwitchHooks` to find the hooks dispatcher
- `pkg/runtime/agent_delegation.go` (`executeSubagentStopHooks` usage pattern at the existing `runForwarding`)
- `arch-spec.md` §2.9, §2.10

Dependencies: P1-A.

Build: `go build ./pkg/runtime/...`
Test: `go test ./pkg/runtime/...` — add a fake telemetry recorder, drive a harness session through `runHarnessForwarding`, assert the three telemetry methods fired with the right arguments. Add a fake hooks executor and assert `subagent_stop` fired and `pre_tool_use` did NOT.

---

### Unit P1-C — Claude Code adapter

Description: Implement `pkg/harness/claude/`. Full adapter per PRD §7.1: invocation flags, stream-json NDJSON parser, event mapping table, error mapping table, native multi-turn via `--resume <session-id>`, `Capabilities()` per PRD §7.1. Ship recorded fixtures in `pkg/harness/claude/testdata/` covering: single-message run, multi-tool-call run, mid-stream error, cancellation, multi-turn resume, heartbeat tick.

Complexity: **L**

Files to create:
- `pkg/harness/claude/adapter.go` — `HarnessAdapter` implementation.
- `pkg/harness/claude/parser.go` — NDJSON parser, line → canonical event.
- `pkg/harness/claude/config.go` — typed config struct (`max_turns`, `system_append`, etc.); `init()` registers via `harness.RegisterConfig`.
- `pkg/harness/claude/process.go` — process lifecycle: spawn, stderr-to-log-file, Cancel/SIGTERM/SIGKILL teardown per FR-13, version check per FR-13/§7.1.
- `pkg/harness/claude/testdata/single_message.jsonl`
- `pkg/harness/claude/testdata/multi_tool_call.jsonl`
- `pkg/harness/claude/testdata/error_mid_stream.jsonl`
- `pkg/harness/claude/testdata/cancellation.jsonl`
- `pkg/harness/claude/testdata/resume.jsonl`
- `pkg/harness/claude/testdata/heartbeat.jsonl`
- `pkg/harness/claude/adapter_test.go` — replay each fixture, assert emitted canonical events match the recorded expected sequence.

Also register `claude` adapter in a blank-import block for the binary. Add `pkg/harness/all/all.go` containing only blank imports for each adapter (`_ "github.com/docker/docker-agent/pkg/harness/claude"`), so `cmd/docker-agent/main.go` can import `pkg/harness/all` once.

Files to read:
- `prd-v2.md` §7.1 (full)
- `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/registry.go` (after P0-E)
- `pkg/harness/replay/replay.go` (for the test pattern)
- `arch-spec.md` §3.1, §3.4, §6.2 (binary version drift)

Dependencies: P0-E, P1-A.

Build: `go build ./pkg/harness/claude/...`
Test: `go test ./pkg/harness/claude/...` — no real binary needed; fixtures are the contract. Add a manual integration test gated by `//go:build integration_harness` that spawns real `claude` with a trivial prompt and asserts a clean `RunEnd`.

---

## Phase 2 — Parallel adapter build

Three engineers in parallel. CI runner provisioning (P0-G) must be resolved before integration tests run, but unit/fixture tests in this phase do not require it. Each unit owns its own subdirectory.

### Unit P2-A — Codex adapter

Description: Implement `pkg/harness/codex/` per PRD §7.2. Simulated multi-turn via prompt prepending; FR-19 single-frame `TextStart`/`TextEnd`; FR-25 token-budget warning at 60%, error at 100%.

Complexity: **L**

Files to create:
- `pkg/harness/codex/adapter.go`
- `pkg/harness/codex/parser.go`
- `pkg/harness/codex/config.go` (`model`, `reasoning_effort`, `multi_turn_budget_tokens`)
- `pkg/harness/codex/multiturn.go` (prompt prepending + budget)
- `pkg/harness/codex/process.go`
- `pkg/harness/codex/testdata/*.jsonl` (six fixtures, mirroring Claude Code's set)
- `pkg/harness/codex/adapter_test.go`

Update:
- `pkg/harness/all/all.go` — blank-import `pkg/harness/codex`.

Files to read:
- `prd-v2.md` §7.2
- `pkg/harness/claude/adapter.go` (after P1-C — pattern reference, do not import)
- `arch-spec.md` §3.4

Dependencies: P1-A, P1-C (for the pattern). Disjoint from P2-B, P2-C, P2-D at the file level.

Build: `go build ./pkg/harness/codex/...`
Test: `go test ./pkg/harness/codex/...`

---

### Unit P2-B — OpenCode CLI adapter

Description: Implement `pkg/harness/opencode/` per PRD §7.3. Clone of Codex parser at the wire level. Emit one-time `Warning` event at adapter load when `task_prefix` is used (no per-call system prompt support; FR-NEW-NN at PRD §7.3).

Complexity: **M** (smaller than Codex because the wire mapping is simpler).

Files to create:
- `pkg/harness/opencode/adapter.go`
- `pkg/harness/opencode/parser.go`
- `pkg/harness/opencode/config.go` (`task_prefix`)
- `pkg/harness/opencode/process.go`
- `pkg/harness/opencode/testdata/*.jsonl`
- `pkg/harness/opencode/adapter_test.go`

Update:
- `pkg/harness/all/all.go` — blank-import `pkg/harness/opencode`.

Files to read:
- `prd-v2.md` §7.3
- `pkg/harness/codex/parser.go` (after P2-A — reference; OpenCode wire is close but not identical)

Dependencies: P1-A. Disjoint from P2-A at the file level (`pkg/harness/opencode/` vs `pkg/harness/codex/`). The `pkg/harness/all/all.go` blank-import edits must be sequenced (see "Execution Order" below).

Build: `go build ./pkg/harness/opencode/...`
Test: `go test ./pkg/harness/opencode/...`

---

### Unit P2-C — ACP base + Copilot adapter

Description: Implement `pkg/harness/acp/base.go` (shared ACP client adapter per arch spec §5.3). Implement `pkg/harness/acp/copilot/` on top per PRD §7.4. Includes: `NewClientSideConnection` wiring, ACP method handlers (`ReadTextFile`, `WriteTextFile`, terminal/* via `ToolExecutor`, `RequestPermission` via `PermissionRequester`), session update → canonical event translation, capability negotiation (FR-NEW-8), Cancel → SIGTERM → SIGKILL teardown (FR-13), process pool keyed by `(agent_name, working_dir)` for NFR-11.

Complexity: **L**

Files to create:
- `pkg/harness/acp/base.go` — shared client adapter scaffolding.
- `pkg/harness/acp/capabilities.go` — per-session negotiation.
- `pkg/harness/acp/pool.go` — process pool.
- `pkg/harness/acp/translate.go` — `SessionUpdate` → canonical event.
- `pkg/harness/acp/process.go` — lifecycle + stderr-to-log-file.
- `pkg/harness/acp/copilot/adapter.go` — Copilot-specific: binary, env (`GITHUB_TOKEN`), `Capabilities()`.
- `pkg/harness/acp/copilot/config.go` — `acp_handshake_timeout`.
- `pkg/harness/acp/copilot/testdata/*.jsonl` — recorded ACP frames.
- `pkg/harness/acp/copilot/adapter_test.go`
- `pkg/harness/acp/base_test.go` — translation and lifecycle tests using a fake `ClientSideConnection`.

Update:
- `pkg/harness/all/all.go` — blank-import `pkg/harness/acp/copilot`.

Files to read:
- `prd-v2.md` §7.4
- `pkg/acp/agent.go`, `pkg/acp/run.go` (existing server-side; pattern reference only — do NOT import)
- `~/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.0/client.go` and `client_gen.go` (full)
- `~/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.0/types_gen.go` (skim — find `ReadTextFileRequest`, `WriteTextFileRequest`, `RequestPermissionRequest`, `SessionUpdate*` types)
- `arch-spec.md` §3.7, §3.8, §5.3, §6.1, §6.2

Dependencies: P1-A. Disjoint from P2-A and P2-B at the file level.

Build: `go build ./pkg/harness/acp/...`
Test: `go test ./pkg/harness/acp/...`

---

### Unit P2-D — Sandbox hardening (split from P0-E if needed)

Description: Promote the `pkg/harness/sandbox/` stubs from P0-E into a hardened implementation if P0-E shipped only stubs. Symlink-safe path resolution, `..` rejection, env filtering, terminal `cd`-escape detection (FR-39 best-effort). Add hostile-path test corpus.

Complexity: **M**

Files to modify:
- `pkg/harness/sandbox/sandbox.go`
- `pkg/harness/sandbox/env.go`
- `pkg/harness/sandbox/terminal.go`

Files to create:
- `pkg/harness/sandbox/sandbox_test.go` — hostile fixtures: `..`, absolute outside root, symlink → outside root, symlink chain, mixed cases.

Files to read:
- `pkg/harness/sandbox/*` (after P0-E)
- `arch-spec.md` §3.7, §6.1
- `prd-v2.md` FR-38–FR-41

Dependencies: P0-E.

Build: `go build ./pkg/harness/sandbox/...`
Test: `go test ./pkg/harness/sandbox/...` — must include 1000-iteration fuzz on `Resolve` with random path segments to catch traversal escapes.

Note: P2-D can be done by the engineer least loaded; it's small enough that whoever finishes their adapter first picks it up.

---

## Phase 3 — OpenClaw + hardening

Single engineer, plus a security-focused reviewer for P3-B.

### Unit P3-A — OpenClaw adapter

Description: Implement `pkg/harness/acp/openclaw/` per PRD §7.5. Mostly clone of Copilot adapter with different binary, no `GITHUB_TOKEN`, `IdleTimeout: 2m`. Reuse `pkg/harness/acp/base.go` verbatim.

Complexity: **M** (significantly smaller than Copilot because the base is shared).

Files to create:
- `pkg/harness/acp/openclaw/adapter.go`
- `pkg/harness/acp/openclaw/config.go`
- `pkg/harness/acp/openclaw/testdata/*.jsonl`
- `pkg/harness/acp/openclaw/adapter_test.go`

Update:
- `pkg/harness/all/all.go` — blank-import `pkg/harness/acp/openclaw`.

Files to read:
- `prd-v2.md` §7.5
- `pkg/harness/acp/copilot/adapter.go` (after P2-C — reference)
- `pkg/harness/acp/base.go` (after P2-C)

Dependencies: P2-C.

Build: `go build ./pkg/harness/acp/openclaw/...`
Test: `go test ./pkg/harness/acp/openclaw/...`

---

### Unit P3-B — Conformance suite + 20 scenarios

Description: Build `pkg/harness/conformance/` with the 20 canonical scenarios (PRD §9.4). Every v1 adapter must pass all 20. Scenarios run against fixtures so no real binary needed.

Complexity: **L**

Files to create:
- `pkg/harness/conformance/scenarios.go` — 20 named scenarios as `(name, fixturePath, expectedEvents)` tuples.
- `pkg/harness/conformance/runner.go` — drives an adapter through a fixture and asserts canonical event sequence + FSM compliance + cancellation timing + heartbeat presence.
- `pkg/harness/conformance/scenarios_test.go` — runs each registered adapter through each scenario.

Files to read:
- `prd-v2.md` §9.4
- Each adapter's `testdata/` (after P1-C, P2-A, P2-B, P2-C, P3-A)
- `arch-spec.md` §3.4

Dependencies: P1-C, P2-A, P2-B, P2-C, P3-A.

Build: `go build ./pkg/harness/conformance/...`
Test: `go test ./pkg/harness/conformance/...` — all 5 adapters green across all 20 scenarios.

---

### Unit P3-C — Goleak + process-orphan tests

Description: Verify FR-13, NFR-5, NFR-6 via `goleak` integration tests and a process-orphan counter. 1000 consecutive runs in CI.

Complexity: **M**

Files to create:
- `pkg/harness/lifecycle/lifecycle_test.go` — uses `goleak.VerifyTestMain`. Spawns each adapter 1000 times sequentially with a fake harness binary (a shell script that prints canonical JSON and exits). Asserts no leaked goroutines, no leaked file descriptors, no zombie children.

Files to create (test fixtures):
- `pkg/harness/lifecycle/testdata/fake-claude.sh` — minimal shell script producing valid stream-json.
- One per adapter type.

Files to read:
- Each adapter's `process.go`
- `prd-v2.md` FR-13, NFR-5, NFR-6
- `arch-spec.md` §6.6

Dependencies: P1-C, P2-A, P2-B, P2-C, P3-A.

Build: `go build ./pkg/harness/lifecycle/...`
Test: `go test -run TestLifecycle ./pkg/harness/lifecycle/...` — must run 1000 iterations cleanly.

---

### Unit P3-D — CLI subcommands: `harness describe`, `harness trace`, `harness lint`

Description: Three CLI surfaces from PRD §6.4. `describe <type>` prints `AdapterCapabilities` and accepted `harness.config` schema. `trace <agent>` streams canonical events for the active session in human-readable form. `lint <events.jsonl>` validates a recorded event stream against FSM rules.

Complexity: **M**

Files to create:
- `cmd/docker-agent/cmd_harness.go` — root subcommand.
- `cmd/docker-agent/cmd_harness_describe.go`
- `cmd/docker-agent/cmd_harness_trace.go`
- `cmd/docker-agent/cmd_harness_lint.go`

Files to read:
- `cmd/docker-agent/main.go` (subcommand registration pattern)
- `prd-v2.md` §6.4
- `pkg/harness/harness.go`, `pkg/harness/fsm.go`, `pkg/harness/registry.go`

Dependencies: P3-B (`lint` uses the same FSM logic), P3-A (all adapters registered).

Build: `go build ./cmd/docker-agent/...`
Test: `go test ./cmd/docker-agent/...` — black-box test that `docker-agent harness describe claude-code` prints expected YAML; `harness lint testdata/broken.jsonl` exits non-zero with FSM violation message.

---

## Phase 4 — Dogfood + GA (out of scope for this impl plan, per PRD §10)

PRD §10 Phase 4 is manual: migrate Mark's GM team config, run the JTBD 3 benchmark, write the doc page. No code units here.

---

## Execution Order

### Phase 0, Step 1 (sequential): P0-A — config v9 snapshot
  Files modified: `pkg/config/v9/*` (new directory, full snapshot of `pkg/config/latest/`)
  Files to read: `pkg/config/latest/`, `pkg/config/v8/`
  Build: `go build ./pkg/config/v9/...`
  Test: `go test ./pkg/config/v9/...`

### Phase 0, Step 2 (parallel group A):
  P0-A must be complete. The following touch disjoint files and may run in parallel:

  - **P0-B — config schema + version bump**
    Files modified: `pkg/config/latest/types.go`, `pkg/config/latest/validate.go`, `pkg/config/load.go` (or upgrade dispatcher)
    Files to read: `pkg/config/latest/types.go`, `pkg/config/latest/validate.go`, `pkg/config/load.go`, `prd-v2.md` §6, `arch-spec.md` §2.3 §2.4
    Build: `go build ./pkg/config/...`
    Test: `go test ./pkg/config/...`

  - **P0-C — agent harness field + opts**
    Files modified: `pkg/agent/agent.go`, `pkg/agent/opts.go`
    Files created: `pkg/agent/harness_spec.go`
    Files to read: `pkg/agent/agent.go`, `pkg/agent/opts.go`, `arch-spec.md` §2.2
    Build: `go build ./pkg/agent/...`
    Test: `go test ./pkg/agent/...`

  - **P0-D — session HarnessSession field**
    Files modified: `pkg/session/session.go`
    Files to read: `pkg/session/session.go` (first 200 lines), `arch-spec.md` §2.6
    Build: `go build ./pkg/session/...`
    Test: `go test ./pkg/session/...`

  - **P0-G — CI prerequisite (no code, can run in parallel with anything)**
    Output: `.gm-agent-team/eng/cross-harness-orchestration/ci-prerequisites.md` with platform-team issue link.

### Phase 0, Step 3 (sequential): P0-E — harness package skeleton
  Depends on P0-C (uses `agent.HarnessSpec`).
  Files created: `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/errors.go`, `pkg/harness/registry.go`, `pkg/harness/fsm.go`, `pkg/harness/heartbeat.go`, `pkg/harness/raw.go`, `pkg/harness/sandbox/*`, `pkg/harness/fake/adapter.go`, `pkg/harness/replay/replay.go`, `pkg/harness/example/adapter.go`
  Files to read: `arch-spec.md` §3, `prd-v2.md` §4.2 §4.3 appendix A, `pkg/runtime/event.go`, `pkg/agent/agent.go`, `pkg/chat/`
  Build: `go build ./pkg/harness/...`
  Test: `go test ./pkg/harness/...`

### Phase 0, Step 4 (sequential): P0-F — teamloader harness branch
  Depends on P0-B, P0-C, P0-E.
  Files modified: `pkg/teamloader/teamloader.go`
  Files created: `pkg/teamloader/testdata/harness-claude.yaml`, plus two negative-test fixtures.
  Files to read: `pkg/teamloader/teamloader.go` (first 250 lines), `pkg/teamloader/agents.go` (or equivalent), `arch-spec.md` §2.7
  Build: `go build ./pkg/teamloader/...`
  Test: `go test ./pkg/teamloader/...`

### Phase 1, Step 1 (sequential): P1-A — runtime translator + branch
  Depends on P0-C, P0-D, P0-E, P0-F.
  Files modified: `pkg/runtime/agent_delegation.go`
  Files created: `pkg/runtime/harness_delegation.go`, `pkg/runtime/harness_delegation_test.go`
  Files to read: `pkg/runtime/agent_delegation.go`, `pkg/runtime/runtime.go` (first 300 lines), `pkg/runtime/event.go`, `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/fsm.go`, `pkg/team/team.go`, `arch-spec.md` §2.5 §2.9 §2.10 §4
  Build: `go build ./pkg/runtime/...`
  Test: `go test ./pkg/runtime/...`

### Phase 1, Step 2 (sequential): P1-B — hooks + telemetry
  Depends on P1-A.
  Files modified: `pkg/runtime/harness_delegation.go`, `pkg/runtime/telemetry.go`
  Files to read: `pkg/runtime/telemetry.go`, `pkg/runtime/hooks.go`, `pkg/runtime/agent_delegation.go`, `arch-spec.md` §2.9 §2.10
  Build: `go build ./pkg/runtime/...`
  Test: `go test ./pkg/runtime/...`

### Phase 1, Step 3 (sequential): P1-C — Claude Code adapter
  Depends on P0-E, P1-A. P1-B can be in flight in parallel since it touches different files (P1-B = `pkg/runtime/`, P1-C = `pkg/harness/claude/`).
  Files created: `pkg/harness/claude/*.go`, `pkg/harness/claude/testdata/*.jsonl`, `pkg/harness/all/all.go`
  Files to read: `prd-v2.md` §7.1, `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/registry.go`, `pkg/harness/replay/replay.go`, `arch-spec.md` §3.1 §3.4 §6.2
  Build: `go build ./pkg/harness/claude/...`
  Test: `go test ./pkg/harness/claude/...`

  **NOTE on `pkg/harness/all/all.go`:** This file accumulates blank imports across P1-C, P2-A, P2-B, P2-C, P3-A. To avoid a merge-conflict pinch, P1-C creates the file with the `claude` import. Subsequent adapters append one line each. The unit that lands first wins the initial file; later units rebase. Treat this file as a sequencing point.

### Phase 2 (parallel group B):
  Depends on P1-A, P1-C (P2-A and P2-B reference Claude's adapter as a pattern; P2-C does not but pulls the same scaffolding).

  P2-A, P2-B, P2-C, P2-D touch disjoint subdirectories. They may run in parallel. The only shared file is `pkg/harness/all/all.go`; coordinate via PR rebases (see note under P1-C).

  - **P2-A — Codex adapter**
    Files created: `pkg/harness/codex/*.go`, `pkg/harness/codex/testdata/*.jsonl`
    Files modified: `pkg/harness/all/all.go` (append one line)
    Files to read: `prd-v2.md` §7.2, `pkg/harness/claude/adapter.go` (reference)
    Build: `go build ./pkg/harness/codex/...`
    Test: `go test ./pkg/harness/codex/...`

  - **P2-B — OpenCode CLI adapter**
    Files created: `pkg/harness/opencode/*.go`, `pkg/harness/opencode/testdata/*.jsonl`
    Files modified: `pkg/harness/all/all.go` (append one line)
    Files to read: `prd-v2.md` §7.3, `pkg/harness/codex/parser.go` (reference)
    Build: `go build ./pkg/harness/opencode/...`
    Test: `go test ./pkg/harness/opencode/...`

  - **P2-C — ACP base + Copilot adapter**
    Files created: `pkg/harness/acp/base.go`, `pkg/harness/acp/capabilities.go`, `pkg/harness/acp/pool.go`, `pkg/harness/acp/translate.go`, `pkg/harness/acp/process.go`, `pkg/harness/acp/base_test.go`, `pkg/harness/acp/copilot/*.go`, `pkg/harness/acp/copilot/testdata/*.jsonl`
    Files modified: `pkg/harness/all/all.go` (append one line)
    Files to read: `prd-v2.md` §7.4, `pkg/acp/agent.go`, `pkg/acp/run.go`, `~/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.0/client.go`, `~/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.0/client_gen.go`, `arch-spec.md` §3.7 §3.8 §5.3 §6.1 §6.2
    Build: `go build ./pkg/harness/acp/...`
    Test: `go test ./pkg/harness/acp/...`

  - **P2-D — Sandbox hardening (if P0-E left it as stubs)**
    Files modified: `pkg/harness/sandbox/*.go`
    Files created: `pkg/harness/sandbox/sandbox_test.go`
    Files to read: `pkg/harness/sandbox/*` (current state after P0-E), `arch-spec.md` §3.7 §6.1, `prd-v2.md` FR-38–FR-41
    Build: `go build ./pkg/harness/sandbox/...`
    Test: `go test ./pkg/harness/sandbox/...`

### Phase 3, Step 1 (sequential): P3-A — OpenClaw adapter
  Depends on P2-C.
  Files created: `pkg/harness/acp/openclaw/*.go`, `pkg/harness/acp/openclaw/testdata/*.jsonl`
  Files modified: `pkg/harness/all/all.go` (append one line)
  Files to read: `prd-v2.md` §7.5, `pkg/harness/acp/copilot/adapter.go`, `pkg/harness/acp/base.go`
  Build: `go build ./pkg/harness/acp/openclaw/...`
  Test: `go test ./pkg/harness/acp/openclaw/...`

### Phase 3, Step 2 (parallel group C):
  Depends on P3-A. Three disjoint units may run in parallel:

  - **P3-B — Conformance suite + 20 scenarios**
    Files created: `pkg/harness/conformance/*.go`
    Files to read: `prd-v2.md` §9.4, each adapter's `testdata/`, `arch-spec.md` §3.4
    Build: `go build ./pkg/harness/conformance/...`
    Test: `go test ./pkg/harness/conformance/...`

  - **P3-C — Goleak + process-orphan tests**
    Files created: `pkg/harness/lifecycle/*.go`, `pkg/harness/lifecycle/testdata/fake-*.sh`
    Files to read: each adapter's `process.go`, `prd-v2.md` FR-13 NFR-5 NFR-6, `arch-spec.md` §6.6
    Build: `go build ./pkg/harness/lifecycle/...`
    Test: `go test -run TestLifecycle ./pkg/harness/lifecycle/...`

  - **P3-D — CLI subcommands**
    Files created: `cmd/docker-agent/cmd_harness*.go`
    Files to read: `cmd/docker-agent/main.go`, `prd-v2.md` §6.4
    Build: `go build ./cmd/docker-agent/...`
    Test: `go test ./cmd/docker-agent/...`

---

## Cross-check: parallel-group disjointness

**Phase 0, Step 2 (A):**
- P0-B → `pkg/config/latest/`, `pkg/config/load.go`
- P0-C → `pkg/agent/`
- P0-D → `pkg/session/session.go`
- P0-G → no code
- **DISJOINT.** ✓

**Phase 2 (B):**
- P2-A → `pkg/harness/codex/`, plus appends to `pkg/harness/all/all.go`
- P2-B → `pkg/harness/opencode/`, plus appends to `pkg/harness/all/all.go`
- P2-C → `pkg/harness/acp/` (excluding `openclaw/`), plus appends to `pkg/harness/all/all.go`
- P2-D → `pkg/harness/sandbox/`
- **Subdirectories disjoint.** `pkg/harness/all/all.go` is a sequencing point: append-only single-line edits, resolved by rebase. ✓

**Phase 3, Step 2 (C):**
- P3-B → `pkg/harness/conformance/`
- P3-C → `pkg/harness/lifecycle/`
- P3-D → `cmd/docker-agent/cmd_harness*.go`
- **DISJOINT.** ✓

---

## Total unit count

- Phase 0: 7 units (P0-A through P0-G)
- Phase 1: 3 units (P1-A, P1-B, P1-C)
- Phase 2: 4 units (P2-A through P2-D)
- Phase 3: 4 units (P3-A through P3-D)
- **Total: 18 implementation units**

Estimated calendar with 3 engineers, per PRD §10: 6–7 weeks. Phase 0 = 1 eng-week (sequential dominated by P0-E and P0-F), Phase 1 = 2 eng-weeks, Phase 2 = 2 eng-weeks (parallel), Phase 3 = 1 eng-week. Phase 4 (dogfood) outside this plan.
