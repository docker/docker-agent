# PRD: Cross-Harness Orchestration

**Owner:** docker-agent eng
**Status:** APPROVED FOR ENGINEERING
**Target:** v1 ships 5 harnesses (Claude Code, Codex, OpenCode CLI, Copilot CLI via ACP, OpenClaw via ACP). Cursor + OpenCode SSE deferred.
**Insertion point:** `pkg/runtime/agent_delegation.go` — specifically `runForwarding` (line 248) and `runCollecting` (line 310). See §1 for the full file list.

---

## 1. Problem statement and insertion point

### 1.1 Problem

docker-agent today is a Go CLI agent framework where every agent in a team is backed by a **model** — a raw LLM API call wrapped in docker-agent's own agent loop (tool calling, planning, session memory, TUI).

Model providers now ship their own native **harnesses** — Claude Code CLI, Codex CLI, OpenCode, Copilot CLI, OpenClaw — that bundle a model with provider-tuned prompts, tool sets, safety policies, and context strategies. For coding work, a vendor harness usually outperforms a generic model call because the vendor has tuned the harness to its own model's strengths.

Mark Cavage runs a GM pattern: one orchestrator delegates to specialist subagents. He wants the same pattern with the parent able to dispatch to harnesses instead of raw models. The orchestrator should send a coding task to Claude Code CLI, a separate task to Codex, get structured results back, and continue the conversation — all inside docker-agent's existing TUI, session model, and team config.

The pain:

- **No way to use a vendor harness as a subagent today.** Run docker-agent (lose Claude Code's tuning) or run Claude Code directly (lose docker-agent's orchestration, TUI, team config).
- **Manual harness juggling.** Running Claude Code in one terminal, Codex in another, copy-pasting outputs does not scale and does not preserve context.
- **Multi-model coding workflows are stuck.** Picking the right harness per task requires an orchestrator that can route. docker-agent is the natural home.

Why now: ACP (Agent Client Protocol) just gave us a stable bidirectional protocol for Copilot and OpenClaw. `github.com/coder/acp-go-sdk@v0.13.0` is already in go.mod — we ship `docker-agent serve acp` today, so the wire format is proven. Self-contained harnesses (Claude Code, Codex, OpenCode) ship stable streaming JSON. Technical risk is low enough to commit.

### 1.2 Insertion point — files touched

The branch point is not a file, it is two functions inside `pkg/runtime/agent_delegation.go`. Full file list:

1. **`pkg/runtime/agent_delegation.go`**
   - `(*LocalRuntime).runForwarding` (line 248) — split into two branches. If `child.HasHarness()`, call a new `runHarnessForwarding(ctx, parent, evts, child, req)` that builds a `HarnessSessionRequest`, drives the adapter, and emits canonical-mapped runtime events. Else the existing model-loop path.
   - `(*LocalRuntime).runCollecting` (line 310) — same split. Background agents (`RunAgent`) go through this. Harness-backed agents ARE allowed as background agents (JTBD 3 parallel benchmark, JTBD 4 long-running session both require it).
   - Either branch must emit the parent-visible events `AgentSwitching`, `SubSessionCompleted`, and a `tools.ToolCallResult` return for the orchestrator's tool-call slot.

2. **`pkg/agent/agent.go`** — add `harness *HarnessSpec` field (opaque to runtime, consumed by adapter layer) and `HasHarness() bool` method on `*Agent`. Mirrors `Model() / HasModelOverride()`.

3. **`pkg/agent/opts.go`** — add `WithHarness(spec *HarnessSpec) Opt`. Mirrors `WithModel`.

4. **`pkg/teamloader/teamloader.go`** — build `*Agent` with a harness when config carries one; skip model resolution. Perform the PATH check for the harness binary here (filesystem I/O lives in teamloader, not in `pkg/config/latest/validate.go`).

5. **`pkg/config/latest/types.go`** — add `HarnessConfig` struct on `AgentConfig`. Same pattern as `Fallback *FallbackConfig`, `Hooks *HooksConfig`.

6. **`pkg/config/latest/validate.go`** — schema validation rules per §2. No filesystem I/O.

7. **`pkg/config/v9/`** (new) — freeze the current `pkg/config/latest/` snapshot before bumping `Version` to `"10"`. v9→v10 upgrade is a no-op for configs without `harness:`.

8. **`pkg/session/session.go`** — add `Session.HarnessSession map[string]string` field. Keyed by agent name; value is the adapter-opaque resume token. Serialized via existing `messages` JSON; no schema migration.

9. **`pkg/harness/`** (new package) with the layout:
   ```
   pkg/harness/
     harness.go            // HarnessAdapter interface, Event interface, HarnessSessionRequest
     registry.go           // registry by type
     translate.go          // harness.Event → runtime.Event (Option B from §4)
     sandbox/              // path resolution, env allowlist, terminal guard (FR-29–32)
     example/              // template adapter for new authors (see §9)
     fake/                 // in-process fake adapter for tests
     replay/               // record/replay fixture infrastructure (FR-NEW-13)
     claude/               // adapter
     codex/                // adapter
     opencode/             // adapter
     acp/                  // ACP client adapter base (shared by copilot, openclaw)
       copilot/
       openclaw/
   ```
   The runtime imports `pkg/harness`. It does NOT do blank-import registration of adapter subpackages.

10. **`pkg/runtime/loop.go`** — no change to `registerDefaultTools`. `transfer_task` dispatches by agent name; the harness branch is downstream in `runForwarding`. v1 piggybacks on `transfer_task` to avoid a new top-level tool.

---

## 2. Goals and non-goals

### Goals (v1)

1. Declare a harness-backed subagent in team YAML and have an orchestrator delegate to it.
2. Ship 5 adapters: Claude Code CLI, Codex CLI, OpenCode CLI, Copilot CLI (ACP), OpenClaw (ACP).
3. Normalize every harness to a 12-event canonical event set (AG-UI vocabulary).
4. Multi-turn sessions: a harness subagent can be invoked, return, and be invoked again with prior context preserved.
5. Surface ACP permission prompts in docker-agent TUI and route responses back.
6. Sandbox ACP `terminal/*` and filesystem operations to the session's working directory.
7. Make adapter capabilities introspectable (`AdapterCapabilities`).
8. Adapter authors can build, test, and debug a new adapter without the real harness binary on their machine (record/replay, fake adapter, conformance suite).

### Non-goals (v1)

1. **Replacing the model-backed runtime.** Harness-backed agents are additive.
2. **Harness-as-orchestrator.** Only model-backed agents orchestrate. Harnesses are subagents only.
3. **Custom tool injection into harnesses.** Self-contained harnesses run their own tools.
4. **Cursor adapter.** NDJSON schema not stable. v1.1 if it stabilizes.
5. **OpenCode SSE transport.** v1.1 (needed for per-call system prompts).
6. **Sub-harness delegation.** Harness subagents cannot spawn harness subagents.
7. **AG-UI wire format compatibility.** Borrow vocabulary, skip wire format.
8. **Cost/usage aggregation across harnesses.** Raw per-harness only. v1.1.

---

## 3. User stories (JTBD)

**JTBD 1 — Route to best harness per task.** Multi-part refactor: orchestrator sends algorithmic core to Claude Code, test scaffolding to Codex, config tweak to Copilot.

**JTBD 2 — Subagent specialization.** `@code-reviewer` is Claude Code-backed, `@prototype-builder` is Codex-backed; existing orchestrator routing works unchanged.

**JTBD 3 — Compare two harnesses on the same task.** Dispatch the same task to two harness subagents in parallel from one orchestrator turn.

**JTBD 4 — Long-running harness session with checkpointing.** 90-second Claude Code refactor: streamed text, tool calls, summary in TUI in real time; session persisted; resumable.

**JTBD 5 — ACP harness with permission prompts.** Copilot wants to write outside the working directory: permission prompt surfaces in docker-agent TUI with the same UX as model-backed prompts.

---

## 4. Functional requirements

Numbered. Every requirement is testable.

### 4.1 Config schema

**FR-1.** Team YAML MUST allow declaring a subagent with `harness:` instead of `model:`. The two are mutually exclusive. Validation MUST reject configs that set both or neither.

**FR-2.** The `harness:` field MUST be a struct with: `type` (enum: `claude-code` | `codex` | `opencode` | `copilot` | `openclaw`), and optional `command`, `args`, `env`, `working_dir`, `timeout`, `permission_policy`, `config` (adapter-specific knobs).

**FR-3.** `agent.HasHarness()` MUST return true iff `harness:` is set. It is the branch primitive in `runForwarding`/`runCollecting`.

**FR-4.** Schema-level validation in `pkg/config/latest/validate.go` MUST reject malformed configs (unknown `type` enum, both/neither model+harness, missing required nested fields). Schema validation MUST NOT touch the filesystem. Binary PATH lookup MUST happen in `pkg/teamloader/teamloader.go` at team-load time, and MUST emit a clear error naming the missing binary and an install hint.

**FR-5.** `harness.config` is accepted as an opaque `map[string]any` at the schema level (in `validate.go`). Each adapter MUST register a typed config struct at init time; the teamloader MUST unmarshal `harness.config` into the adapter's typed struct with `yaml.DisallowUnknownField` and surface unknown keys as a load-time error (not runtime). The teamloader MUST also reject `harness:` agents that have non-empty `sub_agents` or `handoffs` (harness-as-orchestrator gate, see FR-NEW-7 lineage).

**FR-6.** Config version MUST bump from `"9"` to `"10"`. `pkg/config/v9/` MUST be frozen as a snapshot of the pre-harness `latest` package before the bump. The v9→v10 upgrade is a no-op for configs without `harness:`.

**FR-7.** `permission_policy.i_understand_the_risk: true` with no nested `auto_allow` or `allow_unrestricted` MUST be a validation error ("you acknowledged a risk you didn't take"). Same for the inverse (`auto_allow` without `i_understand_the_risk`).

**FR-8.** Working-dir resolution: `harness.working_dir` ?? `session.WorkingDir` ?? `os.Getwd()`. Resolved at team-load time. Reuses the path-expansion pattern from `Toolset.AllowList` resolution.

### 4.2 Adapter interface

**FR-9.** Every adapter MUST implement:

```go
type HarnessAdapter interface {
    Name() string
    Capabilities() AdapterCapabilities
    Run(ctx context.Context, req HarnessSessionRequest) error
}
```

`HarnessSessionRequest` replaces `SubSessionRequest`. Naming is consistent with `runHarnessForwarding`.

**FR-10.** `Capabilities()` MUST be a pure function (no I/O, no process spawn). Returns:

```go
type AdapterCapabilities struct {
    Protocol     ProtocolClass    // ProtocolStream | ProtocolACP
    Requires     HostRequirements // binary name, min version, env vars
    Features     AdapterFeatures  // supports_multi_turn, supports_per_call_system_prompt, streams_text_deltas, streams_reasoning
    BuiltInTools []string         // informational
    IdleTimeout  time.Duration    // process-pool idle timeout, per-adapter
}
```

`ProtocolClass` is a typed constant (`ProtocolStream`, `ProtocolACP`), not a raw string.

`AdapterCapabilities()` returns the adapter's **static** support surface — what it will use if available. For ACP adapters, per-session capability negotiation happens inside `Run` and may downgrade actual session behavior (FR-NEW-8). The split is intentional and documented.

**FR-11.** `Run` MUST emit events through the `EventSink` supplied in `HarnessSessionRequest` and MUST NOT panic on the caller's goroutine. All harness-runtime errors MUST be surfaced as `RunError` events. `Run` returns `nil` on clean shutdown. A non-nil return is reserved for adapter-internal bugs where the sink is unreachable.

**FR-12.** Adapters MUST be process-per-session. Multiple concurrent subagents of the same type MUST run in independent processes.

**FR-13.** Adapters MUST clean up child processes on context cancellation. ACP adapters MUST first call `conn.Cancel(ctx, params)` (polite cancellation per ACP SDK), then SIGTERM, wait 5s, then SIGKILL. Non-ACP adapters: SIGTERM → wait 5s → SIGKILL. A test MUST verify no orphan processes.

**FR-14.** Adapters MUST forward child-process stderr to a per-session log file at `${XDG_STATE_HOME:-~/.local/state}/docker-agent/sessions/<session-id>/harness-<n>.stderr`. Stderr MUST NOT be parsed for events.

### 4.3 Canonical event set

**FR-15.** Canonical events are a public type set in `pkg/harness`. Events are a **discriminated union**: `Event` is an interface with one concrete struct per kind. The runtime translator (`pkg/harness/translate.go`) converts each `harness.Event` to the matching `runtime.Event` at the boundary (Option B per arch review §4).

The 12 canonical events:

```
Lifecycle:    RunStart, RunEnd, RunError
Text:         TextStart, TextDelta, TextEnd
Reasoning:    ReasoningStart, ReasoningDelta, ReasoningEnd
Tool:         ToolCallStart, ToolCallEnd
Permission:   PermissionPending, PermissionResolved
Liveness:     Heartbeat
```

Total: 12 canonical events. Naming is `Start/End` consistently (not `Started/Finished`).

`HarnessRaw` is NOT in the canonical set. Raw harness frames flow through a separate opt-in `RawEventSink` interface (FR-23).

**FR-16.** Every event MUST carry: `SessionID` (sub-agent session, for fan-out attribution), `AgentName`, `Timestamp`, and a kind-specific `MessageID` or `CallID` where applicable. The translator stamps `SessionScoped` (via `Session.ID`) and `AgentContext` (via `child.Name()` + `r.now()`) so every event satisfies the existing `pkg/runtime/event.go` interfaces.

**FR-17.** Every session MUST emit exactly one `RunStart` and exactly one terminal event (`RunEnd` or `RunError`). The runtime FSM enforcer wraps `EventSink` and rejects: duplicate `RunStart`, terminal after terminal, `Start` without matching `End`, `End` without matching `Start`, `Heartbeat` after terminal. Violation panics in dev builds and logs+drops in prod.

**FR-18.** `Text*`, `Reasoning*`, and `ToolCall*` events MUST be balanced by message/call ID. Enforced by FR-17's FSM wrapper.

**FR-19.** Codex adapter MUST NOT emit `TextDelta` (Codex does not stream text). It sets `Features.StreamsTextDeltas = false` and emits a single `TextStart` immediately followed by a `TextEnd` carrying the full text in `Content`. The FSM enforcer permits this pattern.

**FR-20.** Adapters MUST emit `Heartbeat` at least every 30 seconds during an active run (between `RunStart` and a terminal event). The TUI uses `Heartbeat` to distinguish "thinking" from "hung" for long-running sessions (JTBD 4: 90-second refactor). Adapters that have a natural keepalive (ACP `session/update` ticks) may piggyback on it; otherwise emit synthetically on a timer.

**FR-21.** The harness path MUST emit exactly these four runtime events when translating canonical events at the boundary:

| Runtime event | Triggered by |
|---|---|
| `StreamStartedEvent` | first `RunStart` |
| `MessageAddedEvent` | each `TextEnd` (persists assistant message to session) |
| `SubSessionCompletedEvent` | clean `RunEnd` (mirrors model-backed `runForwarding` line 295) |
| `StreamStoppedEvent` | `RunEnd` or `RunError` (preserves TUI streamDepth balance) |

Additional runtime events (`AgentChoiceEvent`, `AgentChoiceReasoningEvent`, `ToolCallEvent`, `ToolCallResponseEvent`, `ToolCallConfirmationEvent`, `AuthorizationEvent`, `ErrorEvent`, `WarningEvent`, `TokenUsageEvent`) are emitted as canonical events translate naturally.

**FR-22.** Orchestrator MUST consume the event stream without knowing which harness produced it. A conformance test MUST replay a recorded canonical event stream through the orchestrator and assert identical behavior for each adapter.

**FR-23.** Adapters that emit raw frames MUST do so via a separate `RawEventSink` interface, opt-in per session:

```go
type RawEventSink interface {
    EmitRaw(adapter string, frame []byte)
}
```

`RawSink` on `HarnessSessionRequest` is nil unless the consumer wired it up. Raw frames carry a `Source` field naming the wire format (`"opencode-line"`, `"acp-update"`, `"claude-stream-json"`).

### 4.4 Session continuity (multi-turn)

**FR-24.** Adapters whose `Features.SupportsMultiTurn = true` MUST accept `HarnessSessionRequest.SessionToken` (opaque to docker-agent, returned from a prior `RunEnd`) and use it to resume.

**FR-25.** For harnesses without native multi-turn (Codex, OpenCode CLI), the adapter MUST simulate multi-turn by prepending prior turns to the prompt up to a configurable token budget (default 50% of harness context window). Exceeding the budget MUST emit `RunError{code: context_exhausted}`. The adapter MUST emit a `Warning` event when prepending exceeds 60% so we collect data on the right default.

**FR-26.** docker-agent MUST persist per-subagent harness session tokens on the parent `Session` via `Session.HarnessSession map[string]string` (keyed by agent name, value is the adapter-opaque token). No separate filesystem layout. The map serializes through the existing session-store JSON; no schema migration.

**FR-27.** `HarnessSessionRequest` MUST carry `PriorTurns []chat.Message` for adapters that need to construct multi-turn prompts. The runtime supplies the parent's relevant context from `parent.GetAllMessages()`; the adapter applies the token budget and decides how many turns to prepend.

### 4.5 Error handling

**FR-28.** `RunError` MUST carry: `code` (enum below), `message`, `retryable` (bool), `cause` (string), and optional `retry_after_seconds` (int) for rate limits.

Canonical error codes:

```
binary_not_found, binary_version_mismatch, auth_failed, rate_limited,
network_error, timeout, context_exhausted, permission_denied,
capability_mismatch, harness_crashed, protocol_error, cancelled, unknown
```

`rate_limited` is retryable with `retry_after_seconds`. `capability_mismatch` fires when an orchestrator request exceeds an adapter's declared capabilities (e.g. system prompt to an adapter with `SupportsPerCallSystemPrompt=false`).

Per-adapter mapping tables (harness signal → canonical code) live in §7.

**FR-29.** Timeouts default to 5 minutes per `Run`, configurable per agent. Hitting the timeout MUST emit `RunError{code: timeout, retryable: true}` and tear down per FR-13.

**FR-30.** Malformed JSON/JSON-RPC MUST emit `RunError{code: protocol_error, retryable: false}` with offending bytes (truncated to 1KB) in `cause`.

**FR-31.** Non-zero process exit before `RunEnd` MUST emit `RunError{code: harness_crashed}` with exit code and last 4KB of stderr in `cause`.

**FR-32.** The orchestrator MUST receive every `RunError` as a tool-call failure (analogous to a model tool error), so existing retry/fallback logic applies unchanged.

### 4.6 Permission handling (ACP)

**FR-33.** ACP adapters MUST forward every `session/request_permission` JSON-RPC call from the harness as a `PermissionPending` canonical event with: request ID, operation (`fs/write_text_file`, `terminal/create`), target path or command, and harness-supplied `reason`.

**FR-34.** Permission resolution order: **team-level `team.Permissions()` allow/ask/deny patterns first**, then per-agent `permission_policy`, then TUI prompt. This preserves the security posture for users who configured deny patterns at the team level (otherwise harness tools would silently bypass them). Adapter MUST translate the final decision into an ACP `session/permission_response` reply within 30s; otherwise emit `RunError{code: permission_denied}`.

**FR-35.** `permission_policy.auto_allow` is available only with `i_understand_the_risk: true` (FR-7). Default is `prompt`.

**FR-36.** `auto_allow` decisions MUST still emit a `PermissionResolved{decision: allow}` event for observability. TUI and audit logs need the record. Mirrors the existing `AuthorizationEvent` pattern at `pkg/runtime/event.go:450`.

**FR-37.** TUI MUST use `ToolCallConfirmationEvent` (same event type as model-backed permission prompts) for ACP permission prompts. No TUI changes required for harness paths. This is an enforceable invariant, not a UX aspiration.

### 4.7 Sandboxing (ACP terminal/* and fs)

**FR-38.** All ACP `fs/read_text_file` and `fs/write_text_file` operations MUST be resolved against an explicit sandbox root (the agent's `working_dir`, defaulting per FR-8). Paths that resolve outside the sandbox root (after symlink resolution) MUST be rejected with ACP error `permission_denied` and MUST NOT raise a `PermissionPending`. (`fs/list_dir` is NOT in `acp-go-sdk@v0.13.0`; if it lands later, treat it under the same rule.)

**FR-39.** `terminal/create` MUST set the child shell CWD to the sandbox root and refuse commands containing `cd` to a path outside the root (best-effort string match) unless `permission_policy.terminal = allow_unrestricted` is explicit.

**FR-40.** All sandbox enforcement MUST occur in shared `pkg/harness/sandbox/`, not per-adapter, and not in the harness. Tests MUST verify a hostile harness sending `..`-traversal or symlink escape is rejected.

**FR-41.** Environment variables exposed to harness child processes MUST be filtered through an allowlist: `PATH`, `HOME`, `USER`, `LANG`, `LC_*`, `TERM`, plus any explicitly listed in `harness.env`. docker-agent's own secrets MUST NOT leak unless explicitly passed.

### 4.8 New requirements (from arch review)

**FR-NEW-1.** `on_agent_switch` and `subagent_stop` hooks MUST fire on harness sub-sessions. `pre_tool_use` and `before_llm_call` hooks MUST NOT fire (harness owns its loop).

**FR-NEW-2.** ACP permission prompts MUST go through `team.Permissions()` first, then per-agent `permission_policy`, then TUI. (See FR-34.)

**FR-NEW-3.** Telemetry MUST record: harness type, cold start latency, per-event latency, error code distribution. Wired through `r.telemetry.RecordHarnessStart/Finish/Event`.

**FR-NEW-4.** OTel span `runtime.harness_session` MUST be opened per sub-session, with attributes for harness type, working dir, resume-vs-new.

**FR-NEW-5.** `run_skill` MUST reject harness-backed agents at validation time. Skills require model-backed agents in v1 (the skill's system prompt has no clean place to land on a self-contained harness).

**FR-NEW-6.** TUI MUST use `ToolCallConfirmationEvent` for ACP permission prompts. (See FR-37.)

**FR-NEW-7.** Working-dir fallback: `harness.working_dir` ?? `session.WorkingDir` ?? `os.Getwd()`. (See FR-8.)

**FR-NEW-8.** `AdapterCapabilities()` returns the static support surface. Per-session ACP capability negotiation happens inside `Run` and may downgrade actual session behavior (e.g. emit `RunError{code: capability_mismatch}` if the harness lacks a required capability). The static/negotiated split is documented per adapter.

**FR-NEW-9.** Harness concurrency for parallel fan-out (JTBD 3) rides on the existing bgAgents handler (`runtime.go:238`). Sequential `transfer_task` invocations are unlimited.

**FR-NEW-10.** `Run` returning a non-nil error (adapter-internal bug, sink unreachable) MUST be silently converted by the runtime to `ErrorEvent{code: harness_crashed}`. The error is never propagated to the orchestrator loop.

**FR-NEW-11.** An agent's harness session token is owned by one process at a time. Concurrent reuse of the same session token by two adapter instances is an error; the runtime MUST detect and reject the second use with `RunError{code: protocol_error}`. Prevents corruption of multi-turn history when two `@code-reviewer` instances run concurrently.

**FR-NEW-12.** CI integration tests require real harness binaries (`claude`, `codex`, `opencode`, `copilot`, `openclaw`) on CI runners. CI runner provisioning (image build, secret management for `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GITHUB_TOKEN`, cost budget for CI calls) is a prerequisite that MUST be resolved before Phase 2 begins. Surface to the platform team at Phase 0 kickoff.

**FR-NEW-13.** Each adapter MUST ship a `testdata/` directory with recorded fixture sessions (harness stdout/stderr/ACP frames). The adapter test suite MUST be runnable without the real harness binary using these fixtures. `pkg/harness/replay/` provides the record/replay harness; adapters consume it via `replay.PlayFixture(t, "testdata/multi_tool_call.jsonl")`. This is in scope for v1, not v1.1.

---

## 5. Non-functional requirements

### 5.1 Performance

**NFR-1.** Cold start budget per harness: ≤3s for Claude Code, ≤2s for Codex and OpenCode, ≤1.5s for ACP harnesses. Exceeding the budget is logged as a warning, not a failure.

**NFR-2.** Adapter overhead (event normalization, JSON parse, channel send) MUST be ≤5ms p99 per event. Measured via benchmark.

**NFR-3.** End-to-end latency from harness stdout to TUI render MUST be ≤50ms p99.

### 5.2 Reliability

**NFR-4.** Adapter MUST recover from a single transient read error (EAGAIN, partial line). Two consecutive read errors → `RunError{code: protocol_error}`.

**NFR-5.** No goroutine leaks. Verified by `goleak`.

**NFR-6.** Cancellation observed within 200ms.

### 5.3 Security

**NFR-7.** Sandbox enforcement (FR-38–41) is a security boundary. Bypass is P0.

**NFR-8.** Harness binaries are not checksum-verified in v1. PATH lookup is logged for audit.

**NFR-9.** Credentials for vendor harnesses are the harness's responsibility. Adapter MAY forward env vars listed in `Capabilities().Requires.EnvVars` automatically; users may opt out via `harness.env: {VAR: null}`.

### 5.4 Concurrency

**NFR-10.** Multiple harness subagents MUST run in parallel from one orchestrator turn. Default concurrency limit per team: 4 (configurable). Exceeding queues, does not error. Routed through the bgAgents handler (FR-NEW-9).

**NFR-11.** Two subagents of the same type with different working dirs MUST run in isolated processes and not share ACP connections or session tokens. Process pool keys: `(agent name, working dir)`.

---

## 6. Config schema

### 6.1 Schema reference

```yaml
agents:
  - name: string                    # required, unique per team
    harness:                        # required if model: omitted
      type: enum                    # claude-code | codex | opencode | copilot | openclaw
      command: string               # optional, override binary path
      args: [string]                # optional, appended to adapter defaults
      env: map[string]string        # optional, allowlisted env vars
      working_dir: string           # optional, defaults per FR-8
      timeout: duration             # optional, default 5m
      min_version: string           # optional, override Capabilities().Requires.MinVersion
      permission_policy:            # optional, ACP only
        fs_write: enum              # prompt | auto_allow | auto_deny
        terminal: enum              # prompt | auto_allow | allow_unrestricted | auto_deny
        i_understand_the_risk: bool # required if any auto_allow / allow_unrestricted
      config:                       # optional, adapter-specific typed map
        # ... adapter-specific keys, validated at load time with DisallowUnknownField
```

### 6.2 Examples

**Minimal (one-line case):**

```yaml
agents:
  - name: reviewer
    harness:
      type: claude-code
```

**Claude Code with knobs:**

```yaml
agents:
  - name: code-reviewer
    description: Deep code review using Claude Code
    harness:
      type: claude-code
      timeout: 10m
      config:
        max_turns: 20
        system_append: "Focus on security and correctness."
```

**Codex for greenfield:**

```yaml
agents:
  - name: prototype-builder
    description: New feature prototyping with Codex
    harness:
      type: codex
      working_dir: /tmp/proto
      config:
        model: gpt-5-codex
        reasoning_effort: high   # enum: low | medium | high
```

**OpenCode CLI:**

```yaml
agents:
  - name: refactor-helper
    description: OpenCode-backed refactoring
    harness:
      type: opencode
      config:
        task_prefix: "You are a refactoring assistant. "
```

(See §7.3: OpenCode CLI does not support per-call system prompts. `task_prefix` is the documented workaround; the load-time warning surfaces this.)

**Copilot CLI via ACP:**

```yaml
agents:
  - name: copilot-edit
    description: GitHub Copilot CLI in ACP mode
    harness:
      type: copilot
      working_dir: ./src
      permission_policy:
        fs_write: prompt
        terminal: auto_deny
      config:
        acp_handshake_timeout: 5s
```

**OpenClaw with auto-allow (explicit risk acknowledgment):**

```yaml
agents:
  - name: openclaw-batch
    description: OpenClaw running batch fs ops in a sandbox
    harness:
      type: openclaw
      working_dir: ./scratch
      permission_policy:
        fs_write: auto_allow
        terminal: prompt
        i_understand_the_risk: true
```

### 6.3 Validation rules

- `model:` and `harness:` mutually exclusive (FR-1).
- `harness.type` MUST be a v1 enum value.
- `permission_policy.i_understand_the_risk: true` requires at least one `auto_allow` or `allow_unrestricted`; vice versa (FR-7).
- `working_dir` MUST be absolute or relative to team config dir; resolved at load time (FR-8).
- `harness.config` unknown keys → load-time error naming the offending key.
- `harness:` agents cannot have `sub_agents` or `handoffs` (FR-5; FR-NEW-5 disallows `run_skill` targets).

### 6.4 CLI surfaces

- `docker-agent harness describe <type>` — print `AdapterCapabilities` and accepted `harness.config` schema as YAML.
- `docker-agent config validate` — reject unknown `harness.config` keys with a clear error pointing at the offending line.
- `docker-agent harness trace <agent>` — stream canonical events for an active session in human-readable form.
- `docker-agent harness lint <events.jsonl>` — validate a recorded event stream against the FSM rules (FR-17).

---

## 7. Adapter specs

One section per v1 harness. Each covers invocation, event mapping, error mapping, gaps, multi-turn.

### 7.1 Claude Code CLI

**Binary:** `claude`. Min version pinned in `Capabilities().Requires`.

**Invocation:**
```
claude --output-format stream-json --print "<task>" [--resume <session-id>] [--max-turns N] [--append-system-prompt <text>]
```

**Why these flags:** stream-json → NDJSON one event/line; `--print` → non-interactive; `--resume` → native multi-turn; `--max-turns` → loop bound; `--append-system-prompt` → orchestrator guidance.

**Event mapping (Claude Code → canonical):**

| Claude Code | Canonical |
|---|---|
| `system` (init) | `RunStart` (extract session_id, model, tools) |
| `assistant.message_start` | `TextStart` |
| `assistant.message_delta` (text) | `TextDelta` |
| `assistant.message_delta` (thinking) | `ReasoningDelta` |
| `assistant.message_stop` | `TextEnd` |
| `tool_use_start` | `ToolCallStart` |
| `tool_use_delta` | (folded into `ToolCallStart` payload; no separate canonical event) |
| `tool_result` | `ToolCallEnd` |
| `result` (final) | `RunEnd` (with usage, cost, session_id) |
| Stream close before `result` | `RunError{code: harness_crashed}` |

**Error mapping:**

| Signal | Code | Retryable |
|---|---|---|
| HTTP 429 from Anthropic | `rate_limited` | yes (extract `Retry-After`) |
| HTTP 401/403 | `auth_failed` | no |
| Process exit before `result` | `harness_crashed` | yes |
| Malformed NDJSON line | `protocol_error` | no |
| Context cancellation | `cancelled` | no |
| Wall-clock timeout | `timeout` | yes |

**Multi-turn:** Native `--resume <session-id>`. Adapter persists session_id in `HarnessSessionRequest.SessionToken` on `RunEnd`.

**Capabilities:**
```go
AdapterCapabilities{
  Protocol: ProtocolStream,
  Requires: HostRequirements{Binary: "claude", EnvVars: []string{"ANTHROPIC_API_KEY"}},
  Features: AdapterFeatures{
    SupportsMultiTurn:           true,
    SupportsPerCallSystemPrompt: true,
    StreamsTextDeltas:           true,
    StreamsReasoning:            true,
  },
  IdleTimeout: 10 * time.Minute,
}
```

### 7.2 Codex CLI

**Binary:** `codex`. Min version pinned.

**Invocation:**
```
codex --json [--model <id>] [--reasoning-effort <low|medium|high>] "<task>"
```

**Event mapping:**

| Codex | Canonical |
|---|---|
| `session.start` | `RunStart` |
| `reasoning.start` / `reasoning.delta` / `reasoning.end` | `ReasoningStart` / `ReasoningDelta` / `ReasoningEnd` |
| `message` (final text, no streaming) | `TextStart` immediately followed by `TextEnd` with full text in `Content` (FR-19) |
| `tool_call` (atomic, args+result) | `ToolCallStart` immediately followed by `ToolCallEnd` |
| `session.end` | `RunEnd` |

**Error mapping:**

| Signal | Code | Retryable |
|---|---|---|
| HTTP 429 from OpenAI | `rate_limited` | yes |
| HTTP 401/403 | `auth_failed` | no |
| Stdout EOF before `session.end` | `harness_crashed` | yes |
| Malformed JSONL | `protocol_error` | no |
| Multi-turn budget exhausted | `context_exhausted` | no |
| Wall-clock timeout | `timeout` | yes |

**Multi-turn:** Simulated via prompt prepending (FR-25). Token budget default 50% of context window; configurable via `config.multi_turn_budget_tokens`.

**Capabilities:**
```go
Features: AdapterFeatures{
  SupportsMultiTurn:           true,   // simulated
  SupportsPerCallSystemPrompt: true,
  StreamsTextDeltas:           false,  // enforced by FSM (FR-19)
  StreamsReasoning:            true,
}
```

### 7.3 OpenCode CLI

**Binary:** `opencode`. Min version pinned.

**Invocation:**
```
opencode --format json "<task>"
```

**Event mapping:** Similar to Claude Code; OpenCode emits atomic tool+result pairs, text in chunks (deltas where present, single message otherwise).

**Error mapping:**

| Signal | Code | Retryable |
|---|---|---|
| Upstream rate limit | `rate_limited` | yes |
| Auth failure | `auth_failed` | no |
| Process crash | `harness_crashed` | yes |
| Malformed JSON | `protocol_error` | no |

**Known gaps:** No per-call system prompt in CLI mode. Adapter prepends `config.task_prefix` to the task string and emits a one-time `Warning` event at team-load time: `"opencode CLI does not support per-call system prompts; using task prefix. See OpenCode SSE in v1.1."` SSE transport that fixes this is v1.1.

**Multi-turn:** Simulated, same as Codex.

**Capabilities:**
```go
Features: AdapterFeatures{
  SupportsMultiTurn:           true,
  SupportsPerCallSystemPrompt: false,
  StreamsTextDeltas:           true,
  StreamsReasoning:            false,
}
```

### 7.4 Copilot CLI (ACP)

**Binary:** `copilot --acp`. Min version pinned.

**Wire:** JSON-RPC 2.0 via `acp-go-sdk@v0.13.0` `NewClientSideConnection`. Adapter is the **client**; harness is the **server**. The client adapter lives in `pkg/harness/acp/copilot/`, NOT in `pkg/acp/` (which is the server-side ACP implementation for `docker-agent serve acp`).

`NewClientSideConnection(client, peerInput io.Writer, peerOutput io.Reader)` — `peerInput` writes to harness stdin, `peerOutput` reads from harness stdout. Param ordering matters.

**ACP methods the adapter calls (outbound):**
- `initialize` — handshake, capability exchange. Timeout via `config.acp_handshake_timeout` (default 5s).
- `session/new` — start a session.
- `session/prompt` — send the task.
- `Cancel` — polite cancellation before SIGTERM (FR-13).

**ACP methods the adapter handles (inbound from harness, via `acp.Client` interface):**
- `SessionUpdate` — stream events. Mapped to canonical `Text*`, `ToolCall*`, `Reasoning*`.
- `ReadTextFile`, `WriteTextFile` — adapter executes against sandbox (FR-38).
- `CreateTerminal`, `TerminalOutput`, `WaitForTerminalExit`, `KillTerminal`, `ReleaseTerminal` — adapter executes inside sandbox (FR-39).
- `RequestPermission` — synchronous; adapter emits `PermissionPending`, awaits resolution per FR-34, replies.

**Note:** `fs/list_dir` is NOT in `acp-go-sdk@v0.13.0`. Sandbox enforcement covers only the methods the SDK exposes.

**Static vs negotiated capabilities:** `AdapterCapabilities()` declares what Copilot's adapter will use. At `initialize`, the harness reports its actual capabilities; the adapter MUST honor what the harness reports (do not call `ResumeSession` if `Resume` capability absent). If a required capability is missing, emit `RunError{code: capability_mismatch}`.

**Connection lifecycle:** `ClientSideConnection.Done()` fires on peer crash, framing error, or normal close. The process pool MUST handle `Done()` independently of the idle timeout.

**Event mapping (ACP `SessionUpdate` → canonical):**

| ACP update | Canonical |
|---|---|
| `agent_message_chunk` | `TextDelta` (bracketed by `TextStart`/`TextEnd`) |
| `agent_thought_chunk` | `ReasoningDelta` |
| `tool_call` | `ToolCallStart` |
| `tool_call_update` (status: completed/failed) | `ToolCallEnd` |
| `plan` | raw via `RawEventSink` (out of canonical set in v1) |

**Error mapping:**

| Signal | Code | Retryable |
|---|---|---|
| HTTP 429 / GitHub rate limit | `rate_limited` | yes |
| Auth failure | `auth_failed` | no |
| Harness lacks required ACP capability | `capability_mismatch` | no |
| Process exit / `Done()` before `RunEnd` | `harness_crashed` | yes |
| JSON-RPC framing error | `protocol_error` | no |
| Permission timeout (30s) | `permission_denied` | no |

**Multi-turn:** Native via ACP session lifetime. Session token is the ACP session ID. The runtime keeps the adapter process pooled until team-session end or idle timeout (default 10m, configurable via `IdleTimeout`).

**Capabilities:**
```go
AdapterCapabilities{
  Protocol: ProtocolACP,
  Requires: HostRequirements{Binary: "copilot", EnvVars: []string{"GITHUB_TOKEN"}},
  Features: AdapterFeatures{
    SupportsMultiTurn:           true,
    SupportsPerCallSystemPrompt: true,
    StreamsTextDeltas:           true,
    StreamsReasoning:            true,
  },
  IdleTimeout: 10 * time.Minute,
}
```

### 7.5 OpenClaw (ACP)

**Binary:** `openclaw`. Min version pinned.

**Invocation, wire, event mapping:** Identical pattern to Copilot (§7.4). Shared base in `pkg/harness/acp/`.

**Differences from Copilot:**
- No `GITHUB_TOKEN` requirement; uses its own auth.
- Different built-in tool set; adapter MUST NOT hardcode tool names (use the ACP-provided `name` field verbatim).
- Plan events more verbose; `RawSink` recommended for plan debugging.
- `IdleTimeout: 2 * time.Minute` (faster cold start than Copilot).

**Error mapping:** Same as Copilot, minus GitHub-specific signals.

**Capabilities:** Same shape as Copilot, with `IdleTimeout: 2m` and no `GITHUB_TOKEN` requirement.

---

## 8. Success metrics

**Adoption (90 days post-GA):**
- ≥10 docker-agent users have a harness-backed agent in their team YAML.
- ≥3 distinct harness types in use across the active user base.
- Mark's GM team config includes ≥2 harness-backed subagents.

**Reliability:**
- p99 successful `RunEnd` rate ≥98% across all harnesses (excluding user cancellation and auth errors).
- Zero goroutine/process leaks in CI over 1000 consecutive session runs.
- Zero sandbox escapes reported.

**Performance:**
- p99 cold start within NFR-1 budgets.
- p99 event-stream latency (harness stdout → TUI render) ≤50ms.

**Developer experience:**
- New adapter (e.g. Cursor in v1.1) in ≤500 LOC and ≤2 weeks for one engineer. Baseline reference: the Codex adapter LOC count at v1 ship.
- ≥80% of adapter logic is shared (event normalization, sandbox, process lifecycle); per-adapter code is wire-format + capability mapping only.
- Adapter test suite runs without real harness binaries (FR-NEW-13).

**Output quality:**
- Mark's two-harness side-by-side benchmark (JTBD 3) achievable end-to-end without scripting.
- ACP permission prompts surface in TUI with same latency feel as model-backed prompts.

---

## 9. Adapter author guide

### 9.1 Implementing a new adapter

The minimum surface area:

1. Create `pkg/harness/<name>/` (copy from `pkg/harness/example/`).
2. Implement `HarnessAdapter` (3 methods: `Name`, `Capabilities`, `Run`).
3. Define a typed `Config` struct; register with the loader so unknown keys fail at validate time (FR-5).
4. Register the adapter in `pkg/harness/registry.go` at init.
5. Map every canonical event your harness can produce (§7 mapping tables).
6. Map every error signal to a canonical error code (§7 error mapping tables).
7. If filesystem or terminal access: call `pkg/harness/sandbox/` helpers; do not implement path canonicalization yourself.

### 9.2 Testing without the binary

Three test surfaces, in increasing fidelity:

1. **Unit tests** against recorded fixtures: `replay.PlayFixture(t, "testdata/multi_tool_call.jsonl")` feeds bytes through your parser, asserts emitted canonical events match expected.
2. **Conformance suite**: `harness/conformance` ships 20 canonical scenarios (single message, multi tool call, error mid-stream, cancellation, multi-turn resume, heartbeat under no-output, sandbox escape attempt, parallel fan-out, permission allow/deny, …). Your adapter MUST pass all 20.
3. **Integration tests** with real binary: gated by build tag, run in CI only (FR-NEW-12).

### 9.3 Debugging wrong events

- `docker-agent harness trace <agent>` — stream canonical events to stdout.
- `docker-agent harness lint events.jsonl` — validate an event stream against FSM rules (FR-17).
- Per-session adapter log: `${XDG_STATE_HOME}/docker-agent/sessions/<id>/harness-<n>.stderr` (raw harness stderr) plus `harness-<n>.adapter.log` (adapter's own slog records).
- Every event carries `SessionID`, `AgentName`, `Timestamp`, and (where applicable) `MessageID`/`CallID`. FSM violations log with the preceding 3 events for context.

### 9.4 Conformance test suite

The 20 conformance scenarios are the contract. They assert:

- Lifecycle: every session has exactly one `RunStart` and one terminal.
- Balance: `Start`/`End` pairs balanced for `Text*`, `Reasoning*`, `ToolCall*`.
- Heartbeat: emitted at least every 30s during active runs.
- Attribution: every event has `SessionID` and `AgentName`.
- Cancellation: observed within 200ms (NFR-6), no orphan processes.
- Permission: `PermissionPending` → `PermissionResolved` round-trip works through team/agent/TUI gates.
- Error codes: each canonical error code is reachable via a documented trigger.
- Multi-turn: `SessionToken` round-trips for native harnesses; prompt prepending budget honored for simulated.

If your adapter fails a conformance scenario, your adapter is non-compliant; the scenario is correct by definition.

---

## 10. Implementation phases

Critical path: plumbing first, adapters second. Adapters are parallelizable once the runtime branch and canonical type model exist.

**Phase 0 — Foundations (1 engineer, 1 week)**

1. Bump config version to `"10"`; freeze `pkg/config/v9/` snapshot.
2. Add `HarnessConfig` to `pkg/config/latest/types.go`; `Validate()` rule for `model:`/`harness:` exclusivity; sub-agent/handoff rejection for harness agents; `i_understand_the_risk` cross-field rule.
3. Add `WithHarness` to `pkg/agent/opts.go`; `HasHarness()` method and `harness` field on `*Agent`.
4. Add `Session.HarnessSession map[string]string` field.
5. Wire `pkg/teamloader/teamloader.go` to build harness-backed `*Agent` instances; PATH check for binary.
6. Stub `pkg/harness/`: `HarnessAdapter` interface, discriminated-union `Event` types, `HarnessSessionRequest`, empty registry, sandbox package, replay package, fake adapter.
7. **Prerequisite:** Surface CI runner provisioning need to platform team (FR-NEW-12).

**Phase 1 — Runtime branch + Claude Code adapter (1 engineer, 2 weeks)**

8. Implement `runHarnessForwarding` and `runHarnessCollecting` in `pkg/runtime/agent_delegation.go`. Translator in `pkg/harness/translate.go` emits the four required runtime events (FR-21).
9. FSM enforcer wrapping `EventSink` (FR-17).
10. Hooks integration: wire `on_agent_switch` and `subagent_stop` (FR-NEW-1).
11. Telemetry and OTel span integration (FR-NEW-3, FR-NEW-4).
12. Claude Code adapter end-to-end. Highest dogfood value, fewest gaps, native multi-turn.

**Phase 2 — Parallel adapter build (3 engineers, 2 weeks)**

Adapters ship independently once Phase 1 lands. Requires CI runner provisioning resolved.

13. Codex adapter (simulated multi-turn, no text streaming).
14. OpenCode CLI adapter (clone of Codex parser; load-time warning for no per-call system prompt).
15. ACP base in `pkg/harness/acp/` + Copilot adapter on top.

**Phase 3 — OpenClaw + hardening (1 engineer, 1 week)**

16. OpenClaw adapter.
17. Sandbox hostile-path tests (FR-38–40): symlink, `..`, absolute outside root. P0 security tests.
18. Goleak / process-orphan tests (FR-13, NFR-5, NFR-6). Must pass 1000 consecutive runs in CI.
19. Conformance suite finalized; all 5 adapters green.
20. `docker-agent harness describe` / `trace` / `lint` CLI commands.

**Phase 4 — Dogfood + GA (1 week)**

21. Migrate Mark's GM team config to use ≥2 harness-backed subagents.
22. JTBD 3 two-harness side-by-side benchmark verified end-to-end.
23. Doc page in OpenCode docs site; cross-link from `/docs/agents`.

**Total: 6–7 weeks with 3 engineers.**

**Critical-path dependencies:**
- Phase 0 → Phase 1 (hard).
- Phase 1 → Phase 2 (hard; runtime branch must land first).
- CI runner provisioning → Phase 2 (hard; FR-NEW-12).
- Phase 2 + Phase 3 → Phase 4.

**Risks engineering must escalate:**
- CI runner provisioning latency (target: resolved by Phase 1 end).
- Hooks policy decision (FR-NEW-1) — get product sign-off before Phase 1.
- Team-level permission interaction with ACP prompts (FR-34) — route through CSO review.

---

## 11. Out of scope (v1)

| Item | Reason | Target |
|---|---|---|
| Cursor adapter | NDJSON schema not stable | v1.1 if it stabilizes |
| OpenCode SSE transport | Per-call system prompts; needs HTTP/SSE infra | v1.1 |
| Harness-as-orchestrator | Recursion/protocol problem; no primary user need | v2 |
| Sub-harness delegation (harness → harness) | Same recursion problem | v2 |
| Custom tool injection into self-contained harnesses | Different injection mechanism per harness | TBD |
| Unified cost/usage aggregation across harnesses | Each harness reports differently | v1.1 |
| Harness binary checksum verification | Defer to OS package manager / user trust | TBD |
| AG-UI wire format compatibility | No consumer yet | When a real AG-UI consumer exists |
| ACP shared session across multiple subagents | No clear user need | TBD |
| Streaming token counts during a run | Most harnesses report only at end | When harnesses support it |
| Remote/network harnesses (non-stdio) | All v1 harnesses are local stdio | v2 |
| `run_skill` targeting harness-backed agents | Skill system prompt has no place to land | v2 if skills evolve |
| Plan events as first-class canonical events | Plans flow through `RawEventSink` in v1 | v1.1 |

---

## Appendix A: Interface sketch

```go
package harness

// HarnessAdapter is the contract every adapter implements.
type HarnessAdapter interface {
    Name() string
    Capabilities() AdapterCapabilities
    Run(ctx context.Context, req HarnessSessionRequest) error
}

// HarnessSessionRequest is the per-invocation request payload.
type HarnessSessionRequest struct {
    Task         string
    SystemPrompt string
    SessionToken string             // empty on first turn
    WorkingDir   string
    Env          map[string]string
    PriorTurns   []chat.Message     // for simulated multi-turn (FR-27)
    Events       EventSink          // canonical event sink (FSM-enforced)
    RawSink      RawEventSink       // optional; nil if consumer opts out
    Config       any                // adapter's typed config struct
}

// ACPRequest extends HarnessSessionRequest for ACP adapters.
// Adapters use type assertion: if acp, ok := req.(ACPRequest); ok { ... }
type ACPRequest interface {
    ToolExecutor() ToolExecutor
    Permission() PermissionGate
}

// EventSink receives canonical events from the adapter. Buffering and
// backpressure policy live in the runtime, not the adapter.
type EventSink interface {
    Emit(Event)
}

type RawEventSink interface {
    EmitRaw(source string, frame []byte)
}

// Event is a discriminated union. One concrete type per kind.
type Event interface{ isHarnessEvent() }

type RunStart struct {
    SessionID string
    AgentName string
    Timestamp time.Time
    Model     string
    Tools     []string
}
type RunEnd struct {
    SessionID    string
    AgentName    string
    Timestamp    time.Time
    SessionToken string             // for multi-turn resume
    Usage        Usage              // typed; not map[string]any
}
type RunError struct {
    SessionID         string
    AgentName         string
    Timestamp         time.Time
    Code              ErrorCode
    Message           string
    Retryable         bool
    Cause             string
    RetryAfterSeconds int            // optional, for rate_limited
}

type TextStart struct      { SessionID, AgentName, MessageID string; Timestamp time.Time }
type TextDelta struct      { SessionID, AgentName, MessageID, Text string; Timestamp time.Time }
type TextEnd struct        { SessionID, AgentName, MessageID, Content string; Timestamp time.Time }

type ReasoningStart struct { SessionID, AgentName, MessageID string; Timestamp time.Time }
type ReasoningDelta struct { SessionID, AgentName, MessageID, Text string; Timestamp time.Time }
type ReasoningEnd struct   { SessionID, AgentName, MessageID string; Timestamp time.Time }

type ToolCallStart struct  { SessionID, AgentName string; CallID ToolCallID; Name string; Args json.RawMessage; Timestamp time.Time }
type ToolCallEnd struct    { SessionID, AgentName string; CallID ToolCallID; Result json.RawMessage; Error string; Timestamp time.Time }

type PermissionPending struct  { SessionID, AgentName, RequestID, Operation, Target, Reason string; Timestamp time.Time }
type PermissionResolved struct { SessionID, AgentName, RequestID string; Decision PermissionDecision; Scope PermissionScope; Timestamp time.Time }

type Heartbeat struct      { SessionID, AgentName string; Timestamp time.Time }

func (RunStart) isHarnessEvent()           {}
func (RunEnd) isHarnessEvent()             {}
func (RunError) isHarnessEvent()           {}
func (TextStart) isHarnessEvent()          {}
func (TextDelta) isHarnessEvent()          {}
func (TextEnd) isHarnessEvent()            {}
func (ReasoningStart) isHarnessEvent()     {}
func (ReasoningDelta) isHarnessEvent()     {}
func (ReasoningEnd) isHarnessEvent()       {}
func (ToolCallStart) isHarnessEvent()      {}
func (ToolCallEnd) isHarnessEvent()        {}
func (PermissionPending) isHarnessEvent()  {}
func (PermissionResolved) isHarnessEvent() {}
func (Heartbeat) isHarnessEvent()          {}

// Typed enums (not raw strings) for compile-time checking.
type ProtocolClass string
const (
    ProtocolStream ProtocolClass = "stream"
    ProtocolACP    ProtocolClass = "acp"
)

type ErrorCode string
const (
    ErrBinaryNotFound        ErrorCode = "binary_not_found"
    ErrBinaryVersionMismatch ErrorCode = "binary_version_mismatch"
    ErrAuthFailed            ErrorCode = "auth_failed"
    ErrRateLimited           ErrorCode = "rate_limited"
    ErrNetworkError          ErrorCode = "network_error"
    ErrTimeout               ErrorCode = "timeout"
    ErrContextExhausted      ErrorCode = "context_exhausted"
    ErrPermissionDenied      ErrorCode = "permission_denied"
    ErrCapabilityMismatch    ErrorCode = "capability_mismatch"
    ErrHarnessCrashed        ErrorCode = "harness_crashed"
    ErrProtocolError         ErrorCode = "protocol_error"
    ErrCancelled             ErrorCode = "cancelled"
    ErrUnknown               ErrorCode = "unknown"
)
```

Final shapes live in the arch spec; the above is the binding contract.

---

## Appendix B: Test plan summary

- **Unit:** each adapter's event-mapping function against recorded harness output fixtures in `testdata/` (FR-NEW-13).
- **Conformance:** 20 canonical scenarios run against every adapter (§9.4). FR-22 becomes a passing test, not a slogan.
- **Integration:** real binary per adapter, in CI behind a build tag (FR-NEW-12).
- **Sandbox:** hostile-path tests for FR-38–40 (symlink, `..`, absolute outside root).
- **Lifecycle:** goleak + process-orphan tests for FR-13, NFR-5, NFR-6. 1000 consecutive runs.
- **Multi-turn:** session-token round-trip for native harnesses; prompt prepending + budget for simulated.
- **Concurrency:** N=8 parallel subagents, no event interleaving across sessions (FR-16 attribution).
- **FSM:** FR-17 enforcer panics in dev on FSM violation; tests verify each violation is caught.
- **Permission ordering:** FR-34 routes prompts through team → agent policy → TUI; tests verify each gate fires.
