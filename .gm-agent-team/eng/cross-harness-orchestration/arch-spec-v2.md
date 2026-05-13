# Architecture Spec v2: Cross-Harness Orchestration

**Owner:** docker-agent eng
**Status:** APPROVED FOR IMPLEMENTATION (revised post-review)
**Source PRD:** `prd-v2.md`
**Supersedes:** `arch-spec.md` (v1)
**Insertion points:** `pkg/runtime/agent_delegation.go` (`runForwarding` line 248, `runCollecting` line 310)

**Revision summary (v1 → v2).** Five blocking fixes from `dx-review-arch.md` and six coverage gaps from `consistency-check.md`. Specifically:

1. Translator location is now unambiguous: `pkg/runtime/harness_delegation.go`. `pkg/harness/translate.go` removed from §2.1 entirely. (Fix 1)
2. `Run` returns void. All terminal states flow through events. Runtime wraps the adapter call with `recover()` that converts panics to `RunError{Code: ErrCodeHarnessCrashed}`. (Fix 2)
3. ACP separation moved into the type system via two interfaces: `HarnessAdapter` (base) and `ACPAdapter` (additional). Non-ACP adapters can no longer be passed `ACPCallbacks`. (Fix 3)
4. `PriorTurns` replaced with `ResumeToken` + `SimulatedHistory`. Adapter check order is documented. (Fix 4)
5. YAML unknown-key validation uses `yaml.v3`'s `KnownFields(true)`. Exact error format pinned in §3.9. (Fix 5)
6. Plus: session-token ownership guard, `run_skill` rejection, OpenCode multi-turn module, replay recorder, and FR-NEW-10 (`Run` panic recovery) test added in impl-plan-v2.

---

## 1. Scope

This document specifies the Go-level architecture for cross-harness orchestration: package layout, exact interface signatures, data flow from orchestrator tool call to harness subprocess and back, the technology decisions that shape those signatures, and the risks tracked at the architecture level (not phase-level).

It binds the PRD's appendix A and §1.2 file list into compilable Go and incorporates the v1 review feedback.

---

## 2. Component design

### 2.1 New package: `pkg/harness/`

Directory layout (mirrors PRD §1.2 item 9, revised post-review):

```
pkg/harness/
  harness.go            // HarnessAdapter and ACPAdapter interfaces, AdapterCapabilities,
                        // SubSessionRequest, ACPCallbacks, EventSink, RawEventSink,
                        // ToolExecutor, PermissionRequester, typed enums
                        // (ProtocolClass, ErrorCode, PermissionDecision, …)
  event.go              // Discriminated-union Event interface and the 14 concrete event structs
  registry.go           // Adapter registry: Register(name, factory), Lookup(name),
                        // typed-config registration for FR-5 unknown-key rejection,
                        // tokenInUse session-token ownership guard (FR-NEW-11)
  fsm.go                // EventSink wrapper that enforces the canonical FSM (FR-17, FR-18)
  heartbeat.go          // Synthetic Heartbeat ticker for adapters without natural keepalive
  errors.go             // ErrorCode constants + helpers for building RunError events
  sandbox/
    sandbox.go          // Path resolution, sandbox root, symlink-safe containment check
    env.go              // Env allowlist (PATH, HOME, USER, LANG, LC_*, TERM, opt-in extras)
    terminal.go         // Terminal CWD guard, `cd`-out-of-root string check (FR-39)
  example/
    adapter.go          // Template adapter for new authors; pure no-op that emits a minimal lifecycle
  fake/
    adapter.go          // In-process fake adapter; takes a scripted Event sequence
  replay/
    replay.go           // PlayFixture(t, path) infrastructure (FR-NEW-13)
    record.go           // Recorder wrapping an EventSink, writes NDJSON for fixture generation
  claude/
    adapter.go          // Claude Code CLI adapter (Phase 1)
    parser.go           // stream-json NDJSON parser
    config.go           // Typed Config struct (max_turns, system_append, …)
  codex/
    adapter.go          // Codex CLI adapter (Phase 2)
    parser.go           // codex --json parser
    config.go           // Typed Config struct (model, reasoning_effort, multi_turn_budget_tokens, …)
    multiturn.go        // SimulatedHistory prepend + token-budget warning/error
  opencode/
    adapter.go          // OpenCode CLI adapter (Phase 2)
    parser.go           // opencode --format json parser
    config.go           // Typed Config struct (task_prefix, …)
    multiturn.go        // SimulatedHistory prepend (same logic as codex/multiturn.go)
  acp/
    base.go             // Shared ACP client adapter: NewClientSideConnection wiring,
                        // SessionUpdate → canonical translation, ToolExecutor binding,
                        // PermissionRequester binding, Cancel-then-SIGTERM teardown (FR-13)
    capabilities.go     // Per-session capability negotiation (FR-NEW-8)
    pool.go             // Process pool keyed by (agent_name, working_dir) for NFR-11
    copilot/
      adapter.go        // Copilot-specific invocation, env (GITHUB_TOKEN), config (Phase 2)
      config.go
    openclaw/
      adapter.go        // OpenClaw-specific invocation, env, config (Phase 3)
      config.go
```

**`translate.go` is NOT in `pkg/harness/`.** The translator lives in `pkg/runtime/harness_delegation.go` (see §4.2). This is the authoritative location. Rationale: the translator constructs `runtime.Event` values (`MessageAddedEvent`, `SubSessionCompletedEvent`) and writes to `*session.Session`. Placing it in `pkg/harness/` would create an import cycle (`pkg/harness` → `pkg/runtime` → `pkg/harness`). The one-way direction is:

```
pkg/runtime  --imports-->  pkg/harness  --imports-->  pkg/agent
pkg/harness                does NOT import pkg/runtime
pkg/harness                does NOT import pkg/session
```

**Imports:**
- `pkg/harness` is imported by `pkg/runtime` (for the discriminated-union types, FSM, registry lookup) and by `pkg/teamloader` (for adapter typed-config validation, capability lookup, binary PATH check).
- Adapter subpackages (`pkg/harness/claude/...`) are imported by the program's main entry point (`cmd/docker-agent/`) via blank imports for their `init()` registration. The runtime itself does **not** blank-import adapters; that keeps `pkg/runtime` free of vendor-specific dependencies.

### 2.2 Changes to `pkg/agent/`

**`pkg/agent/agent.go`:**

```go
type Agent struct {
    // ... existing fields ...
    harness *HarnessSpec   // nil when the agent is model-backed
}

func (a *Agent) HasHarness() bool { return a.harness != nil }
func (a *Agent) Harness() *HarnessSpec { return a.harness }
```

`HarnessSpec` is a value type that travels from teamloader → agent → runtime:

```go
// HarnessSpec is the per-agent harness configuration the runtime needs at
// dispatch time. Built by teamloader from latest.AgentConfig.Harness; opaque
// to the runtime beyond the adapter name and the resolved working dir.
type HarnessSpec struct {
    AdapterName      string                // e.g. "claude-code"
    Command          string                // optional binary path override; "" => use Capabilities().Requires.Binary
    Args             []string              // appended to adapter defaults
    Env              map[string]string     // allowlisted, merged with sandbox env
    WorkingDir       string                // resolved at load time (FR-8)
    Timeout          time.Duration         // default 5m (FR-29)
    MinVersion       string                // override Capabilities().Requires.MinVersion
    PermissionPolicy *PermissionPolicy     // ACP only; nil for non-ACP
    Config           any                   // adapter-typed config struct (post-unmarshal)
}

type PermissionPolicy struct {
    FSWrite             PermissionMode  // prompt | auto_allow | auto_deny
    Terminal            PermissionMode  // prompt | auto_allow | allow_unrestricted | auto_deny
    IUnderstandTheRisk  bool
}

type PermissionMode string
const (
    PermissionPrompt           PermissionMode = "prompt"
    PermissionAutoAllow        PermissionMode = "auto_allow"
    PermissionAllowUnrestricted PermissionMode = "allow_unrestricted"
    PermissionAutoDeny         PermissionMode = "auto_deny"
)
```

**`pkg/agent/opts.go`:**

```go
func WithHarness(spec *HarnessSpec) Opt {
    return func(a *Agent) { a.harness = spec }
}
```

### 2.3 Changes to `pkg/config/latest/`

**`pkg/config/latest/types.go`:**

```go
type AgentConfig struct {
    // ... existing fields ...
    Harness *HarnessConfig `json:"harness,omitempty" yaml:"harness,omitempty"`
}

type HarnessConfig struct {
    Type             string                 `json:"type" yaml:"type"`
    Command          string                 `json:"command,omitempty" yaml:"command,omitempty"`
    Args             []string               `json:"args,omitempty" yaml:"args,omitempty"`
    Env              map[string]string      `json:"env,omitempty" yaml:"env,omitempty"`
    WorkingDir       string                 `json:"working_dir,omitempty" yaml:"working_dir,omitempty"`
    Timeout          Duration               `json:"timeout,omitempty" yaml:"timeout,omitempty"`
    MinVersion       string                 `json:"min_version,omitempty" yaml:"min_version,omitempty"`
    PermissionPolicy *PermissionPolicyConfig `json:"permission_policy,omitempty" yaml:"permission_policy,omitempty"`
    Config           map[string]any         `json:"config,omitempty" yaml:"config,omitempty"`
}

type PermissionPolicyConfig struct {
    FSWrite             string `json:"fs_write,omitempty" yaml:"fs_write,omitempty"`
    Terminal            string `json:"terminal,omitempty" yaml:"terminal,omitempty"`
    IUnderstandTheRisk  bool   `json:"i_understand_the_risk,omitempty" yaml:"i_understand_the_risk,omitempty"`
}
```

**`pkg/config/latest/validate.go`** adds:

1. Cross-field rule on `AgentConfig`: `Model` and `Harness` are mutually exclusive; one must be present (FR-1). When `Harness != nil`, `SubAgents` and `Handoffs` MUST be empty (FR-5).
2. `Harness.Type` MUST be one of `claude-code | codex | opencode | copilot | openclaw` (FR-2).
3. `PermissionPolicy.IUnderstandTheRisk` cross-field rule (FR-7).
4. **Unknown-key rejection** for `Harness.Config` is deferred to teamloader, where the adapter's typed config struct is registered (FR-5). See §3.9 for the exact YAML API call and error format.

### 2.4 Config version bump: v9 → v10

**Strategy:** snapshot before mutate (FR-6). Unchanged from v1:

1. Copy `pkg/config/latest/` → `pkg/config/v9/`. Update its package declaration to `package v9`. `Version` stays `"9"`.
2. In `pkg/config/latest/`, bump `Version = "10"`.
3. Wire `pkg/config/upgrade/` so a v9 file upgrades to v10. The upgrade is a no-op for configs without `harness:`.
4. Existing `pkg/config/v8/` and earlier remain untouched.

### 2.5 Changes to `pkg/runtime/agent_delegation.go`

Branch on `child.HasHarness()`:

```go
func (r *LocalRuntime) runForwarding(ctx context.Context, parent *session.Session, evts EventSink, req delegationRequest) (*tools.ToolCallResult, error) {
    // ... lookup callerAgent, child ...
    if child.HasHarness() {
        return r.runHarnessForwarding(ctx, parent, evts, callerAgent, child, req)
    }
    return r.runModelForwarding(ctx, parent, evts, callerAgent, child, req)
}

func (r *LocalRuntime) runCollecting(ctx context.Context, parent *session.Session, cfg SubSessionConfig, onContent func(string)) *agenttool.RunResult {
    child, err := r.team.Agent(cfg.AgentName)
    if err != nil { return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", cfg.AgentName, err)} }
    if child.HasHarness() {
        return r.runHarnessCollecting(ctx, parent, cfg, child, onContent)
    }
    return r.runModelCollecting(ctx, parent, cfg, child, onContent)
}
```

New functions in `pkg/runtime/harness_delegation.go`:

- `runHarnessForwarding(ctx, parent, evts, callerAgent, child, req) (*tools.ToolCallResult, error)`
- `runHarnessCollecting(ctx, parent, cfg, child, onContent) *agenttool.RunResult`
- `translateSink` — canonical → runtime event translator (see §4.2)
- `runtimePermissionRequester` — implements `harness.PermissionRequester`
- `runAdapter(ctx, adapter, req, acp, isACP)` — wraps the adapter call in a goroutine with `recover()` for panic-to-`RunError` conversion (see §2.5.1)

#### 2.5.1 Adapter call wrapper (Fix 2: panic recovery)

`Run` returns void. The runtime is responsible for ensuring the adapter's goroutine never escapes uncaught. Inside `runHarnessForwarding`:

```go
// runAdapter spawns the adapter goroutine. Recovers panics into a synthetic
// RunError event so a buggy adapter cannot crash the orchestrator process.
// FR-NEW-10: terminal state always flows through events.
func (r *LocalRuntime) runAdapter(ctx context.Context, adapter harness.HarnessAdapter, req harness.SubSessionRequest, acp *harness.ACPCallbacks) {
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
    if acp != nil {
        // ACPAdapter is checked by the caller; this is the runACP path.
        adapter.(harness.ACPAdapter).RunACP(ctx, req, *acp)
        return
    }
    adapter.Run(ctx, req)
}
```

The caller dispatches:

```go
isACP := false
var acpBindings *harness.ACPCallbacks
if _, ok := adapter.(harness.ACPAdapter); ok {
    isACP = true
    acpBindings = &harness.ACPCallbacks{
        ToolExecutor: sandbox.NewToolExecutor(req.WorkingDir),
        Permission:   &runtimePermissionRequester{r, parent, child, evts},
    }
    if acpBindings.ToolExecutor == nil || acpBindings.Permission == nil {
        // Defensive: should be impossible.
        evts.Emit(/* runtime error event */)
        return tools.ResultError("internal: ACP bindings nil"), nil
    }
}
go r.runAdapter(ctx, adapter, req, acpBindings)
```

### 2.6 Changes to `pkg/session/session.go`

Add one field on `Session`:

```go
type Session struct {
    // ... existing fields ...

    // HarnessSession stores adapter-opaque resume tokens for harness-backed
    // subagents (FR-26). Key is the agent name, value is the adapter's
    // opaque session token (Claude Code session_id, Codex thread_id,
    // OpenCode session_id, ACP session ID). Serializes through the existing
    // session-store JSON; no schema migration.
    HarnessSession map[string]string `json:"harness_session,omitempty"`
}
```

Locked access via `HarnessSessionGet` / `HarnessSessionSet` pair using the existing `Session.mu`.

### 2.7 Changes to `pkg/teamloader/teamloader.go`

In the agent-build loop (around line 146):

1. If `agentConfig.Harness != nil`:
   - Look up the adapter via `harness.LookupAdapter(agentConfig.Harness.Type)`. Unknown type → error.
   - Unmarshal `agentConfig.Harness.Config` (raw `map[string]any`) into the adapter's typed config struct using `yaml.v3` decoder with `KnownFields(true)`. Unknown keys → load-time error in the format spec'd in §3.9 (FR-5).
   - Build `*agent.HarnessSpec`. Resolve `WorkingDir` per FR-8.
   - PATH-check the binary.
   - Construct the agent with `agent.WithHarness(spec)`. Skip model and toolset construction.
2. Else: existing model-backed construction.

**FR-NEW-5 enforcement: `run_skill` rejection.** In `pkg/agent/validate.go` (or equivalent), add a check:

```go
// ValidateSkillTarget asserts the agent is eligible to be a run_skill target.
// Harness-backed agents cannot be used as skill targets in v1: skill prompts
// would be silently dropped because the harness owns its own system-prompt
// composition.
func (a *Agent) ValidateSkillTarget() error {
    if a.HasHarness() {
        return fmt.Errorf("agent %q has harness=%s; harness-backed agents cannot be used as skill targets in v1",
            a.name, a.harness.AdapterName)
    }
    return nil
}
```

Called from the `run_skill` tool's target-resolution path (`pkg/runtime/loop.go` or wherever the tool dispatches).

### 2.8 Changes to `pkg/runtime/loop.go`

**No changes for `transfer_task`.** `run_skill` invokes `agent.ValidateSkillTarget()` before dispatching (FR-NEW-5).

### 2.9 Hooks integration

In `runHarnessForwarding` / `runHarnessCollecting`:

- `on_agent_switch` fires via the existing `r.executeOnAgentSwitchHooks` call inside `r.swapCurrentAgent` when `SwitchCurrentAgent` is true.
- `subagent_stop` fires via the same `defer r.executeSubagentStopHooks` pattern used by `runModelForwarding`.
- `pre_tool_use` and `before_llm_call` are intentionally **not** invoked on the harness path.

### 2.10 Telemetry and OTel

- OTel span `runtime.harness_session` opened at the top of `runHarnessForwarding` / `runHarnessCollecting` with attributes `harness.type`, `agent.name`, `working_dir`, `resume` (bool), `session.id`. (FR-NEW-4)
- New `Telemetry` methods: `RecordHarnessStart(harnessType, agentName)`, `RecordHarnessFinish(harnessType, agentName, code ErrorCode, durationMs)`, `RecordHarnessEvent(harnessType, eventKind, latencyMs)`. (FR-NEW-3)

---

## 3. Interface definitions

All types live in `pkg/harness/`. Public.

### 3.1 HarnessAdapter and ACPAdapter (Fix 3: ACP separation in the type system)

```go
package harness

import (
    "context"
)

// HarnessAdapter is the base contract every adapter implements. Implementations
// are stateless and safe for concurrent use; per-session state lives on the
// goroutine running Run. Process-per-session is mandatory (FR-12).
//
// Run returns void. ALL terminal states (success, error, crash) flow through
// events on req.Events. The runtime wraps each Run call in a goroutine with
// recover() that converts panics to RunError{Code: ErrCodeHarnessCrashed}.
// Adapter authors MUST NOT return errors from Run; that path does not exist.
// FR-NEW-10.
type HarnessAdapter interface {
    // Name returns the stable adapter identifier (e.g. "claude-code"). Used
    // as the registry key and as the canonical value of HarnessConfig.Type.
    Name() string

    // Capabilities returns the adapter's static support surface. Pure
    // function: no I/O, no process spawn, safe to call at config-load time
    // (FR-10).
    Capabilities() AdapterCapabilities

    // Run drives a single non-ACP harness session to terminal state. The
    // adapter MUST emit exactly one terminal event (RunEnd or RunError) on
    // req.Events before returning. Run MUST NOT panic (the runtime recovers
    // anyway, but adapters should emit RunError themselves with a precise
    // code).
    Run(ctx context.Context, req SubSessionRequest)
}

// ACPAdapter is implemented by adapters whose protocol is ProtocolACP.
// The runtime checks for this interface via type assertion and dispatches
// RunACP instead of Run for ACP adapters. The runtime constructs and
// verifies non-nil ACPCallbacks before calling RunACP; the adapter can
// rely on both ToolExecutor and Permission being non-nil.
//
// Non-ACP adapters MUST NOT implement ACPAdapter. Adapters that implement
// ACPAdapter MUST return ProtocolACP from Capabilities().Protocol; the
// runtime asserts this at registry registration time.
type ACPAdapter interface {
    HarnessAdapter
    RunACP(ctx context.Context, req SubSessionRequest, acp ACPCallbacks)
}
```

The runtime's dispatch logic:

```go
adapter := harness.LookupAdapter(child.Harness().AdapterName)
if acp, ok := adapter.(harness.ACPAdapter); ok {
    bindings := harness.ACPCallbacks{
        ToolExecutor: sandbox.NewToolExecutor(req.WorkingDir),
        Permission:   &runtimePermissionRequester{...},
    }
    // Defensive nil check; runtime always constructs both, but assert.
    if bindings.ToolExecutor == nil || bindings.Permission == nil {
        panic("runtime: ACPCallbacks nil after construction")
    }
    go r.runAdapterACP(ctx, acp, req, bindings)
} else {
    go r.runAdapter(ctx, adapter, req)
}
```

### 3.2 AdapterCapabilities

```go
type AdapterCapabilities struct {
    Protocol     ProtocolClass     // ProtocolStream | ProtocolACP
    Requires     HostRequirements
    Features     AdapterFeatures
    IdleTimeout  time.Duration
}

type HostRequirements struct {
    Binary       string
    MinVersion   string
    EnvVars      []string
    InstallHint  string
}

type AdapterFeatures struct {
    SupportsMultiTurn           bool
    SupportsPerCallSystemPrompt bool
    StreamsTextDeltas           bool
    StreamsReasoning            bool
}

type ProtocolClass string
const (
    ProtocolStream ProtocolClass = "stream"
    ProtocolACP    ProtocolClass = "acp"
)
```

`BuiltInTools []string` dropped from v1 per DX review §4.2 (`S6`).

### 3.3 SubSessionRequest (Fix 4: ResumeToken + SimulatedHistory split)

```go
import (
    "encoding/json"
    "log/slog"
    "time"

    "github.com/docker/docker-agent/pkg/chat"
)

// SubSessionRequest is what the runtime passes to every adapter. ACP adapters
// receive ACPCallbacks separately via RunACP; non-ACP adapters never see
// callbacks.
type SubSessionRequest struct {
    RunID            string             // sub-session ID (for event attribution)
    ParentID         string             // parent session ID
    AgentName        string             // child agent name

    SystemPrompt     string             // optional; adapter may ignore if SupportsPerCallSystemPrompt=false
    Task             string             // primary task description

    // ResumeToken is set when the runtime wants the adapter to resume a
    // previous session via native multi-turn (Claude Code session_id,
    // Codex thread_id, OpenCode session_id, ACP session ID). It is
    // adapter-opaque; the adapter wrote it on a previous RunEnd and the
    // runtime stored it in parent.HarnessSession[agent_name].
    //
    // Non-empty ResumeToken means "resume". The adapter MUST use it and
    // MUST ignore SimulatedHistory.
    ResumeToken      string

    // SimulatedHistory carries prior conversation turns for adapters that do
    // not support native resume (Codex, OpenCode CLI). The adapter prepends
    // these to the prompt up to a token budget.
    //
    // Non-empty SimulatedHistory means "fresh session, but seed context."
    // SimulatedHistory is non-empty ONLY when ResumeToken == "".
    SimulatedHistory []chat.Message

    WorkingDir       string             // sandbox root (FR-38)
    Env              map[string]string  // post-allowlist (FR-41)

    // Config is the adapter-specific typed config struct, unmarshaled by
    // teamloader from HarnessConfig.Config (via KnownFields(true)). The
    // adapter type-asserts to its own Config type.
    Config           json.RawMessage

    Timeout          time.Duration      // wall-clock timeout for Run (FR-29)

    Logger           *slog.Logger       // adapter logger; writes to harness-<n>.adapter.log
    Events           EventSink          // FSM-enforced canonical event sink (required, non-nil)
    RawSink          RawEventSink       // optional; nil disables raw frame forwarding (FR-23)
}

// ACPCallbacks is passed to ACP adapters as a separate parameter on RunACP.
// Non-ACP adapters never see ACPCallbacks at all (Fix 3: ACP separation in
// the type system).
//
// Both fields are non-nil when the struct is constructed by the runtime.
// Adapters can rely on this contract.
type ACPCallbacks struct {
    ToolExecutor ToolExecutor
    Permission   PermissionRequester
}
```

**Adapter rule for resume vs simulated history (Fix 4):**

> 1. Check `ResumeToken` first. If non-empty, use native resume (the harness's session-resume mechanism). Ignore `SimulatedHistory`.
> 2. If `ResumeToken` is empty and `SimulatedHistory` is non-empty, prepend it to the prompt as the adapter's documented serialization (e.g., user/assistant role markers).
> 3. If both are empty, run a fresh session with no prior context.

Runtime invariant (enforced at request construction): at most one of `ResumeToken` and `SimulatedHistory` is non-empty.

### 3.4 Event — discriminated union

Unchanged from v1 (§3.4 of arch-spec.md). 14 concrete event types embedding `EventMeta`:

- Lifecycle (3): `RunStart`, `RunEnd`, `RunError`
- Text (3): `TextStart`, `TextDelta`, `TextEnd`
- Reasoning (3): `ReasoningStart`, `ReasoningDelta`, `ReasoningEnd`
- Tool (2): `ToolCallStart`, `ToolCallEnd`
- Permission (2): `PermissionPending`, `PermissionResolved`
- Liveness (1): `Heartbeat`

```go
type Event interface {
    isHarnessEvent()
    GetSessionID() string
    GetAgentName() string
    GetTimestamp() time.Time
}

type EventMeta struct {
    SessionID string    `json:"session_id"`
    AgentName string    `json:"agent_name"`
    Timestamp time.Time `json:"timestamp"`
}

// RunError carries the canonical error code. Used both by adapters that fail
// and by the runtime's panic-recovery wrapper (Fix 2).
type RunError struct {
    EventMeta
    Code              ErrorCode `json:"code"`
    Message           string    `json:"message"`
    Retryable         bool      `json:"retryable"`
    Cause             string    `json:"cause,omitempty"`
    RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
}

// ... remaining 13 concrete types unchanged from arch-spec.md §3.4
```

`ErrorCode` constants (`pkg/harness/errors.go`):

```go
const (
    ErrCodeRateLimited          ErrorCode = "rate_limited"
    ErrCodeAuth                 ErrorCode = "auth"
    ErrCodeContextExhausted     ErrorCode = "context_exhausted"
    ErrCodeTimeout              ErrorCode = "timeout"
    ErrCodeUserCancelled        ErrorCode = "user_cancelled"
    ErrCodePermissionDenied     ErrorCode = "permission_denied"
    ErrCodeProtocolError        ErrorCode = "protocol_error"
    ErrCodeBinaryVersionMismatch ErrorCode = "binary_version_mismatch"
    ErrCodeCapabilityMismatch   ErrorCode = "capability_mismatch"
    ErrCodeHarnessCrashed       ErrorCode = "harness_crashed"  // panic-recovered
    ErrCodeSandboxEscape        ErrorCode = "sandbox_escape"
    ErrCodeInternal             ErrorCode = "internal"
    ErrCodeUnsupported          ErrorCode = "unsupported"
)
```

### 3.5 EventSink

```go
type EventSink interface {
    Emit(Event) error
}
```

The FSM enforcer wraps any downstream sink and validates lifecycle/balance rules per FR-17 / FR-18.

### 3.6 RawEventSink (opt-in)

```go
type RawEventSink interface {
    EmitRaw(source string, frame []byte)
}
```

`source` constants in `pkg/harness/raw.go`: `"claude-stream-json"`, `"codex-json"`, `"opencode-line"`, `"acp-update"`.

### 3.7 ToolExecutor (ACP only)

Unchanged from v1 §3.7. Lives in `harness.go`. Passed to ACP adapters via `ACPCallbacks.ToolExecutor`, never to non-ACP adapters.

```go
type ToolExecutor interface {
    ReadTextFile(ctx context.Context, req ReadFileRequest) (ReadFileResponse, error)
    WriteTextFile(ctx context.Context, req WriteFileRequest) (WriteFileResponse, error)

    CreateTerminal(ctx context.Context, req CreateTerminalRequest) (CreateTerminalResponse, error)
    TerminalOutput(ctx context.Context, req TerminalOutputRequest) (TerminalOutputResponse, error)
    WaitForTerminalExit(ctx context.Context, req WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error)
    KillTerminal(ctx context.Context, req KillTerminalRequest) error
    ReleaseTerminal(ctx context.Context, req ReleaseTerminalRequest) error
}
```

Sandbox-enforced. `fs/list_dir` is **not** in `acp-go-sdk@v0.13.0`; deferred to v1.1.

### 3.8 PermissionRequester (ACP only)

```go
type PermissionRequester interface {
    Request(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

type PermissionRequest struct {
    RequestID string
    Operation string // "fs/write_text_file", "terminal/create"
    Target    string
    Reason    string
    AgentName string
}

type PermissionDecision string
const (
    PermissionAllow PermissionDecision = "allow"
    PermissionDeny  PermissionDecision = "deny"
)

type PermissionScope string
const (
    PermissionScopeOnce    PermissionScope = "once"
    PermissionScopeSession PermissionScope = "session"
)
```

Implementation lives in `pkg/runtime/harness_delegation.go` (`runtimePermissionRequester`). Consults `team.Permissions()`, then `agent.Harness().PermissionPolicy`, then emits `ToolCallConfirmationEvent` to the parent's `EventSink`.

### 3.9 HarnessConfig (config schema type) and unknown-key validation (Fix 5)

`HarnessConfig` schema-level type lives in `pkg/config/latest/types.go` (§2.3).

**YAML unknown-key validation.** The teamloader unmarshals `HarnessConfig.Config` (a `map[string]any`) into the adapter's registered typed config struct using `yaml.v3`'s decoder with `KnownFields(true)`. This is the v3 equivalent of `encoding/json`'s `DisallowUnknownFields`; `DisallowUnknownField` (singular, JSON-style) is **not** an API on `yaml.v3` and must not appear in the implementation.

```go
// pkg/teamloader/harness.go
func unmarshalHarnessConfig(adapterName string, raw map[string]any, zero func() any) (any, error) {
    cfg := zero()  // returns a *AdapterConfig zero value
    // Round-trip map → YAML bytes → typed struct with strict field checking.
    b, err := yaml.Marshal(raw)
    if err != nil {
        return nil, fmt.Errorf("internal: marshal harness.config map: %w", err)
    }
    dec := yaml.NewDecoder(bytes.NewReader(b))
    dec.KnownFields(true)
    if err := dec.Decode(cfg); err != nil {
        return nil, translateUnknownFieldError(adapterName, err)
    }
    return cfg, nil
}
```

**Exact error format.** Translator maps yaml.v3's `unknown field "X"` error into:

```
error: unknown field "typo" in harness config for agent "code-reviewer"
  valid fields: type, command, args, env, working_dir, timeout, config
```

Implementation:

```go
// translateUnknownFieldError converts yaml.v3's verbose error into a
// docker-agent-flavored one with the agent name, the offending field, and
// the list of valid fields from the typed config struct (via reflection on
// the struct's yaml tags). The agent name is supplied by the caller; the
// field list is derived from the registered zero-value struct.
//
// Output format (exact):
//   error: unknown field "<key>" in harness config for agent "<agent_name>"
//     valid fields: <comma-separated yaml tags>
func translateUnknownFieldError(agentName string, err error) error
```

The valid-fields list is built by reflecting over the registered zero-value config struct's exported fields, reading the `yaml:"..."` tag from each. The error MUST include the agent name (so multi-agent configs are diagnosable) and the comma-separated list of valid keys.

### 3.10 Session.HarnessSession field

See §2.6. `map[string]string` keyed by agent name (FR-26).

### 3.11 Session token ownership guard (FR-NEW-11, new in v2)

`pkg/harness/registry.go` adds a process-wide tracking map:

```go
// tokenInUse tracks active sessions by their adapter-opaque session token to
// prevent concurrent reuse. FR-NEW-11: the same token used by two adapter
// instances simultaneously emits RunError{code: capability_mismatch} for the
// second.
var (
    tokenInUseMu sync.Mutex
    tokenInUse   = make(map[string]bool) // key: adapter_name + ":" + token
)

// AcquireToken registers a session token as in-use. Returns false if the
// token was already in-use; the caller MUST emit RunError and abort.
func AcquireToken(adapterName, token string) bool {
    if token == "" {
        return true  // empty token = fresh session, no guard needed
    }
    tokenInUseMu.Lock()
    defer tokenInUseMu.Unlock()
    key := adapterName + ":" + token
    if tokenInUse[key] {
        return false
    }
    tokenInUse[key] = true
    return true
}

// ReleaseToken deregisters a token. Idempotent.
func ReleaseToken(adapterName, token string) {
    if token == "" {
        return
    }
    tokenInUseMu.Lock()
    defer tokenInUseMu.Unlock()
    delete(tokenInUse, adapterName+":"+token)
}
```

The runtime calls `AcquireToken` at the start of `runHarnessForwarding` when `req.ResumeToken != ""`. On false, it emits:

```go
req.Events.Emit(harness.RunError{
    EventMeta: ...,
    Code:      harness.ErrCodeCapabilityMismatch,
    Message:   "session token already in use",
    Retryable: false,
})
```

and aborts. The matching `ReleaseToken` is `defer`'d for symmetry.

---

## 4. Data flow

### 4.1 Invoking a harness-backed subagent

```
Orchestrator (model-backed) emits a tool call:
  transfer_task{agent: "code-reviewer", task: "review this diff"}

    ↓ loop.go dispatcher matches tool name

handleTaskTransfer → runForwarding → child.HasHarness() == true → runHarnessForwarding

runHarnessForwarding(ctx, parent, evts, callerAgent, child, req)
  opens OTel span "runtime.harness_session"
  r.telemetry.RecordHarnessStart(child.Harness().AdapterName, child.Name())
  loads adapter:  harness.LookupAdapter(child.Harness().AdapterName)

  // Build the request.
  resumeToken := parent.HarnessSessionGet(child.Name())
  var simHistory []chat.Message
  if resumeToken == "" && child.Harness().Config.SupportsMultiTurn-via-simulation {
      simHistory = parent.GetAllMessages()
  }
  // FR-NEW-11: acquire token ownership before dispatching.
  if !harness.AcquireToken(adapterName, resumeToken) {
      evts.Emit(translate(RunError{Code: capability_mismatch, Message: "session token already in use"}))
      return tools.ResultError("session token already in use"), nil
  }
  defer harness.ReleaseToken(adapterName, resumeToken)

  req := harness.SubSessionRequest{
      RunID, ParentID, AgentName,
      SystemPrompt, Task,
      ResumeToken:      resumeToken,
      SimulatedHistory: simHistory,
      WorkingDir, Env (post-allowlist), Config (typed),
      Timeout, Logger,
      Events:  fsm.NewEnforcer(translateSink{...}),
      RawSink: nil,  // wired by --debug flag in CLI
  }

  // Dispatch on adapter type (Fix 3).
  if acp, ok := adapter.(harness.ACPAdapter); ok {
      bindings := harness.ACPCallbacks{
          ToolExecutor: sandbox.NewToolExecutor(req.WorkingDir),
          Permission:   &runtimePermissionRequester{...},
      }
      // Defensive: runtime always constructs both.
      if bindings.ToolExecutor == nil || bindings.Permission == nil {
          panic("runtime: ACPCallbacks nil after construction")
      }
      go r.runAdapterACP(ctx, acp, req, bindings)
  } else {
      go r.runAdapter(ctx, adapter, req)
  }

  // Drain events (translator emits runtime events into evts).
  drainEvents(...)
  fires r.executeSubagentStopHooks
  r.telemetry.RecordHarnessFinish(...)
  span.End()
  returns tools.ResultSuccess(accumulatedText) or tools.ResultError on RunError
```

### 4.2 Event flow from harness subprocess to TUI

```
harness subprocess  →  adapter parses stdout/stderr  →  adapter.req.Events.Emit(canonicalEvent)
    ↓
fsm.Enforcer.Emit (validates lifecycle/balance per FR-17/FR-18)
    ↓
translateSink.Emit  (pkg/runtime/harness_delegation.go — THE AUTHORITATIVE LOCATION)
  switches on Event concrete type, builds runtime.Event(s) per FR-21 table
  has access to *session.Session and *agent.Agent via the closure
    ↓
parent.evts.Emit(runtimeEvent)
    ↓
PersistenceObserver writes to session store; TUI renders
```

`translateSink` is defined in `pkg/runtime/harness_delegation.go`. There is **no** `pkg/harness/translate.go` — that file does not exist in v2. `pkg/harness` cannot construct `runtime.Event` because it does not import `pkg/runtime` (one-way import direction).

The minimal `translateSink` shape:

```go
type translateSink struct {
    evts   EventSink             // runtime EventSink (parent session's stream)
    parent *session.Session
    child  *agent.Agent
    r      *LocalRuntime
    // Accumulator for streaming TextDeltas → MessageAdded on TextEnd.
    accum  map[string]*strings.Builder
}

func (s *translateSink) Emit(e harness.Event) error {
    switch ev := e.(type) {
    case harness.RunStart:
        s.evts.Emit(&runtime.StreamStartedEvent{...})
    case harness.TextDelta:
        s.accum[ev.MessageID].WriteString(ev.Text)
    case harness.TextEnd:
        msg := buildMessage(ev, s.accum[ev.MessageID].String())
        s.evts.Emit(&runtime.MessageAddedEvent{Message: msg})
        delete(s.accum, ev.MessageID)
    case harness.ToolCallStart:
        s.evts.Emit(&runtime.ToolCallEvent{...})
    case harness.ToolCallEnd:
        s.evts.Emit(&runtime.ToolCallResponseEvent{...})
    case harness.RunError:
        s.evts.Emit(&runtime.ErrorEvent{Code: mapCode(ev.Code), ...})
    case harness.RunEnd:
        if ev.SessionToken != "" {
            s.parent.HarnessSessionSet(s.child.Name(), ev.SessionToken)
        }
        s.evts.Emit(&runtime.SubSessionCompletedEvent{...})
        s.evts.Emit(&runtime.StreamStoppedEvent{...})
    // Heartbeat, PermissionPending, PermissionResolved, Reasoning* emit nothing
    // or auxiliary runtime events depending on the FR-21 table.
    default:
        // Sealed interface; the FSM enforcer rejects unknown types upstream.
        return fmt.Errorf("unknown canonical event: %T", e)
    }
    return nil
}
```

### 4.3 Multi-turn sessions (Fix 4 wording)

**Native resume (Claude Code, ACP harnesses):**

```
Turn 1:
  parent.HarnessSessionGet("code-reviewer") == ""
  req.ResumeToken == ""
  req.SimulatedHistory == nil
  adapter starts fresh session
  on RunEnd: ev.SessionToken == "abc-123"
  translateSink writes parent.HarnessSessionSet("code-reviewer", "abc-123")

Turn 2:
  parent.HarnessSessionGet("code-reviewer") == "abc-123"
  req.ResumeToken == "abc-123"
  req.SimulatedHistory == nil  (runtime guarantees: at most one is non-empty)
  adapter resumes via native mechanism (e.g., claude --resume abc-123)
  on RunEnd: token updated
```

**Simulated multi-turn (Codex, OpenCode CLI):**

```
Turn N:
  parent.HarnessSessionGet("code-reviewer") == ""  (never set; no native resume)
  req.ResumeToken == ""
  req.SimulatedHistory = parent.GetAllMessages()
  adapter prepends serialized SimulatedHistory to the task string until the
    token budget (default 50% of context window, configurable via Config)
  on exceeding 60% budget: adapter emits a Warning event (we do not have a
    Warning event type yet; adapter emits a ToolCallEnd with informational
    payload OR a RunError with Code=context_exhausted Retryable=true depending
    on configuration; pinned during impl)
  on exceeding 100%: adapter emits RunError{Code: context_exhausted}
  on RunEnd: ev.SessionToken == "" (no token to persist)
```

### 4.4 ACP permission prompt flow

Unchanged from v1 §4.4. The ACP adapter calls `acp.Permission.Request(...)` from inside its `session/request_permission` JSON-RPC handler. The runtime's `runtimePermissionRequester` consults team policy → agent policy → TUI, emits `ToolCallConfirmationEvent` to the parent's `EventSink`, blocks on `r.resumeChan`.

---

## 5. Technology decisions

### 5.1 Option B (canonical events public, translator at runtime boundary) vs Option A

**Decision: Option B.** Unchanged from v1 §5.1.

### 5.2 Event as interface (discriminated union) vs fat struct

**Decision: discriminated union.** Unchanged from v1 §5.2.

### 5.3 ACP base in `pkg/harness/acp/`

**Decision: shared base + per-harness subpackages.** Unchanged from v1 §5.3.

### 5.4 Config version bump strategy

**Decision: snapshot then bump.** Unchanged from v1 §5.4.

### 5.5 ACP separation via two interfaces (new in v2 — Fix 3)

**Decision: separate `HarnessAdapter` and `ACPAdapter` interfaces.** Rejected alternatives:

- **Nilable `Tools` / `Permission` fields on `SubSessionRequest`.** Original v1 shape. Footgun: nothing in the type system distinguishes ACP from non-ACP. A non-ACP adapter could be passed non-nil callbacks; an ACP adapter could be passed nil and segfault. DX review §5.1.
- **Sub-struct `ACP *ACPCallbacks` on the request.** Better than v1 but still requires nil-checking inside the adapter. Loses compile-time enforcement.

The two-interface split:
- Compile-time guarantee: only adapters that implement `ACPAdapter` can receive `ACPCallbacks`.
- Runtime dispatch is a single type assertion: `if acp, ok := adapter.(ACPAdapter); ok`.
- The runtime constructs both fields of `ACPCallbacks` itself and never passes them to non-ACP adapters.

### 5.6 `Run` returns void; panic recovery in the runtime (new in v2 — Fix 2)

**Decision: `Run` returns no value. All terminal states flow through events.**

Rejected: `Run(ctx, req) error` (v1 shape). The error path was undocumented at the call site (FR-NEW-10 said "silently convert to ErrorEvent"). Adapter authors would assume the standard Go contract and return errors; those errors would be swallowed and translated to a generic code. Pit of failure.

The new contract:
- Adapter MUST emit exactly one terminal event (`RunEnd` or `RunError`) on `req.Events` before `Run` returns.
- Adapter MUST NOT panic. The runtime catches panics defensively via `recover()` and converts to `RunError{Code: ErrCodeHarnessCrashed, Cause: <stack>}` so a buggy adapter cannot crash the orchestrator process.
- The runtime, not the adapter, is responsible for terminal-event synthesis on adapter crash.

This is enforced in `runAdapter` (§2.5.1) and tested by FR-NEW-10 unit in impl-plan-v2 (P1-A).

---

## 6. Risks

### 6.1 ACP `fs/list_dir` not in SDK v0.13.0

Unchanged from v1 §6.1.

### 6.2 Static vs negotiated ACP capabilities

Unchanged from v1 §6.2.

### 6.3 Pre-existing test failures

Unchanged from v1 §6.3.

### 6.4 CI runner provisioning for integration tests

Unchanged from v1 §6.4.

### 6.5 Harness binary version drift

Unchanged from v1 §6.5.

### 6.6 Orphan process leak

Unchanged from v1 §6.6. FR-13 cleanup order plus `goleak` + process-orphan test in P3-C.

### 6.7 (NEW) Adapter panics on the caller's goroutine

**Impact:** A buggy adapter (nil pointer deref in parser, slice bounds error) crashes the orchestrator process. Mark loses all in-flight sessions.

**Mitigation:** `runAdapter` wraps every adapter call in `defer recover()` that converts panic to `RunError{Code: ErrCodeHarnessCrashed, Cause: debug.Stack()}` (§2.5.1). Tested in P1-A. The adapter goroutine is the only entry point; there is no other surface where adapter code runs outside the recovery wrapper.

### 6.8 (NEW) Concurrent reuse of the same session token

**Impact:** Two concurrent `transfer_task` calls to the same harness-backed agent in the same parent session would both try to resume the same harness session token. The harness's native resume semantics are undefined under concurrent access; data could leak between sub-sessions.

**Mitigation:** `harness.AcquireToken` / `ReleaseToken` guard (§3.11). The second concurrent attempt fails fast with `RunError{Code: ErrCodeCapabilityMismatch, Message: "session token already in use"}`. Tested in P1-C (new unit).

---

## 7. Cross-references

- **PRD §1.2 file list:** every file appears in §2 of this spec, with `translate.go` removed from `pkg/harness/` (Fix 1).
- **PRD §4 functional requirements:** numbered FRs are referenced inline.
- **PRD §7 adapter specs:** the per-adapter event-mapping and error-mapping tables ARE the binding contract.
- **PRD appendix A:** refined into compilable Go in §3 here. Differences documented per interface.
- **dx-review-arch.md:** five blockers addressed (B1: translator location §2.1+§4.2; B2: `Run` returns void §3.1; B3: ACP separation §3.1+§3.3; B4: ResumeToken+SimulatedHistory §3.3; B5: YAML error format §3.9).
- **consistency-check.md:** six gaps addressed (FR-NEW-5 §2.7; FR-NEW-10 §2.5.1 + impl-plan-v2 P1-A test; FR-NEW-11 §3.11 + impl-plan-v2 P1-C; FR-25 OpenCode half via `multiturn.go` in §2.1 + impl-plan-v2 P2-B; FR-NEW-13 record.go via §2.1 + impl-plan-v2 P0-E; sandbox stub clarification handled in impl-plan-v2 P0-E/P2-D scope split).
