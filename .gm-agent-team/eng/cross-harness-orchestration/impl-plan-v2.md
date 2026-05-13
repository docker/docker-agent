# Implementation Plan v2: Cross-Harness Orchestration

**Source PRD:** `prd-v2.md`
**Architecture spec:** `arch-spec-v2.md`
**Supersedes:** `impl-plan.md` (v1)
**Branch:** `gm/cross-harness-orchestration`
**Baseline:** builds=true, tests=pre-existing failures in `pkg/config TestCheckRequiredEnvVars` and `pkg/teamloader TestLoadExamples (dmr/unload_on_switch)` — do not fix in this branch.

**Revision summary (v1 → v2).**

- P0-B updated with exact YAML unknown-key API call (`yaml.v3` `KnownFields(true)`) and exact error format string. (Fix 5)
- P0-B also enforces FR-NEW-5 (`run_skill` rejection of harness-backed agents) via new `ValidateSkillTarget` method on `*agent.Agent`; runtime call-site updated.
- P0-E adds `pkg/harness/replay/record.go` (replay recorder used by adapter integration tests to generate fixtures). (Consistency gap)
- P0-E also adds the `tokenInUse` map + `AcquireToken`/`ReleaseToken` to `pkg/harness/registry.go`. (FR-NEW-11 infrastructure)
- P1-A reflects `Run` returns void: includes the `runAdapter` wrapper with `recover()` for panic-to-`RunError` conversion. (Fix 2)
- P1-A test list adds the FR-NEW-10 case: adapter panic recovers to `RunError{Code: harness_crashed}` and parent session receives a tool failure.
- P1-A test list adds bgAgents wiring check.
- New P1-C "Session token ownership guard": runtime uses `AcquireToken` on resume, deregisters on `RunEnd`/`RunError`; second concurrent use of the same token emits `RunError{capability_mismatch}`. (FR-NEW-11 enforcement)
- P1-D becomes Claude Code adapter (previously P1-C in v1).
- P2-A and P2-B now both list `multiturn.go` and a shared budget-test pattern. (Consistency gap)
- ACP separation (Fix 3) is reflected in `HarnessAdapter` / `ACPAdapter` interfaces in P0-E and dispatch logic in P1-A.

---

## Phase 0 — Foundations

### Unit P0-A — Config snapshot: freeze `pkg/config/v9/`

Description: Copy current `pkg/config/latest/` tree to `pkg/config/v9/`. Pure copy. Package rename only.

Complexity: **S**

Files to create:
- `pkg/config/v9/types.go` (copy of `pkg/config/latest/types.go`, `package v9`)
- `pkg/config/v9/validate.go` (copy of `pkg/config/latest/validate.go`, `package v9`)
- Full directory snapshot of `pkg/config/latest/`

Files to read:
- `pkg/config/latest/` (full directory)
- `pkg/config/v8/` (one file, pattern reference)

Dependencies: none.

Build: `go build ./pkg/config/v9/...`
Test: `go test ./pkg/config/v9/...`

---

### Unit P0-B — Config schema: `HarnessConfig`, validation, version bump, YAML unknown-key error format

Description: In `pkg/config/latest/`, bump `Version` to `"10"`. Add `HarnessConfig` and `PermissionPolicyConfig` (arch spec §2.3). Validation rules per arch spec §2.3 (FR-1, FR-2, FR-5, FR-7).

**FR-NEW-5: harness-backed agents cannot be `run_skill` targets.** Add `ValidateSkillTarget() error` to `*agent.Agent` in `pkg/agent/validate.go` (file may need to be created) that returns an error when the agent has a harness. The error message is:

```
agent "<name>" has harness="<type>"; harness-backed agents cannot be used as skill targets in v1
```

Wire the validation in `pkg/runtime/loop.go` (or wherever `run_skill` resolves its target) immediately before dispatch.

**Fix 5: YAML unknown-key error format.** The teamloader will perform the actual unmarshal in P0-F, but the format is pinned here so both P0-B and P0-F agree:

- Use `yaml.v3` decoder with `KnownFields(true)`. The API is `yaml.NewDecoder(r).KnownFields(true)`. Do **not** use `DisallowUnknownField` — that is a method on `encoding/json`'s `Decoder` and does not exist on `yaml.v3`.
- The exact error message format an end user sees on a typo:

  ```
  error: unknown field "typo" in harness config for agent "code-reviewer"
    valid fields: type, command, args, env, working_dir, timeout, config
  ```

  Includes: the offending key (quoted), the agent name (quoted), the comma-separated list of valid yaml tags from the typed config struct.

The format is enforced by a helper `translateUnknownFieldError(agentName string, err error) error` defined in `pkg/teamloader/harness.go` and tested in P0-F. P0-B documents the format in a code comment on `HarnessConfig.Config` so adapter authors know what their users will see.

Complexity: **M**

Files to modify:
- `pkg/config/latest/types.go` — add `HarnessConfig`, `PermissionPolicyConfig`, `Harness *HarnessConfig` field on `AgentConfig`, bump `Version`.
- `pkg/config/latest/validate.go` — add validation rules.
- `pkg/config/upgrade.go` or `pkg/config/load.go` — add v9 → v10 step (no-op for configs without `harness:`).

Files to create:
- `pkg/agent/validate.go` (if not already present) with the `ValidateSkillTarget` method.

Files to read:
- `pkg/config/latest/types.go`, `pkg/config/latest/validate.go` (full)
- `pkg/config/load.go` (grep for `Version == "9"`)
- `prd-v2.md` §6
- `arch-spec-v2.md` §2.3, §2.4, §2.7 (FR-NEW-5), §3.9

Dependencies: P0-A.

Build: `go build ./pkg/config/... ./pkg/agent/...`
Test: `go test ./pkg/config/... ./pkg/agent/...`. Table-driven cases:
1. `model:` only valid
2. `harness:` only valid
3. both → error
4. neither → error
5. `harness:` with `sub_agents` → error
6. `harness:` with unknown `type` → error
7. `i_understand_the_risk: true` without `auto_allow` → error
8. v9 file with no `harness:` upgrades cleanly to v10
9. `ValidateSkillTarget()` on a model-backed agent returns nil
10. `ValidateSkillTarget()` on a harness-backed agent returns an error containing `harness-backed agents cannot be used as skill targets in v1`

---

### Unit P0-C — Agent harness field and opts

Description: Add `harness *HarnessSpec` field on `*agent.Agent` plus `HasHarness()`, `Harness()`, `WithHarness(spec)`. Define `HarnessSpec`, `PermissionPolicy`, `PermissionMode` in `pkg/agent/` (arch spec §2.2).

Complexity: **S**

Files to modify:
- `pkg/agent/agent.go` — add field, two accessors.
- `pkg/agent/opts.go` — add `WithHarness`.

Files to create:
- `pkg/agent/harness_spec.go` — `HarnessSpec`, `PermissionPolicy`, `PermissionMode` types.

Files to read:
- `pkg/agent/agent.go`, `pkg/agent/opts.go` (full)
- `arch-spec-v2.md` §2.2

Dependencies: none.

Build: `go build ./pkg/agent/...`
Test: `go test ./pkg/agent/...` — constructs an agent with `WithHarness(&HarnessSpec{AdapterName: "claude-code"})`, asserts accessors.

---

### Unit P0-D — Session `HarnessSession` field

Description: Add `HarnessSession map[string]string` field on `*session.Session` (arch spec §2.6). Add locked accessor pair `HarnessSessionGet(name) string` and `HarnessSessionSet(name, token string)` using the existing `Session.mu`.

Complexity: **S**

Files to modify:
- `pkg/session/session.go` — add field, two accessors.

Files to read:
- `pkg/session/session.go` (first 200 lines, plus marshaling code)
- `pkg/session/store.go` if it exists
- `arch-spec-v2.md` §2.6

Dependencies: none.

Build: `go build ./pkg/session/...`
Test: `go test ./pkg/session/...` — JSON round-trip; concurrent access doesn't race (run with `-race`).

---

### Unit P0-E — Harness package skeleton (with replay recorder and token guard)

Description: Create `pkg/harness/` with interfaces, event types, registry, FSM, heartbeat, sandbox stubs, fake adapter, replay infrastructure (now including the recorder), and the session-token ownership map (now including `AcquireToken`/`ReleaseToken`).

Two key interfaces in `pkg/harness/harness.go` (Fix 3, Fix 2):

```go
// Base interface. Non-ACP adapters implement this and only this.
type HarnessAdapter interface {
    Name() string
    Capabilities() AdapterCapabilities
    Run(ctx context.Context, req SubSessionRequest)  // void; events carry terminal state
}

// ACP adapters additionally implement this. The runtime detects via type assertion.
type ACPAdapter interface {
    HarnessAdapter
    RunACP(ctx context.Context, req SubSessionRequest, acp ACPCallbacks)
}
```

`SubSessionRequest` carries `ResumeToken string` and `SimulatedHistory []chat.Message` per Fix 4 (arch spec §3.3).

`ACPCallbacks` is a separate struct passed to `RunACP` only:

```go
type ACPCallbacks struct {
    ToolExecutor ToolExecutor
    Permission   PermissionRequester
}
```

Complexity: **M**

Files to create:
- `pkg/harness/harness.go` — `HarnessAdapter`, `ACPAdapter`, `SubSessionRequest`, `ACPCallbacks`, `AdapterCapabilities`, `HostRequirements`, `AdapterFeatures`, `ProtocolClass`, `EventSink`, `EventHandler`, `RawEventSink`, `ToolExecutor`, `PermissionRequester`, `PermissionDecision`, `PermissionScope`, `PermissionRequest`.
- `pkg/harness/event.go` — `Event` interface, `EventMeta`, 14 concrete event types, JSON Marshal/Unmarshal helpers keyed off a wire `Kind` field.
- `pkg/harness/errors.go` — `ErrorCode` typed string and the 13 canonical codes (including `ErrCodeHarnessCrashed` for the panic-recovery path and `ErrCodeCapabilityMismatch` used by the token guard).
- `pkg/harness/registry.go` — `Register(name, factory)`, `LookupAdapter(name)`, typed-config registration (`RegisterConfig(name, zero func() any)`, `UnmarshalConfig(name, raw any) (any, error)`).

  **PLUS new in v2: session-token ownership map.** Adds:
  ```go
  var (
      tokenInUseMu sync.Mutex
      tokenInUse   = make(map[string]bool) // key: adapter_name + ":" + token
  )
  func AcquireToken(adapterName, token string) bool
  func ReleaseToken(adapterName, token string)
  ```
  Per arch spec §3.11.
- `pkg/harness/fsm.go` — `NewEnforcer(downstream EventSink) EventSink`.
- `pkg/harness/heartbeat.go` — `NewTicker(ctx, interval, sink, meta) func()`.
- `pkg/harness/raw.go` — `Source*` constants.

Subpackages (stubs in this unit; real impl in P2-D):
- `pkg/harness/sandbox/sandbox.go` — `Resolve(root, path) (string, error)`, `ErrEscape` sentinel, `AllowedEnv()`.
- `pkg/harness/sandbox/env.go` — env allowlist + `Filter`.
- `pkg/harness/sandbox/terminal.go` — `GuardTerminalCommand(cmd string) error`.

Helpers (full in this unit):
- `pkg/harness/fake/adapter.go` — `New(events []harness.Event) harness.HarnessAdapter` for in-process tests.
- `pkg/harness/replay/replay.go` — `PlayFixture(t *testing.T, path string) []harness.Event` (FR-NEW-13).
- **`pkg/harness/replay/record.go` (NEW in v2)** — `Recorder` type that wraps an `EventSink` and writes events to NDJSON:
  ```go
  // Recorder wraps an EventSink and records every emitted event to NDJSON,
  // suitable for generating adapter fixtures during integration testing.
  type Recorder struct {
      Inner EventSink
      W     io.Writer
  }

  func NewRecorder(inner EventSink, w io.Writer) *Recorder
  func (r *Recorder) Emit(e Event) error  // writes JSON line then delegates to Inner
  func (r *Recorder) Close() error
  ```
  Used by adapter integration tests in P1-C, P2-A, P2-B, P2-C, P3-A to convert real-binary runs into fixture JSONL files committed to `testdata/`.
- `pkg/harness/example/adapter.go` — minimal no-op adapter, the template for new authors.

Files to read:
- `arch-spec-v2.md` §3 in full, §3.11
- `prd-v2.md` §4.2, §4.3, appendix A
- `pkg/runtime/event.go` (full)
- `pkg/agent/agent.go`, `pkg/agent/harness_spec.go` (after P0-C)
- `pkg/chat/` (just enough for `chat.Message` shape)

Dependencies: P0-C.

Build: `go build ./pkg/harness/...`
Test: `go test ./pkg/harness/...`. Required cases:
- FSM enforcer rejects duplicate `RunStart`, terminal-after-terminal, unbalanced `Start`/`End` pairs.
- Registry round-trips.
- `AcquireToken("claude-code", "abc")` returns true, second call returns false, after `ReleaseToken` returns true again. `AcquireToken("claude-code", "")` always returns true.
- Sandbox: `Resolve` traversal, symlink, escape cases.
- Env filter drops sensitive vars unless explicitly allowed.
- `Recorder` writes the expected NDJSON line per event; round-trips through `PlayFixture`.

---

### Unit P0-F — Teamloader: harness-backed agent construction

Description: In `pkg/teamloader/teamloader.go`, branch on `agentConfig.Harness != nil` (arch spec §2.7).

**Fix 5 enforcement in teamloader:**

Create `pkg/teamloader/harness.go` with:

```go
// unmarshalHarnessConfig converts the user's raw map[string]any into the
// adapter's registered typed config struct using yaml.v3 KnownFields(true).
// Returns a docker-agent-flavored error on unknown keys.
func unmarshalHarnessConfig(adapterName, agentName string, raw map[string]any, zero func() any) (any, error) {
    cfg := zero()
    b, err := yaml.Marshal(raw)
    if err != nil {
        return nil, fmt.Errorf("internal: marshal harness.config map: %w", err)
    }
    dec := yaml.NewDecoder(bytes.NewReader(b))
    dec.KnownFields(true)
    if err := dec.Decode(cfg); err != nil {
        return nil, translateUnknownFieldError(agentName, cfg, err)
    }
    return cfg, nil
}

// translateUnknownFieldError converts yaml.v3's "field <X> not found in type
// <T>" error into:
//   error: unknown field "<X>" in harness config for agent "<agent>"
//     valid fields: <comma-separated yaml tags from cfg's exported fields>
// If the error is not an unknown-field error, returns it wrapped with the
// agent name.
func translateUnknownFieldError(agentName string, cfg any, err error) error
```

The `valid fields:` list is built by reflecting on `cfg`'s struct type, reading the `yaml:"..."` tag from each exported field (stripping the part after the first comma to drop `omitempty` etc.). Empty tag means use lowercased field name.

PATH-check the binary via `exec.LookPath(spec.Command)` (or `Capabilities().Requires.Binary` when `Command == ""`). Missing → error naming the binary and the install hint from `Capabilities().Requires.InstallHint`.

Complexity: **M**

Files to modify:
- `pkg/teamloader/teamloader.go` — add harness branch in the per-agent build loop.

Files to create:
- `pkg/teamloader/harness.go` — `unmarshalHarnessConfig`, `translateUnknownFieldError`.
- `pkg/teamloader/testdata/harness-claude.yaml` — happy-path fixture.
- `pkg/teamloader/testdata/harness-unknown-key.yaml` — fixture with `max_tunrs` typo.
- `pkg/teamloader/testdata/harness-missing-binary.yaml` — fixture pointing at `/nonexistent/binary`.

Files to read:
- `pkg/teamloader/teamloader.go` (first 250 lines)
- `pkg/teamloader/agents.go` (or wherever `buildAgent` lives)
- `arch-spec-v2.md` §2.7, §3.9

Dependencies: P0-B, P0-C, P0-E.

Build: `go build ./pkg/teamloader/...`
Test: `go test ./pkg/teamloader/...`. Cases:
- Happy path: `harness-claude.yaml` loads, agent has `HasHarness() == true`, `Harness().AdapterName == "claude-code"`.
- Unknown key: `harness-unknown-key.yaml` fails to load with error message exactly matching the format spec in §3.9:
  ```
  error: unknown field "max_tunrs" in harness config for agent "code-reviewer"
    valid fields: max_turns, system_append, ...
  ```
  Test asserts the substring `unknown field "max_tunrs" in harness config for agent "code-reviewer"` AND `valid fields:` appears with at least `max_turns` listed.
- Missing binary: `harness-missing-binary.yaml` fails with message naming the binary and including the install hint substring.

---

### Unit P0-G — CI prerequisite: surface harness-binary provisioning to platform team

Description: NOT code. File a tracking issue with the platform team for CI runner images with `claude`, `codex`, `opencode`, `copilot`, `openclaw`, plus secrets and budget.

Complexity: **S** (no code).

Output: issue link saved in `.gm-agent-team/eng/cross-harness-orchestration/ci-prerequisites.md`.

Dependencies: none.

---

## Phase 1 — Runtime branch + Claude Code adapter

### Unit P1-A — Runtime translator, `runHarnessForwarding` skeleton, panic recovery

Description: Create `pkg/runtime/harness_delegation.go`. Implement `runHarnessForwarding`, `runHarnessCollecting`, the `translateSink` translator, `runtimePermissionRequester`, and the **`runAdapter` panic-recovery wrapper** (Fix 2).

The translator lives **here**, in `pkg/runtime/harness_delegation.go`. There is no `pkg/harness/translate.go` in v2 (Fix 1). This unit is the authoritative implementation of canonical → runtime event translation.

Wire the FSM enforcer in front of the translator. Open the `runtime.harness_session` OTel span. Persist `SessionToken` from `RunEnd` into `parent.HarnessSessionSet(child.Name(), token)`. Fire `subagent_stop` hook.

**Adapter dispatch (Fix 3 — ACP separation):**

```go
adapter := harness.LookupAdapter(child.Harness().AdapterName)
if acpAdapter, ok := adapter.(harness.ACPAdapter); ok {
    bindings := harness.ACPCallbacks{
        ToolExecutor: sandbox.NewToolExecutor(req.WorkingDir),
        Permission:   &runtimePermissionRequester{r, parent, child, evts},
    }
    if bindings.ToolExecutor == nil || bindings.Permission == nil {
        panic("runtime: ACPCallbacks nil after construction")
    }
    go r.runAdapterACP(ctx, acpAdapter, req, bindings)
} else {
    go r.runAdapter(ctx, adapter, req)
}
```

**Panic recovery wrapper (Fix 2 — FR-NEW-10):**

```go
// runAdapter calls a non-ACP adapter's Run with panic recovery. A panic is
// converted to a synthetic RunError so a buggy adapter cannot crash the
// orchestrator process.
func (r *LocalRuntime) runAdapter(ctx context.Context, adapter harness.HarnessAdapter, req harness.SubSessionRequest) {
    defer func() {
        if rec := recover(); rec != nil {
            req.Events.Emit(harness.RunError{
                EventMeta: harness.EventMeta{
                    SessionID: req.RunID,
                    AgentName: req.AgentName,
                    Timestamp: time.Now(),
                },
                Code:      harness.ErrCodeHarnessCrashed,
                Message:   fmt.Sprintf("adapter panic: %v", rec),
                Retryable: false,
                Cause:     string(debug.Stack()),
            })
        }
    }()
    adapter.Run(ctx, req)
}

// runAdapterACP is the ACP equivalent. Same recovery; different dispatch.
func (r *LocalRuntime) runAdapterACP(ctx context.Context, adapter harness.ACPAdapter, req harness.SubSessionRequest, acp harness.ACPCallbacks) {
    defer func() { /* same recover-and-emit-RunError logic */ }()
    adapter.RunACP(ctx, req, acp)
}
```

Modify `runForwarding` and `runCollecting` in `pkg/runtime/agent_delegation.go` to branch on `child.HasHarness()`. Rename the existing function bodies to `runModelForwarding` / `runModelCollecting` (no logic change to the model path).

Complexity: **L**

Files to modify:
- `pkg/runtime/agent_delegation.go` — split `runForwarding` / `runCollecting`.

Files to create:
- `pkg/runtime/harness_delegation.go` — `runHarnessForwarding`, `runHarnessCollecting`, `runAdapter`, `runAdapterACP`, `translateSink`, `runtimePermissionRequester`.
- `pkg/runtime/harness_delegation_test.go` — see test list below.

Files to read:
- `pkg/runtime/agent_delegation.go` (full)
- `pkg/runtime/runtime.go` (first 300 lines)
- `pkg/runtime/event.go` (full)
- `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/fsm.go` (after P0-E)
- `pkg/team/team.go` (`Permissions()`)
- `arch-spec-v2.md` §2.5, §2.5.1, §2.9, §2.10, §4.1, §4.2, §4.4

Dependencies: P0-C, P0-D, P0-E, P0-F.

Build: `go build ./pkg/runtime/...`
Test: `go test ./pkg/runtime/...`. Required cases using `pkg/harness/fake` as the adapter:

1. **Happy path:** scripted `RunStart → TextDelta → TextEnd → RunEnd` produces `StreamStartedEvent`, `MessageAddedEvent` with the right content, `SubSessionCompletedEvent`, `StreamStoppedEvent`. Parent `HarnessSession["agent"]` updated from `RunEnd.SessionToken`.
2. **RunError path:** scripted `RunStart → RunError{rate_limited}` produces `ErrorEvent` with mapped code; runtime returns `tools.ResultError`.
3. **FSM rejection:** broken sequence (`RunStart` then `TextEnd` without `TextStart`) is rejected by the FSM enforcer.
4. **FR-NEW-10 panic recovery:** fake adapter is configured to `panic("test panic")` from `Run`. The runtime's `runAdapter` recovers, emits `RunError{Code: harness_crashed, Message: "adapter panic: test panic"}`, the translator turns that into `ErrorEvent`, and `runHarnessForwarding` returns `tools.ResultError` carrying the `harness_crashed` code. The parent session receives it as a tool failure. The orchestrator process does NOT crash. Asserted via `assert.NotPanics(t, func() { runHarnessForwarding(...) })`.
5. **ACP dispatch:** when the adapter implements `ACPAdapter`, `runAdapterACP` is called with non-nil `ACPCallbacks`; when it does not, `runAdapter` is called and `RunACP` is never invoked. Verified by a fake adapter that records which method was called.
6. **bgAgents wiring (FR-NEW-9):** drive two harness subagents in parallel from one orchestrator turn via the existing bgAgents handler. Assert no event interleaving across `SessionID`s and both complete cleanly.

---

### Unit P1-B — Hooks + telemetry integration

Description: Wire `on_agent_switch` and `subagent_stop` hooks on the harness path (FR-NEW-1). Wire telemetry methods `RecordHarnessStart`, `RecordHarnessFinish`, `RecordHarnessEvent` (FR-NEW-3). Wire OTel attributes (FR-NEW-4). Confirm `pre_tool_use` and `before_llm_call` are NOT fired on the harness path.

Complexity: **M**

Files to modify:
- `pkg/runtime/harness_delegation.go` — add hook calls and telemetry instrumentation.
- `pkg/runtime/telemetry.go` — add the three `RecordHarness*` methods to the `Telemetry` interface and `defaultTelemetry`.

Files to read:
- `pkg/runtime/telemetry.go`
- `pkg/runtime/hooks.go`
- `pkg/runtime/agent_delegation.go` (`executeSubagentStopHooks` pattern)
- `arch-spec-v2.md` §2.9, §2.10

Dependencies: P1-A.

Build: `go build ./pkg/runtime/...`
Test: `go test ./pkg/runtime/...`. Fake telemetry recorder; assert three methods fired with right args. Fake hooks executor; assert `subagent_stop` fired and `pre_tool_use` did NOT.

---

### Unit P1-C — Session token ownership guard (NEW in v2)

Description: Enforce FR-NEW-11. The runtime acquires a token before dispatching an adapter; releases it on terminal event; rejects the second concurrent acquisition with `RunError{capability_mismatch}`.

In `pkg/runtime/harness_delegation.go`, before the `go r.runAdapter(...)` dispatch:

```go
if !harness.AcquireToken(adapterName, req.ResumeToken) {
    // Same as if the adapter emitted RunError directly.
    req.Events.Emit(harness.RunError{
        EventMeta: harness.EventMeta{
            SessionID: req.RunID,
            AgentName: req.AgentName,
            Timestamp: time.Now(),
        },
        Code:      harness.ErrCodeCapabilityMismatch,
        Message:   "session token already in use",
        Retryable: false,
    })
    // The FSM enforcer + translator handle the rest. Wait for the RunError to
    // flow through and return tools.ResultError.
    return /* error result */
}
defer harness.ReleaseToken(adapterName, req.ResumeToken)
```

`AcquireToken("", "")` always succeeds (empty token = fresh session, no contention possible). The defer pairs with the acquisition; release happens before the function returns regardless of how the adapter terminated.

Complexity: **S**

Files to modify:
- `pkg/runtime/harness_delegation.go` — add the acquire/release around the dispatch.

Files to create:
- `pkg/runtime/harness_token_test.go` — concurrent dispatch test.

Files to read:
- `pkg/harness/registry.go` (for `AcquireToken`/`ReleaseToken` after P0-E)
- `arch-spec-v2.md` §3.11

Dependencies: P1-A.

Build: `go build ./pkg/runtime/...`
Test: `go test -race ./pkg/runtime/...`. Required case:
- Spawn two `runHarnessForwarding` calls concurrently with the same parent session, same agent name, same `ResumeToken == "abc-123"`. Assert one succeeds (emits `SubSessionCompletedEvent`) and one fails with `ErrorEvent{Code: capability_mismatch, Message: "session token already in use"}`. The order of which succeeds is nondeterministic; the test asserts exactly-one-each.
- With empty token (fresh sessions), two concurrent calls both succeed.

---

### Unit P1-D — Claude Code adapter (renamed from v1 P1-C)

Description: Implement `pkg/harness/claude/`. Full adapter per PRD §7.1: invocation flags, stream-json NDJSON parser, event mapping table, error mapping table, native multi-turn via `--resume <session-id>`. Implements `HarnessAdapter` (not `ACPAdapter` — stream protocol). Adapter checks `req.ResumeToken` first; if non-empty, passes to `claude --resume`. If empty and `req.SimulatedHistory` non-empty, this adapter logs a warning and proceeds without prepending (Claude Code uses its own context window; simulated history is not how this adapter operates — but the contract says we must check). Document this in `adapter.go`.

Ship fixtures in `pkg/harness/claude/testdata/`: single-message run, multi-tool-call run, mid-stream error, cancellation, multi-turn resume, heartbeat tick.

Create `pkg/harness/all/all.go` containing only blank imports (`_ "github.com/docker/docker-agent/pkg/harness/claude"`).

Complexity: **L**

Files to create:
- `pkg/harness/claude/adapter.go` — implements `HarnessAdapter` (not `ACPAdapter`).
- `pkg/harness/claude/parser.go` — NDJSON parser.
- `pkg/harness/claude/config.go` — typed config struct; `init()` registers via `harness.RegisterConfig`.
- `pkg/harness/claude/process.go` — process lifecycle: spawn, stderr-to-log-file, Cancel/SIGTERM/SIGKILL teardown (FR-13), version check (FR-13).
- `pkg/harness/claude/testdata/single_message.jsonl`
- `pkg/harness/claude/testdata/multi_tool_call.jsonl`
- `pkg/harness/claude/testdata/error_mid_stream.jsonl`
- `pkg/harness/claude/testdata/cancellation.jsonl`
- `pkg/harness/claude/testdata/resume.jsonl`
- `pkg/harness/claude/testdata/heartbeat.jsonl`
- `pkg/harness/claude/adapter_test.go` — replay each fixture, assert canonical event sequence.
- `pkg/harness/all/all.go` — blank-import `claude`.

Files to read:
- `prd-v2.md` §7.1
- `pkg/harness/harness.go`, `pkg/harness/event.go`, `pkg/harness/registry.go` (after P0-E)
- `pkg/harness/replay/replay.go`, `pkg/harness/replay/record.go` (after P0-E)
- `arch-spec-v2.md` §3.1, §3.3, §3.4, §6.5

Dependencies: P0-E, P1-A.

Build: `go build ./pkg/harness/claude/...`
Test: `go test ./pkg/harness/claude/...`. No real binary needed; fixtures are the contract. Optional integration test gated by `//go:build integration_harness`.

**Sequencing note on `pkg/harness/all/all.go`:** P1-D creates the file with the `claude` import. Subsequent adapters (P2-A, P2-B, P2-C, P3-A) append one line each; coordinate via PR rebase. To avoid the rebase coupling, each adapter MAY instead create its own per-adapter init file (`pkg/harness/all/claude.go`, `pkg/harness/all/codex.go`, etc.) — recommended (DX review S8).

---

## Phase 2 — Parallel adapter build

Three engineers in parallel. P2-A, P2-B, P2-C, P2-D touch disjoint subdirectories.

### Unit P2-A — Codex adapter

Description: Implement `pkg/harness/codex/` per PRD §7.2. Implements `HarnessAdapter` (stream protocol). Simulated multi-turn via prompt prepending — checks `req.ResumeToken` first (always empty for Codex; logged), then prepends `req.SimulatedHistory` until the token budget (default 50%, configurable via `multi_turn_budget_tokens`). Emits a `Warning`-equivalent at 60% (informational `ToolCallEnd` with `name: "_warning"` or a dedicated mechanism pinned during impl) and `RunError{context_exhausted}` at 100% (FR-25).

Complexity: **L**

Files to create:
- `pkg/harness/codex/adapter.go`
- `pkg/harness/codex/parser.go`
- `pkg/harness/codex/config.go` (`model`, `reasoning_effort`, `multi_turn_budget_tokens`)
- `pkg/harness/codex/multiturn.go` — `Prepend(history []chat.Message, task string, budgetTokens int) (prompt string, warnAt60 bool, errAt100 bool)`
- `pkg/harness/codex/process.go`
- `pkg/harness/codex/testdata/*.jsonl` (six fixtures, mirroring Claude's set, including a budget-overflow fixture)
- `pkg/harness/codex/adapter_test.go`
- `pkg/harness/codex/multiturn_test.go` — table-driven cases for the budget warning/error transitions.

Update:
- `pkg/harness/all/all.go` — blank-import `pkg/harness/codex` (or create `pkg/harness/all/codex.go`).

Files to read:
- `prd-v2.md` §7.2
- `pkg/harness/claude/adapter.go` (after P1-D — pattern reference, do not import)
- `arch-spec-v2.md` §3.3 (ResumeToken vs SimulatedHistory rule), §3.4

Dependencies: P1-A, P1-D. Disjoint from P2-B, P2-C, P2-D at the file level.

Build: `go build ./pkg/harness/codex/...`
Test: `go test ./pkg/harness/codex/...`. Required cases include: fresh session (empty SimulatedHistory) produces no prepend; non-empty SimulatedHistory under budget prepends correctly; at 60% budget emits warning; at 100% budget emits `RunError{context_exhausted}`; non-empty `ResumeToken` is logged-and-ignored (no native resume for Codex).

---

### Unit P2-B — OpenCode CLI adapter (NOW with multi-turn module)

Description: Implement `pkg/harness/opencode/` per PRD §7.3. Implements `HarnessAdapter` (stream protocol). **Includes its own `multiturn.go`** with the same simulated-history prepend logic as Codex (FR-25 OpenCode half — closes the v1 consistency gap).

The multi-turn module duplicates Codex's logic (intentional, per arch-spec-v2 §2.1). Both adapters apply: check `ResumeToken` first (always empty for OpenCode), then prepend `SimulatedHistory` against the budget, emit warning at 60%, `RunError{context_exhausted}` at 100%. If during implementation a shared `pkg/harness/internal/multiturn/` package is preferable, that refactor is allowed; the v2 spec admits both topologies. Default to per-adapter `multiturn.go` for clean ownership boundaries.

Complexity: **M**

Files to create:
- `pkg/harness/opencode/adapter.go`
- `pkg/harness/opencode/parser.go`
- `pkg/harness/opencode/config.go` (`task_prefix`)
- `pkg/harness/opencode/multiturn.go` — same shape as `pkg/harness/codex/multiturn.go`.
- `pkg/harness/opencode/process.go`
- `pkg/harness/opencode/testdata/*.jsonl` (six fixtures, including a budget-overflow fixture)
- `pkg/harness/opencode/adapter_test.go`
- `pkg/harness/opencode/multiturn_test.go`

Update:
- `pkg/harness/all/all.go` — blank-import `pkg/harness/opencode` (or `pkg/harness/all/opencode.go`).

Files to read:
- `prd-v2.md` §7.3
- `pkg/harness/codex/parser.go`, `pkg/harness/codex/multiturn.go` (after P2-A — reference)
- `arch-spec-v2.md` §3.3, §3.4

Dependencies: P1-A. Disjoint from P2-A at the file level.

Build: `go build ./pkg/harness/opencode/...`
Test: `go test ./pkg/harness/opencode/...`. Same multi-turn test matrix as P2-A (under-budget, 60% warning, 100% error).

---

### Unit P2-C — ACP base + Copilot adapter

Description: Implement `pkg/harness/acp/base.go` (shared client adapter per arch spec §5.3). Implement `pkg/harness/acp/copilot/` on top per PRD §7.4. Copilot's adapter implements `ACPAdapter` (returns `ProtocolACP` from Capabilities; the runtime detects via type assertion).

The base provides a helper `func RunACPBase(ctx, req, acp, opts) { ... }` that the Copilot adapter's `RunACP` method delegates to with adapter-specific opts (binary name, env, idle timeout).

Includes: `NewClientSideConnection` wiring, ACP method handlers (`ReadTextFile`, `WriteTextFile`, terminal/* via `acp.ToolExecutor`, `RequestPermission` via `acp.Permission`), session update → canonical event translation, capability negotiation (FR-NEW-8), Cancel → SIGTERM → SIGKILL teardown (FR-13), process pool keyed by `(agent_name, working_dir)` for NFR-11.

Complexity: **L**

Files to create:
- `pkg/harness/acp/base.go` — shared client adapter; exposes `RunACPBase`.
- `pkg/harness/acp/capabilities.go`
- `pkg/harness/acp/pool.go`
- `pkg/harness/acp/translate.go` — `SessionUpdate` → canonical event.
- `pkg/harness/acp/process.go`
- `pkg/harness/acp/copilot/adapter.go` — implements `ACPAdapter`.
- `pkg/harness/acp/copilot/config.go`
- `pkg/harness/acp/copilot/testdata/*.jsonl`
- `pkg/harness/acp/copilot/adapter_test.go`
- `pkg/harness/acp/base_test.go`

Update:
- `pkg/harness/all/all.go` (or `pkg/harness/all/copilot.go`)

Files to read:
- `prd-v2.md` §7.4
- `pkg/acp/agent.go`, `pkg/acp/run.go` (existing server-side; pattern reference only — do NOT import)
- `~/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.0/client.go` and `client_gen.go`
- `arch-spec-v2.md` §3.1 (ACPAdapter interface), §3.7, §3.8, §5.3, §5.5, §6.1, §6.2

Dependencies: P1-A. Disjoint from P2-A and P2-B at the file level.

Build: `go build ./pkg/harness/acp/...`
Test: `go test ./pkg/harness/acp/...`. Required cases include: ACPAdapter interface assertion at registry registration (`Capabilities().Protocol == ProtocolACP`); `RunACP` is invoked with non-nil `ACPCallbacks`; permission flow round-trips through `acp.Permission.Request`; capability mismatch on session start emits `RunError{capability_mismatch}`.

---

### Unit P2-D — Sandbox hardening

Description: Promote `pkg/harness/sandbox/` from P0-E stubs into a hardened implementation. Symlink-safe path resolution, `..` rejection, env filtering, terminal `cd`-escape detection (FR-39 best-effort).

**Scope clarification.** P0-E ships real (not stub) implementations of `Resolve`, `Filter`, and `GuardTerminalCommand` so P1-A and the FSM enforcer can use them. P2-D's job is the **hardening pass**: hostile-path test corpus, fuzzing, symlink-chain coverage, environment-edge cases. P2-D MUST land before P2-C ships ACP traffic to real filesystem operations.

Complexity: **M**

Files to modify:
- `pkg/harness/sandbox/sandbox.go` — harden `Resolve`.
- `pkg/harness/sandbox/env.go` — harden `Filter`.
- `pkg/harness/sandbox/terminal.go` — harden `GuardTerminalCommand`.

Files to create:
- `pkg/harness/sandbox/sandbox_test.go` — hostile fixtures.
- `pkg/harness/sandbox/sandbox_fuzz_test.go` — fuzz target on `Resolve`.

Files to read:
- `pkg/harness/sandbox/*` (after P0-E)
- `arch-spec-v2.md` §3.7, §6.1
- `prd-v2.md` FR-38–FR-41

Dependencies: P0-E.

Build: `go build ./pkg/harness/sandbox/...`
Test: `go test ./pkg/harness/sandbox/...`. Must include 1000-iteration fuzz on `Resolve` with random path segments.

---

## Phase 3 — OpenClaw + hardening

### Unit P3-A — OpenClaw adapter

Description: Implement `pkg/harness/acp/openclaw/` per PRD §7.5. Clone of Copilot adapter with different binary, no `GITHUB_TOKEN`, `IdleTimeout: 2m`. Implements `ACPAdapter`. Reuses `pkg/harness/acp/base.go` verbatim.

Complexity: **M**

Files to create:
- `pkg/harness/acp/openclaw/adapter.go` — implements `ACPAdapter`.
- `pkg/harness/acp/openclaw/config.go`
- `pkg/harness/acp/openclaw/testdata/*.jsonl`
- `pkg/harness/acp/openclaw/adapter_test.go`

Update:
- `pkg/harness/all/all.go` (or `pkg/harness/all/openclaw.go`)

Files to read:
- `prd-v2.md` §7.5
- `pkg/harness/acp/copilot/adapter.go`, `pkg/harness/acp/base.go` (after P2-C)

Dependencies: P2-C.

Build: `go build ./pkg/harness/acp/openclaw/...`
Test: `go test ./pkg/harness/acp/openclaw/...`

---

### Unit P3-B — Conformance suite + 20 scenarios

Description: Build `pkg/harness/conformance/` with the 20 canonical scenarios (PRD §9.4).

Complexity: **L**

Files to create:
- `pkg/harness/conformance/scenarios.go`
- `pkg/harness/conformance/runner.go`
- `pkg/harness/conformance/scenarios_test.go`

Files to read:
- `prd-v2.md` §9.4
- Each adapter's `testdata/` (after P1-D, P2-A, P2-B, P2-C, P3-A)
- `arch-spec-v2.md` §3.4

Dependencies: P1-D, P2-A, P2-B, P2-C, P3-A.

Build: `go build ./pkg/harness/conformance/...`
Test: `go test ./pkg/harness/conformance/...` — all 5 adapters green across all 20 scenarios.

---

### Unit P3-C — Goleak + process-orphan tests

Description: Verify FR-13, NFR-5, NFR-6 via `goleak` integration tests and a process-orphan counter. 1000 consecutive runs in CI.

Complexity: **M**

Files to create:
- `pkg/harness/lifecycle/lifecycle_test.go` — uses `goleak.VerifyTestMain`. Spawns each adapter 1000 times sequentially with a fake harness binary (shell script).
- `pkg/harness/lifecycle/testdata/fake-claude.sh` (one per adapter type)

Files to read:
- Each adapter's `process.go`
- `prd-v2.md` FR-13, NFR-5, NFR-6
- `arch-spec-v2.md` §6.6

Dependencies: P1-D, P2-A, P2-B, P2-C, P3-A.

Build: `go build ./pkg/harness/lifecycle/...`
Test: `go test -run TestLifecycle ./pkg/harness/lifecycle/...` — must run 1000 iterations cleanly.

---

### Unit P3-D — CLI subcommands: `harness describe`, `harness trace`, `harness lint`

Description: Three CLI surfaces from PRD §6.4.

Complexity: **M**

Files to create:
- `cmd/docker-agent/cmd_harness.go`
- `cmd/docker-agent/cmd_harness_describe.go`
- `cmd/docker-agent/cmd_harness_trace.go`
- `cmd/docker-agent/cmd_harness_lint.go`

Files to read:
- `cmd/docker-agent/main.go`
- `prd-v2.md` §6.4
- `pkg/harness/harness.go`, `pkg/harness/fsm.go`, `pkg/harness/registry.go`

Dependencies: P0-E (FSM logic), P3-A (all adapters registered).

Build: `go build ./cmd/docker-agent/...`
Test: `go test ./cmd/docker-agent/...`

---

## Phase 4 — Dogfood + GA (out of scope per PRD §10)

---

## Execution Order

### Phase 0, Step 1 (sequential): P0-A — config v9 snapshot

### Phase 0, Step 2 (parallel group A):
- P0-B (config schema + version bump + FR-NEW-5 ValidateSkillTarget)
- P0-C (agent harness field + opts)
- P0-D (session HarnessSession field)
- P0-G (CI prerequisite)

Disjoint at file level. P0-A must be complete.

### Phase 0, Step 3 (sequential): P0-E — harness package skeleton (with record.go + token guard)
Depends on P0-C.

### Phase 0, Step 4 (sequential): P0-F — teamloader harness branch (with unknown-key error format)
Depends on P0-B, P0-C, P0-E.

### Phase 1, Step 1 (sequential): P1-A — runtime translator + branch + panic recovery
Depends on P0-C, P0-D, P0-E, P0-F.

### Phase 1, Step 2 (sequential): P1-B — hooks + telemetry
Depends on P1-A.

### Phase 1, Step 3 (sequential): P1-C — session token ownership guard
Depends on P1-A. Can run in parallel with P1-B (different files: P1-B touches telemetry; P1-C adds acquire/release plumbing to harness_delegation.go — sequence after P1-B for clean diffs).

### Phase 1, Step 4 (sequential): P1-D — Claude Code adapter
Depends on P0-E, P1-A. Can run in parallel with P1-B and P1-C.

### Phase 2 (parallel group B):
- P2-A — Codex adapter
- P2-B — OpenCode CLI adapter (with multiturn.go)
- P2-C — ACP base + Copilot adapter
- P2-D — Sandbox hardening

Depends on P1-A, P1-D. Disjoint subdirectories. Shared `pkg/harness/all/all.go` resolved by per-adapter init files (recommended) or PR rebase.

### Phase 3, Step 1 (sequential): P3-A — OpenClaw adapter
Depends on P2-C.

### Phase 3, Step 2 (parallel group C):
- P3-B — Conformance suite
- P3-C — Goleak + process-orphan tests
- P3-D — CLI subcommands

Depends on P3-A. Disjoint.

---

## Cross-check: parallel-group disjointness

**Phase 0, Step 2 (A):**
- P0-B → `pkg/config/latest/`, `pkg/config/upgrade.go`, `pkg/agent/validate.go`
- P0-C → `pkg/agent/agent.go`, `pkg/agent/opts.go`, `pkg/agent/harness_spec.go`
- P0-D → `pkg/session/session.go`
- P0-G → none

P0-B and P0-C both touch `pkg/agent/`. P0-B creates/modifies `validate.go`; P0-C modifies `agent.go`, `opts.go` and creates `harness_spec.go`. **Disjoint files within `pkg/agent/`**, but the unit boundary requires both engineers to coordinate at PR-merge time on `pkg/agent/` import graph. Recommend P0-C lands first (creates `harness_spec.go`), then P0-B (which can reference `*HarnessSpec` in `ValidateSkillTarget`'s type signature if needed — though it does not, since validation uses `HasHarness()`).

**Phase 2 (B):**
- P2-A → `pkg/harness/codex/` + 1-line in `pkg/harness/all/`
- P2-B → `pkg/harness/opencode/` + 1-line in `pkg/harness/all/`
- P2-C → `pkg/harness/acp/` (excluding `openclaw/`) + 1-line in `pkg/harness/all/`
- P2-D → `pkg/harness/sandbox/`

Disjoint. `pkg/harness/all/` collision avoided by per-adapter init files (DX review S8).

**Phase 3, Step 2 (C):**
- P3-B → `pkg/harness/conformance/`
- P3-C → `pkg/harness/lifecycle/`
- P3-D → `cmd/docker-agent/cmd_harness*.go`

Disjoint.

---

## Total unit count

- Phase 0: 7 units (P0-A through P0-G)
- Phase 1: 4 units (P1-A, P1-B, P1-C, P1-D) — was 3 in v1; new P1-C for FR-NEW-11 token guard
- Phase 2: 4 units (P2-A through P2-D)
- Phase 3: 4 units (P3-A through P3-D)
- **Total: 19 implementation units**

Estimated calendar with 3 engineers: 6–7 weeks. P1-C is small (S) and can ride alongside P1-B / P1-D without affecting the critical path.

---

## Coverage trace (FR → unit)

| FR | Unit(s) |
|---|---|
| FR-1 (model/harness mutually exclusive) | P0-B |
| FR-2 (Type enum) | P0-B |
| FR-3 (harness field on Agent) | P0-C |
| FR-4 (PATH check) | P0-F |
| FR-5 (unknown-key rejection, exact format) | P0-B (format spec), P0-F (enforcement) |
| FR-6 (config version bump) | P0-A, P0-B |
| FR-7 (permission-policy cross-field) | P0-B |
| FR-8 (working_dir resolution) | P0-F |
| FR-9, FR-10, FR-11 (registry, Capabilities purity, no return error) | P0-E |
| FR-12 (process-per-session) | per adapter (P1-D, P2-A, P2-B, P2-C, P3-A) |
| FR-13 (cleanup order) | per adapter, hardened in P3-C |
| FR-14 (per-adapter binary version) | per adapter |
| FR-15–FR-18 (event types, FSM) | P0-E |
| FR-19 (single-frame TextStart/End) | P2-A |
| FR-20 (heartbeat) | P0-E |
| FR-21 (canonical → runtime translation) | P1-A |
| FR-22 (conformance) | P3-B |
| FR-23 (raw frame opt-in) | P0-E |
| FR-25 (simulated multi-turn budgets) | P2-A (Codex), P2-B (OpenCode) |
| FR-26 (HarnessSession persistence) | P0-D, P1-A |
| FR-29 (timeout) | P0-E, P1-A |
| FR-32 (OTel `runtime.harness_session`) | P1-B |
| FR-33 (PermissionRequester) | P0-E (interface), P2-C (ACP implementation) |
| FR-34 (TUI reuse via ToolCallConfirmationEvent) | P1-A |
| FR-37 (30s permission timeout) | P2-C |
| FR-38, FR-39, FR-40, FR-41 (sandbox) | P0-E (stubs+real-Resolve), P2-D (hardening) |
| FR-NEW-1 (hooks) | P1-B |
| FR-NEW-3 (telemetry) | P1-B |
| FR-NEW-4 (OTel attrs) | P1-B |
| FR-NEW-5 (run_skill rejection) | P0-B (ValidateSkillTarget + runtime call-site) |
| FR-NEW-8 (ACP capability negotiation) | P2-C |
| FR-NEW-9 (bgAgents wiring) | P1-A test |
| FR-NEW-10 (Run void; panic recovery) | P0-E (interface shape), P1-A (recover wrapper + test) |
| FR-NEW-11 (session token ownership) | P0-E (registry guard), P1-C (runtime use + test) |
| FR-NEW-12 (CI provisioning) | P0-G |
| FR-NEW-13 (replay + record) | P0-E |

All FRs covered. All 6 consistency-check gaps closed. All 5 DX-review blockers addressed.
