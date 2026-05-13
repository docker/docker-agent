# DX Review: Cross-Harness Orchestration Architecture Spec

**Reviewer lens:** Solomon Hykes (simplicity, composability, default path = right path) + Anders Hejlsberg (type elegance, progressive disclosure, pit of success).

**Source:** `arch-spec.md` §3 + §2.3, cross-checked against `prd-v2.md` §4 (FRs) and §9 (adapter author guide).

**Audience this spec must serve:**
1. **Adapter authors** — engineer implementing the 6th adapter (Cursor) in 2026 without the original team.
2. **Runtime consumers** — engineer wiring `pkg/runtime` to the harness package.
3. **Config authors** — Mark writing YAML and getting a fast, clear error when he typos a field.

## Verdict: **SUGGESTIONS** (do not start implementation as-specified)

The shape is right: discriminated union for events, pure `Capabilities()`, process-per-session, sandbox in shared code, replay infrastructure. Solomon would call this composable. Anders would approve of the sealed interface and typed enums.

But there are **five blocking issues** that will burn the second adapter author, and several footguns that compile-time Go could catch but currently won't. Fix these before Phase 0 starts. The cost is hours; the benefit is every future adapter.

---

## 1. `HarnessAdapter` interface

```go
type HarnessAdapter interface {
    Name() string
    Capabilities() AdapterCapabilities
    Run(ctx context.Context, req HarnessSessionRequest) error
}
```

### What's right
- Three methods. No more. Hejlsberg would approve — the minimum surface.
- `Run(ctx, req) error` is the standard Go shape. Adapter authors can't get the signature wrong.
- `Capabilities()` returning a value (not a pointer) signals purity at the type level. Good.

### What's wrong

**Issue 1.1 — `Capabilities()` purity is unenforceable and undocumented in the type.** The spec says "Pure function: no I/O, no process spawn, safe to call at config-load time (FR-10)." But Go has no way to express this. A first-time adapter author will absolutely call `exec.LookPath` inside `Capabilities()` because that's where they're thinking about the binary. The teamloader will then call it under a mutex during config load. Then someone calls it from the `harness describe` CLI without expecting blocking I/O.

**Fix:** Make this structural, not aspirational. Two options:

- **Option A (preferred):** Split into `Adapter` and `AdapterFactory`. `AdapterFactory.Describe() AdapterCapabilities` is the pure part. `AdapterFactory.New(...) Adapter` is where instantiation happens. `Adapter.Run(...)` runs the session. This is the gstack/dagger pattern.
- **Option B:** Move `Capabilities` to a package-level function registered alongside the adapter: `harness.Register(name, factory, capabilities)`. Now `Capabilities` is a static value, not a method — it cannot do I/O by construction.

Recommend Option B. Smaller diff, same guarantee.

**Issue 1.2 — `Name()` duplicates the registry key.** The adapter is registered as `"claude-code"` in the registry, and `Name()` must return `"claude-code"`. Two sources of truth. First-time author will return `"claude"` and wonder why teamloader can't find it.

**Fix:** Pass `name` to the registry; remove `Name()` from the interface. The registry owns the name; the adapter is anonymous.

```go
// registry.go
func Register(name string, capabilities AdapterCapabilities, factory func() Adapter)
```

This collapses two truths into one.

**Issue 1.3 — `Run` returns an error that is "reserved for adapter-internal bugs where the event sink is unreachable" (FR-NEW-10). The spec then says the runtime silently converts it to `ErrorEvent{code: harness_crashed}`.**

This is a footgun. The adapter author looks at `func Run(...) error` and assumes the standard Go contract: "return an error when something goes wrong." They will. Then their errors will be silently swallowed and translated to a generic code that hides what actually broke.

**Fix:** Make `Run` return `void` (no return value). All terminal states flow through `RunError` events. If the event sink is unreachable, that is unrecoverable and the adapter should `panic` — there is no caller who can act on the error anyway. Document `Run` as "MUST emit exactly one terminal event before returning."

```go
type Adapter interface {
    Run(ctx context.Context, req SessionRequest)
}
```

This forces the pit of success. Mistake an adapter author can no longer make: returning `fmt.Errorf("rate limit hit")` instead of emitting `RunError{Code: ErrRateLimited}`.

### Most likely first-mistake by an adapter author

Returning a Go `error` from `Run` instead of emitting `RunError`. The spec's FR-NEW-10 acknowledges this happens and papers over it with silent conversion. The type system should make it impossible.

---

## 2. `Event` discriminated union

```go
type Event interface {
    isHarnessEvent()
    GetSessionID() string
    GetAgentName() string
    GetTimestamp() time.Time
}
```

### What's right
- **Sealed interface via unexported method.** Textbook Go discriminated union. External packages cannot add new event kinds. Hejlsberg-approved.
- **Embedded `EventMeta`** removes the boilerplate of repeating SessionID/AgentName/Timestamp on every concrete type.
- **14 concrete types** with `Start/Delta/End` naming. Consistent.

### What's wrong

**Issue 2.1 — Go does not give exhaustiveness checking on type switches.** A consumer's type switch:

```go
switch e := ev.(type) {
case harness.RunStart:
    ...
case harness.TextDelta:
    ...
// forgot Heartbeat. compiler is silent.
}
```

This is **the** weakness of Go's discriminated unions vs. Rust enums or TypeScript discriminated unions. The spec doesn't address it.

**Fix (two parts):**

1. **Use `exhaustruct`/`go-exhaustive` linter.** Add to CI. `golangci-lint` has `exhaustive` which checks type switches over interfaces if you tag the interface. This is the closest Go gets to Hejlsberg's compiler.

2. **Provide a visitor helper in `pkg/harness/visit.go`:**

```go
type Visitor struct {
    OnRunStart           func(RunStart)
    OnRunEnd             func(RunEnd)
    OnRunError           func(RunError)
    OnTextStart          func(TextStart)
    OnTextDelta          func(TextDelta)
    OnTextEnd            func(TextEnd)
    OnReasoningStart     func(ReasoningStart)
    OnReasoningDelta     func(ReasoningDelta)
    OnReasoningEnd       func(ReasoningEnd)
    OnToolCallStart      func(ToolCallStart)
    OnToolCallEnd        func(ToolCallEnd)
    OnPermissionPending  func(PermissionPending)
    OnPermissionResolved func(PermissionResolved)
    OnHeartbeat          func(Heartbeat)
}

func (v Visitor) Visit(e Event) { /* type switch with fallbacks */ }
```

Adapter authors and consumers who use the visitor get a struct-literal that flags missing cases via `go vet -exhaustruct`. Use is opt-in; the raw type switch still works for performance-critical paths.

**Issue 2.2 — `Event` interface methods are an over-spec.** The spec adds `GetSessionID`, `GetAgentName`, `GetTimestamp` to make every event implement `runtime.SessionScoped`. This is the right intent. But:

- The names break Go convention. Idiomatic Go is `SessionID()` not `GetSessionID()`. (Hejlsberg note: this is Java-isms; Solomon's Docker codebase uses idiomatic Go.) Check `pkg/runtime/event.go` — if the existing interface uses `GetX`, fine. If not, this is noise.

- `GetTimestamp() time.Time` returning a value is good, but consider: when does an event have a zero `Timestamp`? FSM enforcer should reject zero-timestamp events at the boundary; otherwise the field is a bug magnet. State this in the FSM contract.

**Fix:** Drop `Get` prefix unless `pkg/runtime/event.go` uses it (consistency wins over idiom in a single codebase). Add to FSM enforcer: reject `Timestamp.IsZero()`.

**Issue 2.3 — The 14 vs 12 event-count discrepancy with the PRD is real.** The arch spec calls it out (line 526) but defaults to 14 in code. The PRD's "12" is wrong — `PermissionPending`/`PermissionResolved` are distinct event types in the spec, that's correct. **Fix the PRD wording**, don't introduce code-vs-PRD drift on day one. Future readers will trust whichever they read first.

### Consumer ergonomics — type switch

A consumer doing the translator looks like:

```go
func (s translateSink) Emit(e harness.Event) error {
    switch ev := e.(type) {
    case harness.RunStart:
        s.evts.Emit(&runtime.StreamStartedEvent{...})
    case harness.TextEnd:
        s.evts.Emit(&runtime.MessageAddedEvent{Message: buildMsg(ev)})
    case harness.RunError:
        s.evts.Emit(&runtime.ErrorEvent{Code: mapCode(ev.Code), ...})
    case harness.RunEnd:
        s.evts.Emit(&runtime.SubSessionCompletedEvent{...})
        s.evts.Emit(&runtime.StreamStoppedEvent{...})
    // 10 other cases that emit nothing
    default:
        return nil  // ignore unknown
    }
    return nil
}
```

This is fine, but the `default: return nil` is a footgun. If we add a 15th canonical event in v1.1, every translator silently drops it. **Fix:** the FSM enforcer should reject anything not in the 14-set; consumers can then `panic("unreachable")` on default and `go vet` flags missing cases via `exhaustive`.

---

## 3. `HarnessSessionRequest`

```go
type HarnessSessionRequest struct {
    SessionID    string
    AgentName    string
    Task         string
    SystemPrompt string
    SessionToken string
    WorkingDir   string
    Env          map[string]string
    PriorTurns   []chat.Message
    Timeout      time.Duration
    Spec         *agent.HarnessSpec
    Events       EventSink
    RawSink      RawEventSink
    Tools        ToolExecutor
    Permission   PermissionRequester
}
```

### What's right
- Plain struct, no constructor magic. Solomon-approved.
- `Spec *agent.HarnessSpec` gives the adapter access to its own typed config without a separate registry round-trip.
- Optional fields (`RawSink`, `Tools`, `Permission`) are pointers/interfaces that allow nil. Clear opt-in.

### What's wrong

**Issue 3.1 — 14 fields, mixed concerns.** The struct mixes:
- **Identity** (SessionID, AgentName)
- **Task** (Task, SystemPrompt, PriorTurns)
- **Resume** (SessionToken)
- **Environment** (WorkingDir, Env, Timeout)
- **Wiring** (Events, RawSink, Tools, Permission)
- **Config** (Spec)

This is the "fat struct" anti-pattern the spec explicitly rejected for `Event` but adopted for the request. A first-time adapter author looking at this struct will not know which fields they're expected to read, which are advisory, and which are nil-by-default-for-them.

**Fix — split by concern, optional via embedding:**

```go
type SessionRequest struct {
    Session  SessionInfo       // ID, AgentName
    Task     TaskInput         // Task, SystemPrompt, PriorTurns, SessionToken
    Env      Environment       // WorkingDir, Env, Timeout
    Config   any               // adapter-typed; see §6
    Sinks    Sinks             // Events (req), RawSink (opt)
    ACP      *ACPBindings      // nil for non-ACP adapters — see §5
}

type ACPBindings struct {
    Tools      ToolExecutor       // never nil if struct present
    Permission PermissionRequester
}
```

Now `ACPBindings` being non-nil **is** the signal "you are an ACP adapter, you must use these." A non-ACP adapter that received a non-nil `ACPBindings` would have a clear bug at construction time, not at use time. See §5 for why this matters.

**Issue 3.2 — `Spec *agent.HarnessSpec` couples `pkg/harness` to `pkg/agent`.** The arch spec acknowledges this and says "the cycle is one-way." It is. But adapter authors will reach into `Spec.Config` (the typed config struct) by import-cycling through `pkg/agent`. The right shape is:

```go
type SessionRequest struct {
    // ...
    Config any  // adapter unmarshals via type assertion to its own typed Config
}
```

Drop `Spec` entirely from `SessionRequest`. Adapters need `Config`, not the full spec. The runtime can read `Spec` for its own purposes (PermissionPolicy, etc.) without passing it to the adapter. Tighter contract; less surface to misuse.

**Issue 3.3 — `PriorTurns []chat.Message` is the wrong name.** "Prior turns" suggests conversation history, but in practice this is "context to inject for simulated multi-turn." A native multi-turn adapter (Claude Code via `--resume`) should **not** read `PriorTurns` — the harness has its own history. The current name doesn't convey that.

A first-time adapter author writing the Claude Code adapter will:
1. See `PriorTurns []chat.Message` in the struct.
2. Think "I should serialize this and prepend to my prompt."
3. Double-feed history (`--resume <token>` PLUS prepended messages).
4. Spend 4 hours debugging why Claude has 2x context.

**Fix — rename and split:**

```go
type TaskInput struct {
    Task         string
    SystemPrompt string
    // ResumeToken is set for native multi-turn adapters. Empty on first turn or
    // if the adapter declares SupportsMultiTurn=false.
    ResumeToken  string
    // SimulatedHistory is set ONLY when the adapter declared SupportsMultiTurn
    // via prompt-prepending (Codex, OpenCode CLI). Empty for native adapters.
    SimulatedHistory []chat.Message
}
```

Now the runtime is responsible for populating exactly one of `ResumeToken` or `SimulatedHistory` based on the adapter's declared strategy, and the adapter knows by **which field is non-empty** what to do. Pit of success.

**Issue 3.4 — `Env map[string]string` is unfiltered at this layer.** The spec says "filtered through sandbox.AllowedEnv (FR-41)" — but if filtering happens before the struct is built, that's correct. If after, the adapter receives raw env and must filter. The spec is ambiguous.

**Fix:** State explicitly in the field comment that `Env` is post-allowlist. Add a runtime assertion (panic in dev) if any key in `Env` is not in the allowlist when the request is constructed.

### Most confusing fields for a new adapter author
1. `PriorTurns` — see 3.3.
2. `SessionToken` vs new session — no docstring on what empty-string means.
3. `Tools` / `Permission` — only-ACP-but-not-typed-as-such. See §5.

---

## 4. `AdapterCapabilities` with `Requires` and `Features`

```go
type AdapterCapabilities struct {
    Protocol     ProtocolClass
    Requires     HostRequirements
    Features     AdapterFeatures
    BuiltInTools []string
    IdleTimeout  time.Duration
}
```

### What's right
- `Protocol` as typed enum: good.
- Splitting `Requires` (host needs this) from `Features` (adapter offers this) is the right conceptual cut.
- `IdleTimeout` per-adapter is correct — Copilot needs 10m, OpenClaw needs 2m, and that's an adapter property not a config property.

### What's wrong

**Issue 4.1 — `Requires` vs `Features` split is correct but the naming makes you guess.** A first-time author will ask: "Does `MinVersion` go in `Requires` or `Features`?" It's in `Requires` but is also visible to the user (the user can override it). "Does `SupportsMultiTurn` belong in `Features` or in `Requires` (the harness must support it)?" Ambiguous on read.

**Fix — rename for direction-of-fit:**

```go
type AdapterCapabilities struct {
    Protocol    ProtocolClass
    // HostNeeds: what must be true of the host environment before the adapter can run.
    HostNeeds   HostRequirements
    // AdapterOffers: what the adapter can do for the caller.
    AdapterOffers AdapterFeatures
    BuiltInTools []string
    IdleTimeout time.Duration
}
```

Or simply: `Requirements` and `Capabilities` (but then the outer type name clashes). Or `Needs` and `Provides`. Anything that makes the direction obvious. `Requires`/`Features` is borderline; explicit is better.

**Issue 4.2 — `BuiltInTools []string` is hostile to consumers.** What is this for? The spec says "informational; not enforced." Adapter authors won't know whether to populate it or leave it empty. Consumers won't know whether to trust it.

A `[]string` with no schema is the worst case: every adapter populates it differently (`"shell"`, `"bash"`, `"terminal"`, `"Terminal"`). Tools have no canonical names across harnesses.

**Fix — drop it from v1.** If we need to surface available tools, add a structured type later (`type ToolDescription struct { Name string; Kind ToolKind; ... }`). The PRD's only use case ("informational") doesn't justify the field. YAGNI.

**Issue 4.3 — No way to express "I support this only under condition X."** Per-session capability negotiation (FR-NEW-8) for ACP is a real concern. The spec handles it by emitting `RunError{capability_mismatch}` from inside `Run`. But the user-facing surface `docker-agent harness describe` will print the static capabilities and lie about what an ACP harness can actually do until it talks to the binary.

**Fix:** Add a documented note on the field: `AdapterOffers reflects the adapter's intent; ACP adapters may downgrade at session start.` And ensure `harness describe` prints a warning footer for ACP types.

### Most likely first-mistake
Populating `BuiltInTools` with adapter-specific strings nobody downstream uses.

---

## 5. `ToolExecutor` and `PermissionRequester` (the ACP-only footgun)

```go
type ToolExecutor interface { /* fs + terminal methods */ }
type PermissionRequester interface {
    Request(ctx, PermissionRequest) (PermissionDecision, error)
}
```

These are currently fields on `HarnessSessionRequest`:

```go
Tools      ToolExecutor       // ACP only; nil for non-ACP
Permission PermissionRequester // ACP only; nil for non-ACP
```

### What's wrong

**Issue 5.1 — Nothing in the type system distinguishes ACP from non-ACP adapters.** A non-ACP adapter (Claude Code) that receives a non-nil `Tools` won't break, because it won't call them. But:

1. The runtime might pass a non-nil `Tools` to a non-ACP adapter by accident.
2. An ACP adapter might receive a nil `Tools` and segfault on first `fs/read_text_file`.
3. There's no compile-time signal of "this is the ACP fork."

The PRD's appendix A had the right shape:

```go
type ACPRequest interface {
    ToolExecutor() ToolExecutor
    Permission() PermissionGate
}

// Adapters use type assertion: if acp, ok := req.(ACPRequest); ok { ... }
```

The arch spec abandoned this for a flat struct. **The PRD was right.** Use the interface.

**Fix — push ACP-ness into the type system. Three options, in order of preference:**

**Option A (preferred): Separate adapter interfaces.**
```go
type Adapter interface {
    Run(ctx, SessionRequest)
}

type ACPAdapter interface {
    Adapter
    RunACP(ctx, SessionRequest, ACPBindings)
}
```
The runtime dispatches on `if acp, ok := adapter.(ACPAdapter); ok { ... }`. ACP adapters cannot run without bindings. Non-ACP adapters cannot accidentally receive them.

**Option B: Embed bindings in request as a typed sub-struct.**
```go
type SessionRequest struct {
    // ...
    ACP *ACPBindings  // non-nil iff adapter.Protocol == ProtocolACP
}

type ACPBindings struct {
    Tools      ToolExecutor
    Permission PermissionRequester
}
```
The runtime sets `ACP != nil` only when dispatching to an ACP adapter, gated by `Capabilities().Protocol`. Adapter checks `req.ACP != nil` for "am I in ACP mode." Slightly worse than A (still a runtime check), but smaller refactor.

**Option C: Status quo with documented panic-on-misuse.** Worst option but acceptable if A/B are too much: define `harness.NilToolExecutor` and `harness.NilPermissionRequester` that panic with "this adapter declared Protocol=stream but called ToolExecutor; this is a bug." Then runtime always passes non-nil; non-ACP adapters get panicking stubs.

Recommend Option A. It's a 50-line refactor that removes a whole class of bugs.

**Issue 5.2 — `PermissionRequester.Request` returns `error` for what should be a typed result.**

```go
Request(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
```

What's the error? Timeout (FR-37: 30s). Context cancellation. Maybe TUI failure. The adapter's job is to convert this to `RunError{code: permission_denied}`. The error type is unstructured.

**Fix:** Return a typed result:
```go
type PermissionResult struct {
    Decision PermissionDecision  // Allow | Deny
    Scope    PermissionScope     // Once | Session
    Reason   string              // optional, for audit
}

// Returns Deny on timeout (with Reason="timeout").
// Returns context.Canceled error only on ctx cancellation; nothing else.
Request(ctx, PermissionRequest) (PermissionResult, error)
```

Now there's exactly one error path (context cancel), and timeout is in-band. The adapter's logic becomes obvious: `if err != nil { /* cancellation */ }; if result.Decision == Deny { /* refuse */ }`.

**Issue 5.3 — `ToolExecutor`'s response types are inconsistent.** `WriteFileResponse struct{}` is empty. `KillTerminal` returns `error` only. `ReadFileResponse` has `Content`. Some signature inconsistency mirrors the ACP wire shape, but adapters will be confused: when is the response empty vs typed?

**Fix:** Document the convention. Either "all methods return `(Response, error)` even when Response is empty" (for future-proofing — add fields without breaking) or "void methods return `error` only." Pick one. The current spec mixes both.

---

## 6. `HarnessConfig` and adapter-specific knobs

```go
type HarnessConfig struct {
    Type             string
    Command          string
    Args             []string
    Env              map[string]string
    WorkingDir       string
    Timeout          Duration
    MinVersion       string
    PermissionPolicy *PermissionPolicyConfig
    Config           map[string]any
}
```

### What's right
- `Type` as enum-validated string is correct for YAML.
- `Config map[string]any` at the schema layer is the right boundary — adapters register typed structs that the teamloader unmarshals into.
- `yaml.DisallowUnknownField` at unmarshal time (FR-5) gives the user a load-time error on typos. Excellent DX.

### What's wrong

**Issue 6.1 — `Config map[string]any` is the right type *at the schema layer*, but the user-facing DX depends entirely on how typed structs are registered and how errors surface.** The arch spec says (§2.7):

> Unmarshal `agentConfig.Harness.Config` (raw `map[string]any`) into the adapter's typed config struct using `yaml.DisallowUnknownField()`.

This is half-right. Two problems:

1. **YAML library mismatch.** `gopkg.in/yaml.v3` doesn't have `DisallowUnknownField`. That's `encoding/json`'s `Decoder.DisallowUnknownFields()`. If the spec means "round-trip through JSON," fine — but say so. If it means "implement equivalent for YAML," that's a non-trivial piece of code (yaml.v3 has `KnownFields(true)` on the decoder; works similarly). Specify the exact API call.

2. **The error message format is unspecified.** A user typing `max_tunrs: 20` (typo) under `harness.config` for Claude Code needs an error like:

   ```
   agent "code-reviewer": unknown field "max_tunrs" in harness.config
     (claude-code adapter accepts: max_turns, system_append, ...)
     at team.yaml:42
   ```

   Not:
   ```
   yaml: unmarshal errors: line 42: field max_tunrs not found in type claude.Config
   ```

   The arch spec doesn't pin this. The PRD §6.4 mentions `docker-agent config validate` should "reject unknown harness.config keys with a clear error pointing at the offending line." Make the spec say: error includes (1) agent name, (2) offending key, (3) accepted keys list, (4) file:line.

**Fix:** Add to §3.9 a sub-section "Config error format" with the literal expected error string. Adapter authors will copy it.

**Issue 6.2 — `Command` + `Args` + `Env` + `WorkingDir` on `HarnessConfig` overlap with what `Capabilities().Requires` already says.** A user CAN override `command` to point at their own binary. Fine. But what's the precedence between `harness.command` and `Capabilities().Requires.Binary`? The spec says (line 100): `Command          string                // optional binary path override; "" => use Capabilities().Requires.Binary`. Good.

What about `Args`? The spec says: `Args []string  // appended to adapter defaults`. So user `args` are appended to adapter defaults? Or replace? "Appended" is a footgun: a user setting `args: ["--print", "hello"]` for Claude Code will get the adapter's `--print "<task>"` AND their `--print "hello"`. Conflict.

**Fix:** Either change to "replaces adapter defaults" or define a clear merge policy (e.g., user args come after adapter args, and last-wins for repeated flags). Prefer **"user args augment but cannot override reserved flags"** with a documented list of reserved flags per adapter.

**Issue 6.3 — `MinVersion` lives on both `HarnessConfig` and `HostRequirements`.** User can override; adapter declares default. Same pattern as `Command`. Make the override precedence explicit in the type comments.

**Issue 6.4 — `PermissionPolicy` on a non-ACP adapter should be a validation error.** The spec doesn't say. A user putting `permission_policy:` under a `claude-code` agent will get... what? Silently ignored? Load-time error?

**Fix:** Validate at teamloader: if `Capabilities().Protocol != ProtocolACP` and `HarnessConfig.PermissionPolicy != nil`, fail load with "permission_policy is only valid for ACP-protocol harnesses; claude-code uses streaming protocol."

### YAML DX

The example configs (PRD §6.2) look fine to me. The "one-liner" minimal case (`type: claude-code` and done) hits the right pit of success. Solomon would say: the simple case is simple. Good.

The Codex example with `reasoning_effort: high` works only if the adapter's typed config has that field as a typed enum, not a string. Make sure the typed config struct uses a custom enum type:

```go
type ReasoningEffort string
const (
    ReasoningLow    ReasoningEffort = "low"
    ReasoningMedium ReasoningEffort = "medium"
    ReasoningHigh   ReasoningEffort = "high"
)
```

So a user typing `reasoning_effort: max` gets a load-time error naming the legal values.

---

## 7. The translator (`pkg/harness/translate.go`)

The arch spec says (line 32):
> `translate.go              // harness.Event → runtime.Event translator (Option B boundary)`

And §2.5 says the translator runs inside `runHarnessForwarding` as part of an `EventSink` chain: `fsm.NewEnforcer(translateSink{evts, parent, child, r})`.

### What's wrong

**Issue 7.1 — The translator is in `pkg/harness/` but needs to construct `runtime.Event` types (`MessageAddedEvent`, `SubSessionCompletedEvent`, etc.) and writes to `parent.HarnessSession`.** That makes `pkg/harness` import `pkg/runtime` AND `pkg/session`. The arch spec (line 74) explicitly says:

> `pkg/harness` is imported by `pkg/runtime` (for the discriminated-union types, translator, FSM, registry lookup)

But the translator constructing `runtime.Event` would create a circular import: `pkg/runtime` imports `pkg/harness` (for types), and `pkg/harness` imports `pkg/runtime` (for translator output).

§4.2 of the arch spec says:
> `translateSink.Emit (pkg/runtime, defined inline in harness_delegation.go)`

So the translator is actually in `pkg/runtime`, not `pkg/harness/translate.go`. **The file `pkg/harness/translate.go` is misnamed** or has different content than the consumer-facing translator. This is a real inconsistency in the spec.

**Fix:** Pick one location and be explicit:

- **Option A (preferred):** Translator lives in `pkg/runtime/harness_translate.go`. `pkg/harness/translate.go` does NOT exist. The "Option B boundary" sits inside the runtime, which already imports `pkg/harness`.
- **Option B:** `pkg/harness/translate.go` exposes a pure function `Translate(e Event) []RuntimeEventConstructor` that returns thunks rather than full events, and the runtime closes over `parent`/`child` to materialize them. More complex.

§4.2 already commits to Option A in spirit. **Update §2.1 and §3 to remove `pkg/harness/translate.go`** and rename to `pkg/runtime/harness_translate.go`. Otherwise a second adapter author will look in the obvious place and find a stub.

**Issue 7.2 — Who calls the translator is clear; the contract on what it emits is not.** FR-21 has a 4-row table (StreamStarted, MessageAdded, SubSessionCompleted, StreamStopped). But the arch spec §4.1 lists more triggers (ToolCallEvent on ToolCallStart, ToolCallResponseEvent on ToolCallEnd, ErrorEvent on RunError). The two contracts disagree about which runtime events are emitted.

**Fix:** Reconcile. Make the full canonical→runtime mapping table a single source of truth in **one** place (likely PRD §4.3 FR-21 expanded, then arch spec references it). Currently it's split between PRD FR-21 and arch §4.1 data flow.

**Issue 7.3 — Translator interface for consumers is undocumented.** Adapter authors don't call the translator. Runtime authors do. But what's the test surface? If I want to assert "for this canonical event sequence, the translator emits this runtime event sequence" — what do I import? The current spec doesn't define a callable interface.

**Fix:** Add to spec:
```go
// pkg/runtime/harness_translate.go
type Translator interface {
    Translate(e harness.Event, ctx TranslationContext) []runtime.Event
}

type TranslationContext struct {
    Parent     *session.Session
    Child      *agent.Agent
    Now        func() time.Time
    Accumulate *TextAccumulator // for assembling streaming TextDeltas into MessageAdded
}
```

Now there's a testable surface. A unit test in `pkg/runtime` can construct a context, feed canonical events, assert runtime events.

---

## 8. Missing DX concerns the spec doesn't address

These are what will burn the **second** adapter author (Cursor in v1.1).

### 8.1 No `Adapter` skeleton you can copy
The spec mentions `pkg/harness/example/adapter.go` (line 41) as a "template adapter for new authors; pure no-op." Good intent. But the spec does not show what the example contains. **Required:** include in the arch spec the actual example code — at least the file structure and what minimal lifecycle it emits. Otherwise the example becomes whatever the first author hacks together, and copy-paste propagates style/error decisions silently.

### 8.2 No documented order of operations inside `Run`
A new adapter author asks: "In what order do I emit events? When do I spawn the process? When do I parse?" The spec describes the **what** but not the **canonical sequence**. Without it, every adapter will have a different shape.

**Fix:** Add to §9 of the PRD (Adapter author guide) a "Canonical Run lifecycle":
```
1. Parse req.Config into typed struct (already done by teamloader; type-assert)
2. Verify binary (already done by teamloader, but defense-in-depth)
3. Build command + env + cwd
4. Emit RunStart (must be first event)
5. Spawn process
6. Start stderr drain goroutine to log file
7. Parse stdout/stderr/jsonrpc; emit canonical events
8. On terminal: emit RunEnd or RunError (must be last event)
9. Cleanup (close pipes, wait/kill process per FR-13)
```

Reference this from the example adapter. Adapter authors who deviate know they're deviating.

### 8.3 No story for streaming `TextDelta` accumulation
The translator emits `MessageAddedEvent` on `TextEnd`. Where do the deltas go? Into an accumulator owned by... whom? The runtime? The adapter? The FSM enforcer?

**Fix:** Specify. Probably runtime owns the accumulator (it knows when to emit `MessageAddedEvent`). State this in §4.1 of the arch spec next to the data flow.

### 8.4 Concurrent EventSink semantics
The spec says (line 540):
> Implementations are responsible for buffering and backpressure; adapters MUST NOT block forever on Emit.

"Forever" is not a contract. Adapters need to know: is `Emit` safe to call from multiple goroutines? Does it block on a full buffer? For how long? What if the consumer is slow?

**Fix:** Specify:
- `Emit` is safe for concurrent use.
- `Emit` may block up to N seconds on backpressure (configurable; default 5s).
- Beyond that, `Emit` returns `ErrSinkFull` (a typed error), and the adapter MUST emit `RunError{code: protocol_error}` and abort.

Without this, adapters will have different behaviors under TUI back-pressure. The conformance suite will not catch it.

### 8.5 Versioning the canonical event set
What happens when we add a 15th event in v1.1? The discriminated union is sealed by `isHarnessEvent()`. New events are additive but consumers using a raw type switch silently drop them. The spec doesn't address this.

**Fix:** Add a "future events" note: "Adapters compiled against canonical events vN will produce events that vN+1 consumers handle. vN+1 consumers MUST handle unknown events via a default case (log + drop)." Or commit to closed evolution: any new event = config version bump. Pick one. State it.

### 8.6 Adapter logging conventions
The PRD §9.3 mentions `harness-<n>.adapter.log` with slog records. The arch spec doesn't specify the logger interface or where adapters get it. Is it passed in `SessionRequest`? Created from `slog.Default()`? Per-adapter? This is the kind of thing every adapter author will solve differently.

**Fix:** Add `Logger *slog.Logger` to `SessionRequest` (or via `Sinks` in the split proposal). Specify that adapters MUST use it for their structured logs, not `log.Printf` or `slog.Default`.

### 8.7 Test helpers for adapter authors
PRD §9.2 promises `replay.PlayFixture(t, "testdata/multi_tool_call.jsonl")`. Arch spec mentions `pkg/harness/replay/` (line 44). But the public surface of `replay` isn't specified. What does `PlayFixture` assert by default? How do I write a fixture? What's the fixture format?

**Fix:** Spec the `replay` package public API in arch §3 — even one paragraph. Without it, each adapter team will invent its own.

### 8.8 What happens when the adapter panics?
The spec says (§3.1):
> Run MUST NOT panic on the caller's goroutine.

OK. But adapters WILL panic (parser bugs, nil pointers). Who catches them? Where does the panic become a `RunError`?

**Fix:** Add a `recover()` wrapper in the runtime's adapter-call site that converts panics to `RunError{code: harness_crashed, cause: <stack>}`. State this in §2.5. Adapter authors can then write parser code without paranoid defensive nil checks — the framework catches bugs and reports cleanly.

---

## What the arch-spec gets right

Solomon and Anders would both nod at:

1. **Sealed discriminated union for events.** The `isHarnessEvent()` pattern is the canonical Go solution. External packages can't pollute the event set. Wire format is clean per type.
2. **Pure `Capabilities()` aspiration** (even if unenforceable). Right idea, wrong shape — see §1.
3. **`ProtocolClass` and `ErrorCode` as typed string enums.** Not `int`s (untraceable), not bare `string`s (typos). Hejlsberg-tier.
4. **Process-per-session is mandatory (FR-12).** No shared state. Composability win. Solomon would call this the Unix way.
5. **Sandbox in shared `pkg/harness/sandbox/`, not per-adapter.** Single source of truth for path traversal/symlink resolution. The arch spec gets this right — adapters cannot accidentally introduce sandbox escapes.
6. **Translator at the runtime boundary (Option B).** Adapters don't import `pkg/session` or `pkg/runtime`. They produce canonical events; the runtime is the only thing that knows the runtime event vocabulary. Right cut.
7. **Replay-driven testing (FR-NEW-13).** Fixtures in `testdata/`, no real binary needed for unit tests. Adapter authors can iterate without API keys. Pit of success for development.
8. **Config version bump to v10 with snapshot-then-mutate.** Boring, correct, matches existing pattern.
9. **TUI reuse via `ToolCallConfirmationEvent` (FR-37).** Harness path looks identical to model path to the TUI. Zero TUI changes. Composition over special-casing.
10. **Per-agent typed `Config` registered at init time (FR-5).** Unknown YAML keys fail at load time, not at session start. Best-class config DX in Go.

The bones are good. The flesh needs work.

---

## Required changes before implementation starts (BLOCKING)

These are the five fixes I'd refuse to start Phase 0 without. Each is hours of spec work, not days.

1. **B1. Resolve the `pkg/harness/translate.go` vs. `pkg/runtime` translator location contradiction (§7.1).** Pick one. Update §2.1 and §4.2 of the arch spec to agree. Without this, the Phase 1 author flips a coin and the Phase 2 author has to refactor.

2. **B2. Remove the `error` return from `Run` (§1.3).** Force terminal state to flow through `RunError` events. Update FR-NEW-10 in the PRD to reflect the new contract. Add a panic-to-`RunError` recovery in the runtime call site (§8.8). This is the single biggest pit-of-success win in the review.

3. **B3. Push ACP-ness into the type system (§5.1).** Either separate `ACPAdapter` interface or `ACP *ACPBindings` sub-struct on the request. Status quo (nil-able `Tools` and `Permission` fields on a flat struct) will cause real bugs and the spec already admits the constraint by comment-marking the fields "ACP only."

4. **B4. Rename and split `PriorTurns` into `ResumeToken` + `SimulatedHistory` (§3.3).** The current naming will cause the Claude Code adapter author to double-feed conversation history. Cost to fix: one rename + one runtime branch. Cost to leave: 4 hours of debugging per adapter team.

5. **B5. Specify the YAML-error format for unknown `harness.config` keys (§6.1).** This is the single most user-visible DX surface for Mark and his team. State exactly what `docker-agent config validate` prints on a typo. Include agent name, key, accepted keys, file:line. Without a spec, every adapter team will format errors differently and Mark will be annoyed.

---

## Suggestions (non-blocking, nice-to-have)

In rough priority order:

- **S1.** Drop `Name()` from `HarnessAdapter`; pass name to `Register` (§1.2). One less duplicate source of truth.
- **S2.** Move `Capabilities()` from instance method to a static value passed to `Register` (§1.1, Option B). Enforces purity at the type level.
- **S3.** Provide a `Visitor` helper in `pkg/harness/visit.go` to get exhaustiveness checking via struct-literal (§2.1). Adopt `go vet -exhaustruct` in CI.
- **S4.** Split `HarnessSessionRequest` into sub-structs by concern (§3.1). 14 fields is too many for one struct.
- **S5.** Drop `Spec *agent.HarnessSpec` from the request; pass `Config any` only (§3.2). Tighter contract; less surface to misuse.
- **S6.** Drop `BuiltInTools []string` from `AdapterCapabilities` (§4.2). Informational without schema is worse than absent.
- **S7.** Rename `Requires` → `HostNeeds`, `Features` → `AdapterOffers` (or `Needs`/`Provides`) (§4.1). Direction-of-fit clarity.
- **S8.** Validate at teamloader: `permission_policy` on non-ACP adapter is a load error (§6.4).
- **S9.** Return typed `PermissionResult` instead of `(PermissionDecision, error)` (§5.2). Timeout becomes in-band, not out-of-band.
- **S10.** Specify the `EventSink.Emit` backpressure contract (§8.4). Bound the "MUST NOT block forever" with a concrete timeout and a typed error.
- **S11.** Spec the `replay` package's public API (§8.7). Otherwise each adapter team invents its own helpers.
- **S12.** Add `Logger *slog.Logger` to `SessionRequest` (§8.6). Adapter logging convention spelled out.
- **S13.** Add to the PRD §9 a "Canonical Run lifecycle" ordered list (§8.2). Reference from the example adapter.
- **S14.** Reconcile the canonical-event-count between PRD ("12") and arch spec ("14") (§2.3). Pick one number, update both.
- **S15.** Fix `DisallowUnknownField` API reference for yaml.v3 (§6.1). It's `KnownFields(true)` on the decoder; spec must be technically correct or the implementer will guess.
- **S16.** State explicit precedence for user `args` vs adapter default args (§6.2). "Appended" is a footgun.
- **S17.** Reject zero-`Timestamp` events at the FSM boundary (§2.2). Avoids a class of "why is this event ordering wrong" bugs.

---

## Bottom line

This is a thoughtful spec — the discriminated union, sealed interface, typed enums, sandbox-in-shared-code, and replay infrastructure are all the right calls. The team has clearly thought about adapter authoring and composability.

The remaining work is hardening the type surfaces against the realistic mistakes a first-time adapter author will make. The Claude Code team and the runtime team are smart and motivated — they will succeed under the current spec. The Cursor adapter author in v1.1, working off the example and the example alone, is who this review protects.

Fix the five blocking items. Take the suggestions you can. Then start Phase 0.
