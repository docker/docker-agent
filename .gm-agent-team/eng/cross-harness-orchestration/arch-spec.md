# Architecture Spec: Cross-Harness Orchestration

**Owner:** docker-agent eng
**Status:** APPROVED FOR IMPLEMENTATION
**Source PRD:** `prd-v2.md`
**Insertion points:** `pkg/runtime/agent_delegation.go` (`runForwarding` line 248, `runCollecting` line 310)

---

## 1. Scope

This document specifies the Go-level architecture for cross-harness orchestration: package layout, exact interface signatures, data flow from orchestrator tool call to harness subprocess and back, the technology decisions that shape those signatures, and the risks tracked at the architecture level (not phase-level).

It binds the PRD's appendix A and §1.2 file list into compilable Go and resolves the open questions raised in the arch review (Option A vs B, fat struct vs interface, ACP base location).

---

## 2. Component design

### 2.1 New package: `pkg/harness/`

Directory layout (mirrors PRD §1.2 item 9):

```
pkg/harness/
  harness.go            // HarnessAdapter interface, AdapterCapabilities, HarnessSessionRequest,
                        // EventSink, RawEventSink, ToolExecutor, PermissionRequester,
                        // typed enums (ProtocolClass, ErrorCode, PermissionDecision, …)
  event.go              // Discriminated-union Event interface and the 14 concrete event structs
  registry.go           // Adapter registry: Register(name, factory), Lookup(name),
                        // typed-config registration for FR-5 unknown-key rejection
  translate.go          // harness.Event → runtime.Event translator (Option B boundary)
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
    record.go           // Recording wrapper used during adapter dev
  claude/
    adapter.go          // Claude Code CLI adapter (Phase 1)
    parser.go           // stream-json NDJSON parser
    config.go           // Typed Config struct (max_turns, system_append, …)
  codex/
    adapter.go          // Codex CLI adapter (Phase 2)
    parser.go           // codex --json parser
    config.go           // Typed Config struct (model, reasoning_effort, multi_turn_budget_tokens, …)
  opencode/
    adapter.go          // OpenCode CLI adapter (Phase 2)
    parser.go           // opencode --format json parser
    config.go           // Typed Config struct (task_prefix, …)
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

**Imports:**
- `pkg/harness` is imported by `pkg/runtime` (for the discriminated-union types, translator, FSM, registry lookup) and by `pkg/teamloader` (for adapter typed-config validation, capability lookup, binary PATH check).
- Adapter subpackages (`pkg/harness/claude/...`) are imported by the program's main entry point (`cmd/docker-agent/`) via blank imports for their `init()` registration. The runtime itself does **not** blank-import adapters; that keeps `pkg/runtime` free of vendor-specific dependencies and lets a library consumer pick which adapters to link.

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

`HarnessSpec` lives in `pkg/agent/` (not `pkg/harness/`) because `pkg/agent` does not import `pkg/harness`. Reverse direction is fine — `pkg/harness` imports `pkg/agent` to read `*HarnessSpec` off an `*Agent` passed into translation helpers.

**`pkg/agent/opts.go`:**

```go
func WithHarness(spec *HarnessSpec) Opt {
    return func(a *Agent) { a.harness = spec }
}
```

Mirrors `WithModel`. Mutually exclusive at the schema layer (FR-1); the agent struct itself permits both — that lets teamloader produce an `*Agent` with `harness` set and `models == nil` without contortion.

### 2.3 Changes to `pkg/config/latest/`

**`pkg/config/latest/types.go`:**

```go
type AgentConfig struct {
    // ... existing fields ...
    Harness *HarnessConfig `json:"harness,omitempty" yaml:"harness,omitempty"`
}

// HarnessConfig is the schema-level shape. Validation rules live in validate.go.
// Binary PATH lookup happens in pkg/teamloader (FR-4).
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
3. `PermissionPolicy.IUnderstandTheRisk` cross-field rule: true with no nested `auto_allow` / `allow_unrestricted` → error; vice versa → error (FR-7).
4. No filesystem I/O. Unknown-key rejection for `Harness.Config` is deferred to teamloader, where the adapter's typed config struct is registered (FR-5).

### 2.4 Config version bump: v9 → v10

**Strategy:** snapshot before mutate (FR-6).

1. Copy the current `pkg/config/latest/` tree to a new `pkg/config/v9/` directory (frozen). Update its package declaration to `package v9`. Update its `Version` constant to remain `"9"`.
2. In `pkg/config/latest/`, bump `Version = "10"`.
3. Wire `pkg/config/upgrade/` (or wherever version-stepping lives — `config.Load` already handles version detection) so a v9 file upgrades to v10. The upgrade is a no-op for configs without `harness:`. v9 files with `harness:` fail upgrade (would not have parsed under v9 anyway).
4. Existing `pkg/config/v8/` and earlier remain untouched.

Add a regression test that loads a representative `pkg/teamloader/testdata/*.yaml` file (currently `version: "9"`) under `Version = "10"` and asserts the result is structurally identical to the v9 load (`Agents`, `Models`, `Providers` deep-equal). This is the no-op upgrade gate.

### 2.5 Changes to `pkg/runtime/agent_delegation.go`

The two functions branch on `child.HasHarness()`:

```go
func (r *LocalRuntime) runForwarding(ctx context.Context, parent *session.Session, evts EventSink, req delegationRequest) (*tools.ToolCallResult, error) {
    span := trace.SpanFromContext(ctx)
    callerAgent, err := r.team.Agent(r.CurrentAgentName())
    if err != nil { return nil, fmt.Errorf("current agent not found: %w", err) }
    child, err := r.team.Agent(req.AgentName)
    if err != nil { return nil, err }

    if req.SwitchCurrentAgent {
        defer r.swapCurrentAgent(ctx, parent.ID, callerAgent, child, evts)()
    }

    if child.HasHarness() {
        return r.runHarnessForwarding(ctx, parent, evts, callerAgent, child, req)
    }
    return r.runModelForwarding(ctx, parent, evts, callerAgent, child, req)  // existing body, refactored
}

func (r *LocalRuntime) runCollecting(ctx context.Context, parent *session.Session, cfg SubSessionConfig, onContent func(string)) *agenttool.RunResult {
    child, err := r.team.Agent(cfg.AgentName)
    if err != nil { return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", cfg.AgentName, err)} }

    if child.HasHarness() {
        return r.runHarnessCollecting(ctx, parent, cfg, child, onContent)
    }
    return r.runModelCollecting(ctx, parent, cfg, child, onContent)  // existing body, refactored
}
```

New functions in the same file (or a new `pkg/runtime/harness_delegation.go` to keep diffs reviewable):

- `runHarnessForwarding(ctx, parent, evts, callerAgent, child, req) (*tools.ToolCallResult, error)` — opens OTel span `runtime.harness_session`, builds `HarnessSessionRequest`, instantiates `EventSink` chain (FSM enforcer → translator → forwarder to `evts`), looks up adapter via `harness.Lookup(child.Harness().AdapterName)`, calls `adapter.Run(ctx, req)`, persists `SessionToken` from the trailing `RunEnd` into `parent.HarnessSession[child.Name()]`, emits `SubSessionCompletedEvent`, fires `subagent_stop` hook. Returns the last assistant text from the `TextEnd` accumulator (`tools.ResultSuccess(text)`), or a `tools.ResultError` carrying the `RunError.Code` and `Message` on terminal error.
- `runHarnessCollecting(ctx, parent, cfg, child, onContent) *agenttool.RunResult` — same plumbing without the AgentSwitching events, drives `onContent` from `TextEnd.Content`.

The translator and FSM enforcer are reusable across both paths; only the outer event-disposition policy differs.

### 2.6 Changes to `pkg/session/session.go`

Add one field on `Session`:

```go
type Session struct {
    // ... existing fields ...

    // HarnessSession stores adapter-opaque resume tokens for harness-backed
    // subagents (FR-26). Key is the agent name, value is the adapter's
    // opaque session token (e.g. Claude Code's session_id, ACP's session ID).
    // Serializes through the existing session-store JSON; no schema migration.
    HarnessSession map[string]string `json:"harness_session,omitempty"`
}
```

Accessors are intentionally not added: callers read/write through the map directly (matches how `AgentModelOverrides` is used today). Concurrent access is gated by the existing `Session.mu` for any field that is read on the request hot path; for `HarnessSession` we add a small `HarnessSessionGet` / `HarnessSessionSet` pair that locks `mu` to keep the contract obvious. (Refinement at impl time if this proves over-engineered.)

### 2.7 Changes to `pkg/teamloader/teamloader.go`

In the agent-build loop (around line 146):

1. If `agentConfig.Harness != nil`:
   - Look up the adapter via `harness.LookupAdapter(agentConfig.Harness.Type)`. Unknown type → error.
   - Unmarshal `agentConfig.Harness.Config` (raw `map[string]any`) into the adapter's typed config struct using `yaml.DisallowUnknownField()`. Unknown keys → load-time error naming the field (FR-5).
   - Build `*agent.HarnessSpec`. Resolve `WorkingDir` per FR-8 (`harness.working_dir` ?? `runConfig.WorkingDir` ?? `os.Getwd()`).
   - PATH-check the binary: `exec.LookPath(spec.Command)` (or `Capabilities().Requires.Binary` when `Command == ""`). Missing → error naming the binary + an install hint pulled from `Capabilities().Requires.InstallHint`.
   - Construct the agent with `agent.New(name, "", agent.WithHarness(spec), agent.WithDescription(...), agent.WithMaxIterations(...), agent.WithHooks(...))`. Skip model construction; skip toolset construction (harness owns its own tools). `sub_agents` / `handoffs` already rejected by validate.go.
2. Else: existing model-backed construction.

Skill toolset construction must also reject harness-backed agents as skill targets (FR-NEW-5); enforced where `run_skill` resolves its target agent, not in teamloader.

### 2.8 Changes to `pkg/runtime/loop.go`

**No changes.** `transfer_task` continues to dispatch by agent name; `handleTaskTransfer` calls `runForwarding`, which now branches on `HasHarness()`. There is no new top-level tool in v1 (PRD §1.2 item 10).

### 2.9 Hooks integration

In `runHarnessForwarding` / `runHarnessCollecting`:

- `on_agent_switch` fires via the existing `r.executeOnAgentSwitchHooks` call inside `r.swapCurrentAgent` when `SwitchCurrentAgent` is true. No change.
- `subagent_stop` fires via the same `defer r.executeSubagentStopHooks` pattern used by `runModelForwarding`. Pass `child.Name()` and the accumulated last-assistant text (concatenated `TextEnd.Content`s).
- `pre_tool_use` and `before_llm_call` are intentionally **not** invoked on the harness path: the harness owns the model loop and its own tool dispatch. The runtime cannot intercept either.

### 2.10 Telemetry and OTel

- New OTel span `runtime.harness_session` opened at the top of `runHarnessForwarding` / `runHarnessCollecting` with attributes `harness.type`, `agent.name`, `working_dir`, `resume` (bool), `session.id`. (FR-NEW-4)
- New `Telemetry` methods: `RecordHarnessStart(harnessType, agentName)`, `RecordHarnessFinish(harnessType, agentName, code ErrorCode, durationMs)`, `RecordHarnessEvent(harnessType, eventKind, latencyMs)`. Wired on the existing `r.telemetry` sink (FR-NEW-3).

---

## 3. Interface definitions

All types live in `pkg/harness/`. Public.

### 3.1 HarnessAdapter

```go
package harness

import (
    "context"
    "time"
)

// HarnessAdapter is the contract every adapter implements. Implementations
// are stateless and safe for concurrent use; per-session state lives on
// the goroutine running Run. Process-per-session is mandatory (FR-12).
type HarnessAdapter interface {
    // Name returns the stable adapter identifier (e.g. "claude-code"). Used
    // as the registry key and as the canonical value of HarnessConfig.Type.
    Name() string

    // Capabilities returns the adapter's static support surface. Pure
    // function: no I/O, no process spawn, safe to call at config-load time
    // (FR-10).
    Capabilities() AdapterCapabilities

    // Run drives a single harness session to terminal state. It MUST NOT
    // panic on the caller's goroutine. All harness-runtime errors are
    // surfaced as RunError events on req.Events. Returns nil on clean
    // shutdown; a non-nil return is reserved for adapter-internal bugs
    // where the event sink is unreachable (FR-11, FR-NEW-10).
    Run(ctx context.Context, req HarnessSessionRequest) error
}
```

### 3.2 AdapterCapabilities

```go
type AdapterCapabilities struct {
    Protocol     ProtocolClass     // ProtocolStream | ProtocolACP
    Requires     HostRequirements  // binary name, min version, env vars
    Features     AdapterFeatures   // capability flags
    BuiltInTools []string          // informational; not enforced
    IdleTimeout  time.Duration     // process-pool idle timeout
}

type HostRequirements struct {
    Binary       string            // e.g. "claude"
    MinVersion   string            // semver-ish, empty == no check
    EnvVars      []string          // names of env vars the adapter expects to forward
    InstallHint  string            // free-form text shown in load-time error
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

### 3.3 HarnessSessionRequest (alias of "SubSessionRequest" per PRD §4.2)

```go
import "github.com/docker/docker-agent/pkg/chat"

type HarnessSessionRequest struct {
    SessionID    string             // sub-session ID (for event attribution)
    AgentName    string             // child agent name
    Task         string             // primary task description
    SystemPrompt string             // optional; adapter may ignore if SupportsPerCallSystemPrompt=false
    SessionToken string             // empty on first turn; adapter-opaque resume token
    WorkingDir   string             // sandbox root (FR-38)
    Env          map[string]string  // filtered through sandbox.AllowedEnv (FR-41)
    PriorTurns   []chat.Message     // for simulated multi-turn (FR-25, FR-27)
    Timeout      time.Duration      // wall-clock timeout for Run (FR-29)
    Spec         *agent.HarnessSpec // the resolved spec (for adapter-typed Config access)
    Events       EventSink          // FSM-enforced canonical event sink (required)
    RawSink      RawEventSink       // optional; nil disables raw frame forwarding (FR-23)
    Tools        ToolExecutor       // ACP only; nil for non-ACP (FR-38, FR-39)
    Permission   PermissionRequester // ACP only; nil for non-ACP (FR-33)
}
```

`agent.HarnessSpec` is in `pkg/agent`; `pkg/harness` already imports `pkg/agent` for this. The cycle is one-way.

### 3.4 Event — discriminated union

```go
import (
    "encoding/json"
    "time"
)

// Event is the canonical event type. Implementations are the 14 concrete
// structs below. The unexported isHarnessEvent() method makes the union
// sealed: external packages cannot add new event kinds.
type Event interface {
    isHarnessEvent()
    // GetSessionID returns the sub-agent session ID. Mandatory on every
    // event for attribution (FR-16).
    GetSessionID() string
    // GetAgentName returns the agent name. Mandatory on every event.
    GetAgentName() string
    // GetTimestamp returns the wall-clock time the event was produced.
    GetTimestamp() time.Time
}

// Embedded in every concrete event for the three mandatory fields.
type EventMeta struct {
    SessionID string    `json:"session_id"`
    AgentName string    `json:"agent_name"`
    Timestamp time.Time `json:"timestamp"`
}

func (e EventMeta) GetSessionID() string    { return e.SessionID }
func (e EventMeta) GetAgentName() string    { return e.AgentName }
func (e EventMeta) GetTimestamp() time.Time { return e.Timestamp }

// --- Lifecycle (3) ---

type RunStart struct {
    EventMeta
    Model string   `json:"model,omitempty"`
    Tools []string `json:"tools,omitempty"`
}

type RunEnd struct {
    EventMeta
    SessionToken string `json:"session_token,omitempty"` // for multi-turn resume
    Usage        Usage  `json:"usage"`
}

type RunError struct {
    EventMeta
    Code              ErrorCode `json:"code"`
    Message           string    `json:"message"`
    Retryable         bool      `json:"retryable"`
    Cause             string    `json:"cause,omitempty"`
    RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
}

// --- Text (3) ---

type TextStart struct {
    EventMeta
    MessageID string `json:"message_id"`
}

type TextDelta struct {
    EventMeta
    MessageID string `json:"message_id"`
    Text      string `json:"text"`
}

type TextEnd struct {
    EventMeta
    MessageID string `json:"message_id"`
    Content   string `json:"content"` // full assembled text, for non-streaming adapters
}

// --- Reasoning (3) ---

type ReasoningStart struct {
    EventMeta
    MessageID string `json:"message_id"`
}

type ReasoningDelta struct {
    EventMeta
    MessageID string `json:"message_id"`
    Text      string `json:"text"`
}

type ReasoningEnd struct {
    EventMeta
    MessageID string `json:"message_id"`
}

// --- Tool (2) ---

// ToolCallID is a typed wrapper to keep call IDs from being confused with
// session IDs or message IDs in helper signatures.
type ToolCallID string

type ToolCallStart struct {
    EventMeta
    CallID ToolCallID      `json:"call_id"`
    Name   string          `json:"name"`
    Args   json.RawMessage `json:"args,omitempty"`
}

type ToolCallEnd struct {
    EventMeta
    CallID ToolCallID      `json:"call_id"`
    Result json.RawMessage `json:"result,omitempty"`
    Error  string          `json:"error,omitempty"`
}

// --- Permission (2) ---

type PermissionPending struct {
    EventMeta
    RequestID string `json:"request_id"`
    Operation string `json:"operation"` // e.g. "fs/write_text_file", "terminal/create"
    Target    string `json:"target"`    // path or command
    Reason    string `json:"reason,omitempty"`
}

type PermissionResolved struct {
    EventMeta
    RequestID string             `json:"request_id"`
    Decision  PermissionDecision `json:"decision"`
    Scope     PermissionScope    `json:"scope,omitempty"`
}

// --- Liveness (1) ---

type Heartbeat struct {
    EventMeta
}

// Total: 3 + 3 + 3 + 2 + 2 + 1 = 14 concrete events.
// PRD says "12 canonical events". The PRD's count groups Permission and
// Liveness together as 3 (Pending, Resolved, Heartbeat); we make Heartbeat
// a separate Liveness category for code clarity. Wire shape is identical.

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
```

**Note on event count.** The PRD §4.3 lists "12 canonical events" and treats Heartbeat as a sibling of Permission. Our Go layout exposes 14 concrete types (PermissionPending + PermissionResolved + Heartbeat = 3 events, not "Permission: 2 + Liveness: 1 = 3 events folded into 12"). The PRD's count is consistent if you group `PermissionPending`/`PermissionResolved` as one event with two phases; ours separates them for compile-time exhaustiveness on type switches. Wire compatibility is unaffected.

### 3.5 EventHandler / EventSink

```go
// EventSink is the consumer-side interface adapters emit to.
//
// Implementations are responsible for buffering and backpressure;
// adapters MUST NOT block forever on Emit. The runtime ships:
//   - fsmEventSink (pkg/harness/fsm.go): enforces FR-17 / FR-18 lifecycle
//     and balance rules, wrapping any downstream sink.
//   - translateEventSink (pkg/runtime): drains canonical events,
//     converts to runtime.Event (per FR-21), and forwards to the
//     runtime's EventSink (the parent session's UI/persistence channel).
type EventSink interface {
    Emit(Event) error
}

// EventHandler is the symmetrical reader side, used by tests and the
// replay harness. Single OnEvent method per PRD §3.
type EventHandler interface {
    OnEvent(Event) error
}
```

`EventSink.Emit` returns `error` (not the PRD appendix-A signature, which had no return). Reason: the FSM enforcer needs a way to surface "you violated the canonical FSM" without panicking in production builds (FR-17). Adapters that don't care can ignore the return.

### 3.6 RawEventSink (opt-in)

```go
// RawEventSink receives raw harness frames from adapters that opt to expose
// them (FR-23). The frame is the verbatim wire bytes; source names the
// wire format. Nil RawSink on HarnessSessionRequest disables raw forwarding.
type RawEventSink interface {
    EmitRaw(source string, frame []byte)
}
```

`source` values are stable strings: `"claude-stream-json"`, `"codex-json"`, `"opencode-line"`, `"acp-update"`. Defined as constants in `pkg/harness/raw.go` so tests can pin them.

### 3.7 ToolExecutor (ACP only)

```go
// ToolExecutor is the ACP-side bridge: the harness asks for fs/terminal
// operations via JSON-RPC, the adapter routes them through this interface,
// which is implemented by pkg/harness/sandbox/. Non-ACP adapters receive
// a nil ToolExecutor and never call it.
type ToolExecutor interface {
    ReadTextFile(ctx context.Context, req ReadFileRequest) (ReadFileResponse, error)
    WriteTextFile(ctx context.Context, req WriteFileRequest) (WriteFileResponse, error)

    CreateTerminal(ctx context.Context, req CreateTerminalRequest) (CreateTerminalResponse, error)
    TerminalOutput(ctx context.Context, req TerminalOutputRequest) (TerminalOutputResponse, error)
    WaitForTerminalExit(ctx context.Context, req WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error)
    KillTerminal(ctx context.Context, req KillTerminalRequest) error
    ReleaseTerminal(ctx context.Context, req ReleaseTerminalRequest) error
}

type ReadFileRequest  struct{ Path string; Line *int; Limit *int }
type ReadFileResponse struct{ Content string }
type WriteFileRequest struct{ Path, Content string }
type WriteFileResponse struct{}

type CreateTerminalRequest struct {
    Command string
    Args    []string
    Env     map[string]string
    Cwd     string  // sandbox enforces this is inside root
}
type CreateTerminalResponse struct { TerminalID string }
// ... TerminalOutput/Wait/Kill/Release request and response shapes mirror ACP SDK 1:1
```

Sandbox-enforced: every `Path` and `Cwd` is resolved against the request's sandbox root via `sandbox.Resolve(root, path)` which returns `ErrEscape` for any traversal that lands outside root after symlink resolution (FR-38, FR-40). The adapter never sees a path it could escape with.

`fs/list_dir` is **not** in `acp-go-sdk@v0.13.0` and therefore not in `ToolExecutor`. When the SDK exposes it, add the method.

### 3.8 PermissionRequester (ACP only)

```go
// PermissionRequester is how the ACP adapter forwards harness permission
// requests to the runtime's gate stack: team.Permissions() → agent
// PermissionPolicy → TUI prompt (FR-34, FR-37).
//
// The adapter calls Request synchronously from inside its ACP
// RequestPermission handler. The implementation MUST resolve within 30s
// or return ErrTimeout, which the adapter maps to RunError{code: permission_denied}.
type PermissionRequester interface {
    Request(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

type PermissionRequest struct {
    RequestID string
    Operation string // "fs/write_text_file", "terminal/create"
    Target    string // path or command
    Reason    string
    AgentName string // for policy lookup
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

The implementation lives in `pkg/runtime/`: it consults `team.Permissions()`, then `agent.Harness().PermissionPolicy`, then emits a `ToolCallConfirmationEvent` to the parent's `EventSink` and waits on `r.resumeChan` for the user's reply, then emits `AuthorizationEvent` + the canonical `PermissionResolved` event. This is the bridge that satisfies FR-37 ("TUI MUST use ToolCallConfirmationEvent"): the harness path reuses the model-backed permission UI verbatim.

### 3.9 HarnessConfig (config schema type)

See §2.3. Lives in `pkg/config/latest/types.go`. Validated in `pkg/config/latest/validate.go`.

### 3.10 Session.HarnessSession field

See §2.6. `map[string]string` keyed by agent name, value is the adapter-opaque token. Serializes through `Session`'s existing JSON encoding (FR-26).

---

## 4. Data flow

### 4.1 Invoking a harness-backed subagent

```
Orchestrator (model-backed) emits a tool call:
  transfer_task{agent: "code-reviewer", task: "review this diff"}

    ↓ runtime.go: loop.go dispatcher matches tool name "transfer_task"

handleTaskTransfer(ctx, sess, toolCall, evts)
  parses args
  validates target is in current agent's sub_agents
  opens OTel span "runtime.task_transfer"
  builds delegationRequest{SubSessionConfig{…}, SwitchCurrentAgent: true}
  calls runForwarding(ctx, sess, evts, req)

    ↓ runForwarding sees child.HasHarness() == true

runHarnessForwarding(ctx, parent, evts, callerAgent, child, req)
  opens OTel span "runtime.harness_session"
  r.telemetry.RecordHarnessStart(child.Harness().AdapterName, child.Name())
  loads adapter:  harness.LookupAdapter(child.Harness().AdapterName)
  resolves WorkingDir, Env (sandbox.AllowedEnv applied)
  loads PriorTurns from parent.GetAllMessages() if SupportsMultiTurn
  loads SessionToken from parent.HarnessSession[child.Name()]
  builds HarnessSessionRequest{Task, SystemPrompt, SessionToken,
    WorkingDir, Env, PriorTurns, Timeout, Spec,
    Events: fsm.NewEnforcer(translateSink{evts, parent, child, r}),
    RawSink: nil,  // wired by --debug flag in CLI
    Tools: sandbox.NewToolExecutor(workingDir),
    Permission: &runtimePermissionRequester{r, parent, child, evts},
  }
  spawns adapter goroutine:  go adapter.Run(ctx, req)
  drains the fsm-validated event stream:
    - on RunStart:      translate → runtime.StreamStartedEvent → evts.Emit
    - on TextDelta:     accumulate; emit no runtime event (the model-backed
                        path also doesn't emit per-token; PartialToolCall is
                        analogous)
    - on TextEnd:       translate → runtime.MessageAddedEvent (persists assistant
                        message into the child session built by newSubSession)
    - on ToolCallStart: translate → runtime.ToolCallEvent
    - on ToolCallEnd:   translate → runtime.ToolCallResponseEvent
    - on PermissionPending: already handled by PermissionRequester before the
                        event surfaces; this is the post-hoc observability record
    - on Heartbeat:     no runtime event; resets TUI "thinking" indicator
    - on RunError:      translate → runtime.ErrorEvent (code mapped per
                        pkg/runtime/event.go ErrorCode constants); save and break
    - on RunEnd:        translate → runtime.SubSessionCompletedEvent + StreamStoppedEvent
                        persist SessionToken: parent.HarnessSession[child.Name()] = e.SessionToken
  fires r.executeSubagentStopHooks (parent agent's hooks, child name, accumulated text)
  r.telemetry.RecordHarnessFinish(...)
  span.End()
  returns tools.ResultSuccess(accumulatedText)  // or tools.ResultError on RunError
```

### 4.2 Event flow from harness subprocess to TUI

```
harness subprocess (claude --output-format stream-json)
  writes NDJSON to stdout
    ↓
adapter (pkg/harness/claude)
  bufio.Scanner reads lines
  parses each line into a Claude Code typed struct
  maps to canonical Event via parser.go's switch
    ↓ adapter.Run(ctx, req) calls req.Events.Emit(canonicalEvent)
fsm.Enforcer.Emit
  validates lifecycle/balance rules
  in dev builds: panics on violation
  in prod:       logs warning, drops event, continues
  delegates to translateSink.Emit on success
    ↓
translateSink.Emit (pkg/runtime, defined inline in harness_delegation.go)
  switches on Event concrete type, builds the runtime.Event(s) per FR-21 table
  may emit 0..N runtime events (e.g. TextDelta emits nothing; TextEnd emits MessageAddedEvent)
    ↓ parent.evts.Emit(runtimeEvent)
runtime EventSink (the parent session's stream channel)
  PersistenceObserver writes to session store
  TUI renders
```

### 4.3 Multi-turn sessions

**Native (Claude Code, ACP harnesses):**

```
Turn 1:
  parent.HarnessSession["code-reviewer"] == "" (or absent)
  HarnessSessionRequest.SessionToken == ""
  adapter starts fresh harness session (claude --print "...", or ACP session/new)
  on RunEnd: e.SessionToken == "abc-123"
  runtime writes parent.HarnessSession["code-reviewer"] = "abc-123"

Turn 2:
  parent.HarnessSession["code-reviewer"] == "abc-123"
  HarnessSessionRequest.SessionToken == "abc-123"
  adapter resumes: claude --resume abc-123 --print "..."
  on RunEnd: e.SessionToken == "abc-123" (or a new one; opaque to runtime)
  runtime writes the new value back
```

**Simulated (Codex, OpenCode CLI):**

```
Turn N:
  HarnessSessionRequest.PriorTurns = parent.GetAllMessages() (filtered to N
    most relevant; runtime supplies all, adapter caps by token budget)
  HarnessSessionRequest.SessionToken == "" always
  adapter prepends serialized PriorTurns to the task string until the token
    budget (default 50% of context window, configurable via Config struct)
  on exceeding 60% budget: adapter emits Warning event
  on exceeding 100%: adapter emits RunError{code: context_exhausted}
  on RunEnd: e.SessionToken == "" (no token to persist)
```

### 4.4 ACP permission prompt flow

```
Harness (Copilot) wants to write outside working_dir:

  Copilot sends JSON-RPC:  session/request_permission {operation: "fs/write_text_file", path: "/etc/passwd", reason: "..."}
    ↓
ACP base adapter's RequestPermission handler (pkg/harness/acp/base.go)
  builds harness.PermissionRequest{RequestID, Operation, Target: "/etc/passwd", Reason, AgentName}
  emits PermissionPending event to req.Events (observability record)
  calls req.Permission.Request(ctx, permReq)
    ↓
runtimePermissionRequester.Request (pkg/runtime/harness_delegation.go)
  Step 1: team.Permissions().Check("fs/write_text_file", "/etc/passwd") → if deny pattern matches: return PermissionDeny
                                                                                    if allow pattern matches: return PermissionAllow
  Step 2: child.Harness().PermissionPolicy.FSWrite ==
            "auto_allow":         return PermissionAllow (gated by IUnderstandTheRisk)
            "auto_deny":          return PermissionDeny
            "prompt" or "":       fall through
  Step 3: TUI prompt
    parent.evts.Emit(ToolCallConfirmationEvent{...})  // reuses model-backed TUI
    block on r.resumeChan
    on user reply: emit AuthorizationEvent + emit canonical PermissionResolved event
    return decision (PermissionAllow / PermissionDeny)
    on 30s timeout: return ErrTimeout
    ↓
ACP base adapter's RequestPermission handler
  receives decision (or error)
  replies to harness with ACP session/permission_response {selected: <Allow/Deny>}
  on ErrTimeout: emit RunError{code: permission_denied}; tear down (FR-13)
    ↓
Harness proceeds or fails based on the response
```

The key invariant: the TUI sees exactly the same event type (`ToolCallConfirmationEvent`) for harness permission prompts as for model-backed tool approval. No TUI changes required.

---

## 5. Technology decisions

### 5.1 Option B (canonical events public, translator at runtime boundary) vs Option A (runtime events used directly)

**Decision: Option B.**

Option A would have adapters emit `runtime.Event` (the existing union in `pkg/runtime/event.go`) directly. Pros: no translator needed; one event type system.

Option B has adapters emit `harness.Event` (the canonical 14-type union); the runtime translates at the boundary.

**Why Option B:**

1. **Decoupling.** Runtime events carry session-store and TUI-specific concerns (`MessageAddedEvent.Message *session.Message`, `SubSessionCompletedEvent.SubSession any`). Forcing adapters to construct these means adapters import `pkg/session`, `pkg/chat`, `pkg/tools`. That couples every adapter to the entire runtime surface and makes the conformance suite (FR-22, §9.4) impossible — the suite must run against the canonical types, not against runtime-shaped events full of pointers to session state.
2. **Stability.** Runtime events change as the TUI evolves (new fields on `TokenUsageEvent`, etc.). Canonical events are a versioned contract. Breaking the contract requires a config-version bump (v10 → v11), forcing intentionality.
3. **Conformance.** The 20-scenario conformance suite (PRD §9.4) records and replays canonical events. With Option A, the suite would record runtime events full of unserializable internal pointers; with Option B, it records canonical JSON.
4. **AG-UI alignment.** The 14 canonical events borrow AG-UI vocabulary (PRD §2 non-goal 7); when an AG-UI consumer eventually appears we can ship an `EmitAgUI` translator alongside `EmitRuntime` without touching adapters.

Cost: one translator function (`pkg/harness/translate.go` plus inline `translateSink` in the runtime). Maintainable: the FR-21 table is the spec; the translator is a 14-case switch.

### 5.2 Event as interface (discriminated union) vs fat struct

**Decision: discriminated union (interface + concrete types).**

The PRD appendix A already commits to this shape; the arch review reaffirmed it. The alternative — a `harness.Event` struct with `Kind` + every possible field optional — has known failure modes:

- Compile-time exhaustiveness: a Go type switch over interface values catches missing cases at review time. A `switch ev.Kind` over strings does not.
- Field correctness: `RunEnd.SessionToken` has no meaning on `TextDelta`. Encoding both in one struct invites bugs where adapters set wrong fields.
- JSON wire shape: each concrete type marshals cleanly with no `if Kind == "x"` carve-outs.

Cost: marshalling/unmarshalling event JSON requires a `Kind` field for the wire and a custom UnmarshalJSON. Standard pattern; ship a single helper in `event.go`.

### 5.3 `pkg/harness/acp/` as shared base for Copilot and OpenClaw

**Decision: shared base in `pkg/harness/acp/base.go`, with `copilot/` and `openclaw/` subpackages for adapter-specific concerns.**

Both adapters speak ACP over stdio via `acp-go-sdk.NewClientSideConnection`. The reused pieces:

- Connection lifecycle (handshake `initialize`, `session/new`, `session/prompt`, `Cancel` + SIGTERM/SIGKILL teardown).
- `SessionUpdate` → canonical event translation (the table in PRD §7.4).
- Permission flow via `PermissionRequester`.
- Filesystem and terminal operations via `ToolExecutor` (sandbox-enforced).
- Per-session capability negotiation against the harness's reported caps (FR-NEW-8).
- Process pool keyed by `(agent_name, working_dir)` for NFR-11.

Adapter-specific:

- Binary name (`copilot --acp` vs `openclaw`), env vars (`GITHUB_TOKEN` for Copilot, none for OpenClaw), typed `Config` struct, idle timeout, error-signal mapping table.

This is the same factoring used by `pkg/model/provider/openai` (shared base) and the OpenAI-compatible providers on top. Keep this consistent.

**Note: `pkg/acp/` (existing) is the SERVER-side implementation for `docker-agent serve acp`. `pkg/harness/acp/` is the CLIENT-side adapter that connects out to harness binaries.** They share the SDK but not the code. Do not collapse them.

### 5.4 Config version bump strategy

**Decision: snapshot then bump (FR-6).**

Alternatives considered:

- **Add `harness:` to v9 without bumping.** Rejected: v9 schemas in the wild that fail to parse new fields would error confusingly; consumers can't tell whether their config is "v9 with harness support" or "v9 without". The `Version` field is what determines that.
- **Bump to v10 in place; rely on `git blame` for v9 history.** Rejected: the upgrade path (`config.Upgrade`) needs both shapes simultaneously to convert a stored v9 document into v10.

The snapshot-then-bump pattern matches what's already in `pkg/config/v8/`, `pkg/config/v7/`, etc. v9 with `harness:` is impossible by construction (the field doesn't exist on `v9.AgentConfig`).

---

## 6. Risks

### 6.1 ACP `fs/list_dir` not in SDK v0.13.0

**Impact:** Sandbox enforcement covers only the methods the SDK exposes. Harnesses that need directory listings will get an `MethodNotFound` error from the adapter.

**Mitigation:** Drop `fs/list_dir` from v1 scope (PRD FR-38 already does). Add a TODO in `pkg/harness/acp/base.go` referencing the SDK feature request. When the SDK exposes the method, the contract additions are:

- `ToolExecutor.ListDir(ctx, ListDirRequest) (ListDirResponse, error)` — sandbox-enforced like `ReadTextFile`.
- One new branch in the ACP client method dispatch.

Tracking: file an issue against `github.com/coder/acp-go-sdk` after v1 ships; revisit at v1.1.

### 6.2 Static vs negotiated ACP capabilities

**Impact:** `AdapterCapabilities()` is declared by the adapter author at compile time. The harness reports its capabilities at runtime via `initialize`. These can disagree (the harness is older than the adapter expects, or the user has a non-standard build).

**Mitigation:** Document the split (FR-NEW-8) and enforce at session start:

- `Capabilities()` returns what the adapter knows how to use.
- `pkg/harness/acp/capabilities.go` reconciles after `initialize` returns. If the harness reports the feature the request needs is absent, emit `RunError{code: capability_mismatch, retryable: false}` immediately, never call the missing method.
- Test fixtures (FR-NEW-13) MUST include both: a session where capabilities match (happy path), and a session where the harness reports a capability gap (the `RunError` path).

**Open question deferred to impl:** should we also expose the negotiated caps on `RunEnd` for telemetry? Not in v1; add when a real consumer asks.

### 6.3 Pre-existing test failures (not our problem, document)

**Impact:** `status.json` records two pre-existing failures: `pkg/config TestCheckRequiredEnvVars` and `pkg/teamloader TestLoadExamples (dmr/unload_on_switch)`. These are unrelated to harness orchestration.

**Mitigation:** Track separately. The harness branch CI must:

- Not introduce new test failures.
- Not "fix" the existing failures as a side effect (that would conflate concerns).
- Flag the existing failures in the PR description so reviewers don't think we caused them.

A check in the impl plan: before any unit lands, capture the baseline test failure list. On PR open, compare. New failures = fix. Pre-existing failures = preserve and note.

### 6.4 CI runner provisioning for integration tests

**Impact:** FR-NEW-12 requires real harness binaries on CI runners, plus secrets for `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GITHUB_TOKEN`, plus a CI budget for paid API calls. Without this, Phase 2 cannot validate adapters end-to-end.

**Mitigation:**

- Phase 0 surfaces the requirement to the platform team in the kickoff meeting (PRD §10 critical-path dependency).
- Phase 1 unit and conformance tests do NOT require real binaries — they run against `testdata/` fixtures via `pkg/harness/replay/` (FR-NEW-13). This means Phase 1 can land and validate independently.
- Phase 2 integration tests are gated by a build tag (`//go:build integration_harness`) and a per-adapter env-var check. CI runners without the binaries or secrets skip those tests with a clear "integration env not provisioned" log line, not a failure.
- If CI provisioning slips past the Phase 1 → Phase 2 boundary, Phase 2 ships with replay coverage only and integration tests run nightly off a developer machine until CI catches up. This is an escalation, not a silent slip — the PR description must state the gap.

### 6.5 Out-of-band: harness binary version drift

**Impact:** A user upgrades their `claude` binary from 1.2.3 to 2.0.0 and the stream-json format changes. The adapter's parser breaks; we emit `protocol_error` for every event.

**Mitigation:** `HostRequirements.MinVersion` is enforced at adapter Run start (each adapter runs `<binary> --version` once and parses; mismatch → `RunError{code: binary_version_mismatch}`). This is documented behavior, not silent failure. Long-term: pin tested binary versions in CI and update the parser when the upstream format changes.

### 6.6 Out-of-band: orphan process leak

**Impact:** Adapter crashes mid-Run; its child harness process is not reaped; the next `docker-agent` session inherits a zombie.

**Mitigation:** FR-13 cleanup order (Cancel → SIGTERM → wait 5s → SIGKILL) is enforced in `pkg/harness/sandbox/` (or per-adapter for non-ACP). A `goleak`+process-orphan integration test at Phase 3 (1000 consecutive runs, no zombies) is the verification gate. P0 to fix before GA.

---

## 7. Cross-references

- **PRD §1.2 file list:** every file enumerated there appears in §2 of this spec.
- **PRD §4 functional requirements:** numbered FRs are referenced inline at the relevant component or interface.
- **PRD §7 adapter specs:** the per-adapter event-mapping and error-mapping tables ARE the binding contract; this spec does not re-derive them. The interfaces in §3 of this spec are what the adapters consume.
- **PRD §10 implementation phases:** mirrored by the impl plan; this spec is phase-agnostic.
- **PRD appendix A:** every interface there is in §3 here, refined into compilable Go. Differences:
  - `EventSink.Emit` returns `error` (this spec) vs `void` (PRD appendix A). Reason: FSM enforcer needs a return value path. Wire-incompatible only with the appendix's sketch; the appendix says "Final shapes live in the arch spec," so this spec is authoritative.
  - `Event` interface has 4 methods (`isHarnessEvent`, `GetSessionID`, `GetAgentName`, `GetTimestamp`) vs PRD's 1. Reason: every event must satisfy `runtime.SessionScoped` and `runtime.Event` at the translator boundary; eager interface satisfaction is the cleanest path.
  - 14 concrete event types vs PRD's "12 canonical events." See §3.4 note. Wire shape compatible.
