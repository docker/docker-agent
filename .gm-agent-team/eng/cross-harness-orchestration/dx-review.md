# DX Review: Cross-Harness Orchestration PRD

**Reviewer lens:** Solomon Hykes (simplicity, Unix philosophy, defaults that are
right) + Anders Hejlsberg (type system elegance, progressive disclosure, errors
caught at compile time).

**Scope:** Config schema, adapter interface, canonical event set, error model,
onboarding/testability. Functional and non-functional correctness are out of
scope here; this is a DX-only pass.

---

## Verdict: SUGGESTIONS

The shape is right. The functional spec is rigorous and the JTBDs are concrete.
But there are four DX problems that will compound over time if shipped as-is:

1. **`kind` should be `type`.** The existing codebase uses `type` as the
   discriminator everywhere (`Toolset.Type`, `MCPToolset` hardcodes
   `m.Type = "mcp"`, `RAGStrategyConfig.Type`, `TaskBudget.Type`). Introducing
   `kind` for harnesses is gratuitous inconsistency. New users will mistype it.
2. **`harness_config` should be `config`.** The PRD already nested `harness_config`
   inside `harness:`. The redundant prefix screams "I am being defensive about
   namespace collision in a place where there is none."
3. **The `SubSessionRequest` and `Event` structs are God-objects.** Adapter
   authors will be confused about which fields apply to which events, and the
   compiler will not catch wrong combinations. This is the single biggest
   future-friction trap.
4. **Capability-driven defaults aren't enforced in types.** `StreamsTextDeltas:
   false` is documented in prose (FR-16) but a Codex adapter author can still
   emit a `TextMessageDelta` and the system will accept it. Lift to type or
   runtime invariant.

Fixable in a focused pass. None require re-architecting.

---

## 1. Config schema DX

### 1.1 `kind` vs `type` — change to `type`

**Existing precedent in `pkg/config/latest/types.go`:**
- `Toolset.Type` (line 798) — the discriminator across every toolset variant
  (mcp, rag, shell, filesystem, fetch, lsp, todo, memory, …).
- `MCPToolset.UnmarshalYAML` (line 51): `m.Type = "mcp"` — even hidden
  discriminators use the name `Type`.
- `RAGStrategyConfig.Type` (line 1292).
- `TaskBudget.Type` (line 1164).
- `ThinkingBudget` uses string/int polymorphism, not a kind/type field, so it
  doesn't apply.

There is no countervailing precedent for `kind` anywhere in the config types.
Kubernetes uses `kind` but docker-agent is not Kubernetes. **Stay consistent
with your own codebase.** This is exactly the "make the right thing easy"
principle: a user who already wrote `type: mcp` in a `toolsets:` block this
morning should not need to switch to `kind: claude-code` this afternoon.

**Change:** `harness.kind` → `harness.type` throughout the PRD.

### 1.2 `harness_config` → `config` (or `options`)

The PRD's own example reads:

```yaml
harness:
  kind: claude-code
  harness_config:
    max_turns: 20
```

The word "harness" appears three times in five lines. Two of those are
namespace prefixes the YAML already provides via nesting. The existing
`Toolset` struct uses bare `Config any` (line 815) for the equivalent escape
hatch. Follow that.

**Change:** `harness.harness_config` → `harness.config`.

`options` is also defensible (more conventional in CLI-flag-style configs) but
`config` matches the existing `Toolset.Config` field and is therefore the
lower-friction choice.

### 1.3 YAML examples — clarity assessment

I read each of the five examples cold, pretending I haven't read the spec.
Verdict per example:

| Example | Clear? | Issue |
|---|---|---|
| Claude Code | ✅ | None. The `max_turns` and `system_append` knobs are obvious from name. |
| Codex | ⚠️ | `reasoning_effort: high` — is this an enum? What are the values? Comment in the example, or link, would help. |
| OpenCode | ❌ | The inline comment "OpenCode CLI has no per-call system prompt; warn surfaced at load" is documentation leaking into examples. A first-time reader has no idea what `task_prefix` does vs. what a `system_append` would have done. |
| Copilot | ⚠️ | `acp_handshake_timeout: 5s` is the only knob shown. Users will wonder "is this all I can set? Where are the others?" |
| OpenClaw | ✅ | The risk-acknowledgment pattern is well-illustrated. |

**Change:** Add a sixth, minimal example at the top:

```yaml
# Simplest possible harness agent: just a kind, no overrides.
agents:
  - name: reviewer
    harness:
      type: claude-code
```

This sets the right expectation: **the simple case is one line**. Then build
up. Progressive disclosure. Hejlsberg would approve.

Also: remove the inline editorializing comment from the OpenCode example.
That's reference-doc material, not example material.

### 1.4 What's missing from the YAML — first "how do I…" questions

A developer will ask, in roughly this order:

1. **"How do I see what `harness.config` keys are valid for kind X?"**
   FR-5 says adapters reject unknown keys. Good. But where does a user learn
   the valid keys? `docker-agent config validate` should print them.
   `docker-agent harness describe claude-code` should dump the
   `AdapterCapabilities` and accepted `harness_config` schema as YAML.
   The PRD does not commit to either CLI surface. **Add this.**

2. **"How do I make Claude Code use my Anthropic key from $ANTHROPIC_API_KEY?"**
   The example does not show `env:`. FR-32 says PATH, HOME, USER, LANG, LC_*,
   TERM are auto-passed plus explicit `harness.env` entries. Where does
   `$ANTHROPIC_API_KEY` come from? `Requires.EnvVars` lists it (line 371) but
   doesn't say "we forward this automatically." This will be question #1.
   **Decide and document:** does docker-agent auto-forward env vars listed in
   `Requires.EnvVars`, or must the user explicitly add them to `harness.env`?
   I'd vote auto-forward, with an opt-out flag.

3. **"Can I share a session token across two agents?"** §7.4 says no for
   v1 ("ACP shared session" is in out-of-scope). Surface this in §6 too.

4. **"What about `~/.config/docker-agent/harness.yaml` defaults?"** Users will
   want to set `permission_policy` defaults once per machine rather than
   per-agent. Not a v1 blocker, but the PRD doesn't mention it.

5. **"How do I pin a binary version?"** FR-4 validates presence at team-load
   time. `Capabilities().Requires` includes "min version" but the YAML schema
   in §6.1 has no `version:` field on `harness:`. Either expose it
   (`harness.min_version: 0.5.0`) or document why pinning is implicit via
   adapter compile-time defaults.

---

## 2. Adapter interface DX

The three-method interface is **good**. `Name()` + `Capabilities()` + `Run()`
is the minimal viable shape and matches how Go interfaces are best designed
(small, narrow, behavioral). Hykes would nod.

But the **request and event struct shapes are God-objects** and will cause
adapter-author confusion.

### 2.1 `SubSessionRequest` is too wide

```go
type SubSessionRequest struct {
    Task          string
    SystemPrompt  string
    SessionToken  string
    WorkingDir    string
    Env           map[string]string
    Events        chan<- Event
    ToolExecutor  ToolExecutor     // ACP only
    Permission    PermissionGate   // ACP only
    HarnessConfig map[string]any
}
```

Problems:

1. **`ToolExecutor` and `Permission` are ACP-only.** A Claude Code adapter
   author will see these in the struct and wonder if they should plumb them
   through. The type system says "yes, you have these"; the docs say "no, they
   are nil." That's the worst combination: the compiler doesn't help.
2. **No phase distinction** between "first turn" (no `SessionToken`) and
   "resume" (token present). The adapter has to branch on string emptiness.
3. **`map[string]any` for `HarnessConfig`** discards every type guarantee.
   Adapter authors will write defensive runtime parsing.

**Recommendations (in order of impact, low risk first):**

- **Split capability-dependent fields onto a second struct, passed via the
  request context or a builder pattern.** Or, more idiomatic Go: make ACP
  features available via type assertion on a `SubSessionRequest` extension
  interface:

  ```go
  // Always available
  type SubSessionRequest struct {
      Task         string
      SystemPrompt string
      SessionToken string  // empty on first turn
      WorkingDir   string
      Env          map[string]string
      Events       EventSink
      Config       HarnessConfig  // typed accessor, not map[string]any
  }

  // Adapters that need it can ask:
  if acp, ok := req.(ACPRequest); ok {
      acp.ToolExecutor().Exec(...)
      acp.Permission().Request(...)
  }
  ```

  This is the Go-idiomatic "capability via interface assertion" pattern (think
  `io.ReaderAt` extending `io.Reader`). It also matches how the existing
  codebase composes optional behaviors.

- **Type `HarnessConfig` per adapter.** Each adapter package exports a
  `Config` struct with typed fields. The runtime unmarshals the YAML's
  `harness.config:` map into the adapter's type at team-load time, surfacing
  unknown keys *at validation*, not on first run. Today's PRD says adapters
  reject unknown keys (FR-5) — but at what stage? If it's at `Run()` time,
  the user discovers typos at runtime, not at `docker-agent config validate`
  time. **Move this to load time** and you eliminate a whole class of "my
  agent crashed in production because of a typo" bugs.

  Mechanically: each adapter registers a `func() any` that returns a fresh
  config struct; the loader unmarshals into it with `yaml.DisallowUnknownField`
  (you already use this on line 166 of `types.go` — use it again).

- **`EventSink` over `chan<- Event`.** A channel is great for orchestrators
  but constrains adapters: you can't synchronously `Emit()` from a deeply
  nested callback without worrying about channel-full deadlock. A tiny
  interface:

  ```go
  type EventSink interface {
      Emit(Event) // never blocks > N ms; drops with telemetry if full
  }
  ```

  …lets the runtime decide buffering policy in one place. Adapters get a
  fire-and-forget API. Tests can supply a slice-backed sink. This is the
  same reason Go's `slog` uses `Handler` not `chan slog.Record`.

### 2.2 `Capabilities()` shape — clear, with one fix

The struct is the right idea. But `Protocol ProtocolClass` typed as
`"stream" | "acp"` is a string enum and Go has no string enums. **Make it a
named type with constants.** Otherwise adapter authors will write
`Protocol: "ACP"` (uppercase) and the runtime won't match.

```go
type ProtocolClass string

const (
    ProtocolStream ProtocolClass = "stream"
    ProtocolACP    ProtocolClass = "acp"
)
```

Same for `permission_policy` enums and error codes. Compile-time enforcement
is one `const` block away. Hejlsberg would put this on a billboard.

**Also:** `AdapterFeatures` is a flag bag. That's fine for v1 (≤5 flags), but
once you hit 10, this becomes a "what does each flag mean and which
combinations are valid?" puzzle. Document combinatorial invariants now:

- "If `SupportsMultiTurn: false`, the runtime MUST NOT pass `SessionToken`."
- "If `StreamsTextDeltas: false`, the adapter MUST NOT emit `TextMessageDelta`."

Today these are buried in prose. Lift them to a `Capabilities.Validate()`
method that the runtime calls at registry-time. Catches errors at startup,
not at first invocation.

### 2.3 Most likely adapter-author mistake

In order of likelihood:

1. **Emitting events out of order or unbalanced.** Forgetting `TextMessageEnd`,
   emitting `RunFinished` twice on an error path, emitting `RunStarted` *after*
   the first `TextMessageStart`. Mitigation: a `runtime/harness.SafeEventSink`
   wrapper that enforces the FSM (RunStarted → … → RunFinished | RunError) and
   panics in dev / errors-to-log in prod on violation. **You called this out
   in FR-13 / FR-14 with tests but did not propose a runtime enforcer.** A
   panicking wrapper used in tests + a counting wrapper used in prod is ~50
   LOC and saves every future adapter author from this bug.

2. **Forgetting to forward stderr or closing stdin too early.** Process
   plumbing footguns are a Go classic. Provide a `harness.ChildProcess` helper
   that wraps `exec.Cmd` with the right defaults (stderr → session log, stdin
   pipe, signal handling per FR-10). Adapters then write `proc, err :=
   harness.Spawn(ctx, "claude", args)` instead of reimplementing process
   lifecycle five times.

3. **Sandbox enforcement leaks.** FR-31 says sandbox enforcement is in the
   adapter, not the harness. Two adapter authors will write two
   path-canonicalization helpers. One will be wrong on symlinks. **Move
   sandbox enforcement to a shared `harness/sandbox` package and require
   adapters to call it.** This is a security boundary (NFR-7), not a courtesy.

4. **Mapping `harness_config.foo` differently between adapters.** If Codex
   accepts `model: gpt-5-codex` and Claude Code accepts `model_name: …`, the
   inconsistency will bite users. **Reserve common key names** (`model`,
   `system_append`, `max_turns`, `temperature`) and document which adapters
   may use which. Adapter-specific keys go under a per-adapter namespace if
   you want to be paranoid (e.g. `harness.config.codex.reasoning_effort`),
   though I'd avoid that until you have a real collision.

---

## 3. Canonical event set DX

The 12-event vocabulary is well-chosen. Start/Delta/End for streaming text is
the right call. AG-UI as inspiration without wire-format commitment (non-goal
#7) is exactly right — borrow the vocabulary, skip the schema lock-in.

### 3.1 Start/Delta/End in a Go channel system — yes, but…

The pattern is correct. The risk is **fan-in interleaving** when multiple
subagents run in parallel (NFR-10) and their events land on a shared bus.
A consumer reading `TextMessageDelta` needs to know which message it belongs
to. The PRD addresses this via `MessageID` in the event struct (Appendix A,
line 613) — good. But you have to make this **non-optional in the type
system**:

```go
type TextDelta struct {
    MessageID MessageID  // required
    Text      string
}
```

Today everything is on one fat `Event` struct (lines 610–622) with optional
fields. **A discriminated-union pattern fits this better in Go:**

```go
type Event interface { isEvent() }

type RunStarted struct { SessionID string; Model string; ... }
type TextMessageDelta struct { MessageID MessageID; Text string }
type ToolCallStarted struct { CallID CallID; Name string; ... }
// ...

func (RunStarted) isEvent()       {}
func (TextMessageDelta) isEvent() {}
// ...
```

Consumers `switch ev := ev.(type) { case TextMessageDelta: … }`. The compiler
catches "I forgot to handle this event kind in my switch" when used with
exhaustive-switch linters (gocheckcompilerdirectives, go-exhaustruct,
musttag), and adapter authors can't accidentally set `ToolArgs` on a
`RunStarted` event because the field doesn't exist on that type. **This is
the single biggest type-safety win available.** Hejlsberg-level pit-of-success.

Cost: ~30 minutes refactor; the events still flow on a single channel of
`Event` interface. Benefit: every event becomes self-documenting and
mis-emission becomes a compile error.

### 3.2 ToolCallStarted + ToolCallFinished as two-events-one-call

The naming is fine. The risk is exactly what you flagged: someone emits
`ToolCallStarted` twice for the same call ID.

**Fix at the type level:** require a `ToolCallID` newtype and have the
runtime's event sink track open call IDs:

```go
type ToolCallID string

// Runtime sink rejects:
//   - ToolCallStarted with an already-open ID
//   - ToolCallFinished without a matching Started
//   - any Finished after RunFinished
```

This is the same FSM wrapper from §2.3. It costs nothing at runtime and turns
"someone made a logic error in the Codex adapter" from a silent
canonical-stream corruption into a logged, attributable panic in dev. Ship it.

**Naming nit:** `ToolCallStarted` reads slightly weird next to "ToolCall
Finished" because "Started" is past tense. Consider `ToolCallStart` /
`ToolCallEnd` to match `TextMessageStart` / `TextMessageEnd` / `ReasoningStart`
/ `ReasoningEnd`. Right now your 12 events use **three different tense
patterns** (Started/Finished, Start/End, Pending/Resolved). Pick two and
stick with them.

Suggested:
- Lifecycle: `RunStart`, `RunEnd`, `RunError`. (Currently `RunStarted`,
  `RunFinished`.)
- Streaming: `*Start`, `*Delta`, `*End`. (Already consistent.)
- Async request/reply: `Permission{Request, Response}`. (Currently
  `Pending/Resolved` — fine, but the asymmetry stands out.)

This is bikeshed-y, but the system is small enough that consistency is
free here. After 50 events it won't be.

### 3.3 `HarnessRaw` as opt-in via separate `RawEventSink` interface

The PRD currently has `HarnessRaw` as a member of the canonical event set,
enabled per-adapter via `harness_config.emit_raw: true` (FR-15). The question
in the prompt asks about a separate `RawEventSink` interface.

**Recommendation: do the separate sink interface.** Reasons:

1. **Type pollution.** Today every consumer of `Event` has to handle a
   `HarnessRaw` case it doesn't care about. If 99% of users never enable
   raw events, that's 99% of consumers writing dead `default:` branches.
2. **Performance.** Raw events can be huge (full ACP frames). Funneling
   them through the same channel as canonical events makes the canonical
   stream pay for the raw stream's worst case.
3. **Discoverability.** A separate `RawEventSink` is opt-in at the **type
   system level**: you wire it up only if you want it. That's strictly
   better than a runtime config flag for an escape hatch.

Concrete shape:

```go
type RawEventSink interface {
    EmitRaw(adapter string, frame []byte)
}

// SubSessionRequest gains an optional field:
type SubSessionRequest struct {
    // ...
    RawSink RawEventSink // nil = adapter does not emit raw frames
}
```

Adapter authors check `if req.RawSink != nil { req.RawSink.EmitRaw(...) }`.
Most adapters can skip the check entirely if they don't have raw frames to
emit. **Remove `HarnessRaw` from the canonical 12-event set.**

This gets you down to 11 canonical events, which is also nicely
"one fewer thing to learn."

### 3.4 What's missing from the event set — first "how do I…" questions

1. **"How do I surface streaming token counts / cost?"** Out-of-scope #9
   (streaming usage) defers this. Fine. But interim users will want at least
   a final-usage report. FR-21 puts `usage` on `RunFinished`. Good. Make
   sure the type is structured (`Usage` struct), not `map[string]any`. The
   PRD's Appendix A shows `Usage *UsageDetail` — keep it typed; resist the
   urge to make it raw JSON "because each harness reports differently." A
   common minimum (input_tokens, output_tokens, cost_usd) covers 90% of use
   cases. Per-harness extras live in a typed `Vendor` sub-struct or in
   `HarnessRaw`.

2. **"How do I surface 'agent is thinking, no output yet'?"** No
   `KeepAlive` / `Heartbeat` event. After 30 seconds with no output, the TUI
   can't distinguish "still working" from "hung." For long-running harness
   sessions (JTBD 4: 90 seconds on a 30-file refactor), this matters. **Add
   a `Heartbeat` event** that adapters emit on a timer when they have nothing
   else to say. Or: have the runtime emit it on the adapter's behalf if no
   events have flowed in N seconds.

3. **"How do I surface 'harness is doing X'?"** Status messages distinct from
   reasoning. Claude Code emits things like "Reading 12 files…" that aren't
   reasoning text, aren't tool calls, aren't text messages. Today these would
   fall into `HarnessRaw` or get squashed into `TextMessageDelta` with no
   distinction. Consider a `StatusUpdate` event with a free-form `text` field.
   (Or accept that this lives in `HarnessRaw` and document the trade-off.)

4. **"How do I surface a sub-agent / sub-tool plan?"** §7.4 maps ACP `plan`
   to `HarnessRaw`. That punts. Plans are first-class in Claude Code and
   ACP. The orchestrator might want to render them. **Either** add a
   `Plan` event with a structured representation **or** explicitly accept
   that plans are out-of-scope-for-canonical-rendering in v1. Pick one and
   write it down.

5. **"How do I attribute a tool call to a sub-agent in a parallel fan-out?"**
   When two subagents run in parallel (NFR-10), the orchestrator sees two
   event streams. If those streams are multiplexed onto one channel for the
   TUI, every event needs a `SubAgentID`. Today only `MessageID` exists.
   **Add `SessionID` or `SubAgentID` to every event** so the TUI can group
   them correctly.

---

## 4. Error handling DX

The error model is mostly right. The mapping ambiguity question is the real
one.

### 4.1 Are the error codes right?

The PRD §4.5 lists: `binary_not_found`, `binary_version_mismatch`,
`auth_failed`, `network_error`, `timeout`, `context_exhausted`,
`permission_denied`, `harness_crashed`, `protocol_error`, `cancelled`,
`unknown`.

Your prompt listed: `context_exhausted`, `rate_limited`, `auth_failed`,
`harness_crashed`, `harness_timeout`, `user_canceled`, `capability_mismatch`,
`unknown`.

These two lists don't match. Reconcile. Specifically:

- **`rate_limited` is missing from the PRD list.** Every model-backed
  harness will hit this. Today it gets mapped to `network_error` (vague) or
  `unknown` (useless). **Add `rate_limited`** with `retryable: true` and a
  `retry_after` hint field (or a `retry_after_seconds int` in the error
  detail).
- **`capability_mismatch` is missing.** When an orchestrator asks an adapter
  to do something its capabilities say it can't (e.g., a system prompt to an
  adapter whose `SupportsPerCallSystemPrompt=false`), what happens? Today the
  PRD says adapters reject unknown `harness_config` keys (FR-5), but the
  cross-capability orchestrator scenario is undocumented. **Add
  `capability_mismatch`** for "the request is well-formed but exceeds my
  declared capabilities."
- **`user_canceled` vs `cancelled`.** Pick one; document that "user_canceled"
  means TUI cancel and "cancelled" means context cancel by parent code, or
  unify them. Two codes for nearly-the-same condition will get muddled.
- **`binary_not_found` and `binary_version_mismatch`** are good — most
  systems collapse these into "couldn't start" and lose the actionable hint.

**Recommended additions:**
- `rate_limited` (with `retry_after_seconds`)
- `capability_mismatch`
- Possibly `quota_exceeded` (distinct from rate limit — non-retryable until
  next billing cycle)

**Recommended removal:**
- Collapse `cancelled` and `user_canceled` into one (`cancelled`) with a
  `cause` string field for distinguishing source. Two codes here is a
  distinction-without-a-difference for the orchestrator's retry logic.

### 4.2 Is the mapping clear enough for consistency?

**No.** This is the most underspecified part of the PRD.

Today the PRD says "adapters map harness-specific signals to these codes" but
doesn't provide a mapping table. Two adapter authors looking at "Claude Code
returned HTTP 429" and "Codex stdout closed with no `result` line" will make
different choices:

- HTTP 429 from Claude Code → `rate_limited` (right) or `network_error`
  (wrong but plausible).
- Codex stdout EOF → `harness_crashed` (right) or `protocol_error` (also
  plausible).
- Auth missing → `auth_failed` (right) or `binary_not_found` (when the
  binary itself works but rejects the call).

**Fix:** Add a "canonical mapping" table to the PRD with rows like:

| Adapter signal | Canonical code | Retryable | Notes |
|---|---|---|---|
| HTTP 429 from upstream | `rate_limited` | yes | extract `Retry-After` if present |
| HTTP 401/403 from upstream | `auth_failed` | no | |
| Process exit before `RunFinished` | `harness_crashed` | yes | include stderr tail |
| Malformed JSON line | `protocol_error` | no | include offending bytes |
| Context cancellation | `cancelled` | no | |
| Wall-clock timeout | `timeout` | yes | |
| Adapter's own bug | `unknown` | no | |

Adapter authors then have a reference, not folklore. The PRD already has
adapter-specific spec sections (§7.1–7.5) — add an "error mapping" subsection
to each one with no more than 6 rows. Two engineers writing two adapters will
then make the same choice for the same signal. **This is a one-page diff that
saves a year of inconsistency bugs.**

### 4.3 Errors as events vs. errors as return values

FR-8 says `Run` returns nil on clean shutdown and non-nil only for
adapter-internal bugs. Errors flow as `RunError` events.

**This is correct** (Go: one error channel per session, not two). But it
implies adapter authors need a clear rule: **never return a non-nil error
from `Run` unless the event sink is unreachable.** Spell that out. Today
it's implied but not enforced — make the rule explicit and add a test that
fails any adapter that returns a non-nil error when the sink received a
`RunError`. Type-system alternative: change the signature to `Run(...)`
returning nothing, and emit panics-as-errors to a separate runtime channel.
I'd keep the current shape but add the test.

---

## 5. Missing DX concerns

### 5.1 Adapter testability — the biggest gap

The PRD's Appendix B test plan mentions "real binary per adapter, in CI
behind a build tag." That's necessary but **not sufficient for adapter
authors**. A developer writing a new adapter (say, Gemini CLI in 6 months)
needs to test their adapter without the actual harness binary on their
machine.

**Missing:**

1. **A `harness/fake` package.** An in-process fake adapter that emits a
   scripted sequence of canonical events. Used by orchestrator tests, by
   TUI tests, and by anyone iterating on event-handling code who doesn't
   want to install Claude Code.

2. **A `harness/replay` mechanism.** Record harness stdout/stderr/ACP frames
   to a fixture file once; replay through the adapter's parser in unit
   tests. The PRD §10 lists "Recording/replay of harness sessions for tests"
   as out-of-scope for v1 (v1.1, orthogonal infra). **I'd pull this into
   v1.** It's the single most impactful thing for the "≤500 LOC, ≤2 weeks
   for a new adapter" success metric. Without recorded fixtures, every new
   adapter author needs the binary on their dev box and on every CI runner.

3. **A "lint my events" tool.** Given a recorded event stream, validate the
   FSM (RunStart present, exactly one terminal, balanced Start/End pairs).
   This is the runtime-enforcer wrapper from §3, repurposed as a CLI:
   `docker-agent harness lint events.jsonl`. Adapter authors run it during
   development before writing the integration test.

4. **Conformance suite.** A set of canonical scenarios (single message, multi
   tool call, error mid-stream, cancellation, multi-turn resume) that any
   adapter MUST pass. Today each adapter has bespoke tests. With a
   conformance suite, you can run all 5 v1 adapters against the same 20
   scenarios and assert orchestrator-side behavior is identical. This is
   how FR-17 ("orchestrator MUST consume the event stream without knowing
   which harness produced it") becomes verifiable, not aspirational.

### 5.2 Debugging — how do I figure out what my adapter did wrong?

The PRD has good observability primitives (stderr to log file, `HarnessRaw`
opt-in, structured `RunError`). It's missing the **debugging UX**:

1. **A `docker-agent harness trace <agent>` command** that streams every
   canonical event for an active session to stdout in human-readable form.
   Like `docker run --attach` for harness adapters. Lets developers see
   "what events am I emitting?" without parsing the session log.

2. **Adapter-side structured logging.** Each adapter SHOULD emit slog records
   to a per-session log file (separate from the harness's stderr) so when
   things go wrong you have both the harness's view and the adapter's view.
   The PRD calls for stderr forwarding but not adapter logging.

3. **Event provenance.** When an `Event` is malformed and the FSM enforcer
   logs a violation, the log should include: adapter name, session ID,
   sequence number, and a few preceding events for context. Today
   Appendix A's `Event` struct has `Timestamp` but no sequence number and
   no adapter attribution.

### 5.3 Onboarding for a new adapter author

Six months from now, a developer (internal or community) wants to add a
Gemini CLI adapter. What's their experience?

The PRD doesn't address this. **Missing:**

1. **A `CONTRIBUTING_HARNESSES.md` or equivalent.** "How to write a new
   adapter" guide. 80% of the content writes itself once the package
   structure exists. Without it, the first community PR will be a slog of
   review comments asking the author to match patterns that aren't
   documented.

2. **A template adapter.** `pkg/harness/example/` with a working, fully
   commented stub that emits a few canonical events using a fake binary
   (`echo`). New adapter authors copy this, swap the parsing logic, ship.
   This is how Cobra, gRPC, and most extensible Go projects bootstrap
   contributions.

3. **An adapter checklist.** A markdown table of things every adapter
   must do (implement Interface, register in registry, declare
   Capabilities, map all canonical events, map all canonical error codes,
   sandbox path checks if filesystem, conformance suite green, fixture
   recorded). The PRD's FR-numbered requirements are the seed of this list
   — just convert them into a checklist with checkboxes.

4. **A "minimal viable adapter" benchmark.** The success metric in §8 says
   "≤500 LOC and ≤2 weeks for one engineer." How is that measured? **Add a
   reference: "the Codex adapter is N LOC at v1 ship; that's the baseline."**
   Without a reference, the metric is unfalsifiable.

### 5.4 Two more small DX items worth fixing now

- **`HasHarness()` naming.** The branch point method is `agent.HasHarness()`
  (FR-3). The negation is `!agent.HasHarness()` which reads ambiguously
  ("does it have a harness configured?" vs "is it harness-backed?"). Today
  agents have `Model` not `HasModel()`. Prefer `agent.IsHarnessBacked()` or
  `agent.Backing()` returning an enum (`Model | Harness`). The latter
  scales when v2 adds more backings; the former is the lowest-friction
  rename.

- **`SubSessionRequest` vs. terminology in the rest of the spec.** Some
  places call it "subsession," others "subagent session," others "harness
  session." Pick one. I'd suggest **"harness session"** consistently
  (matches `runHarnessSession` in FR insertion point) and rename the
  struct to `HarnessSessionRequest`. The `Sub` prefix is doing nothing
  the nesting in the team config doesn't already convey.

---

## What the PRD gets right

A specific list, because credit where due:

1. **Goals/non-goals separation is excellent.** The non-goals are surgical:
   "no harness-as-orchestrator," "no custom tool injection," "no AG-UI wire
   format." Each prevents a class of scope creep. Hykes-grade discipline.

2. **The "borrow AG-UI vocabulary, not the wire format" decision** is
   exactly right. The semantic model is the valuable part; locking yourself
   to someone else's JSON schema for an internal event bus is anti-pattern.

3. **JTBDs are concrete.** JTBD 3 ("compare two harnesses on the same task")
   in particular is the kind of user-driven requirement that drives
   capability surfacing and parallelism — and you actually traced it
   through to NFR-10 (parallel concurrency) and Capabilities introspection.

4. **The 12-event canonical set is well-scoped.** No `MetadataChanged`,
   `ToolCallProgress`, `MemoryUpdated`, or other speculative events. Just
   the minimum needed for the TUI to render and the orchestrator to route.
   Adding events later is easy; removing them is impossible.

5. **`Capabilities()` as a pure function (FR-7) with declared `Requires`,
   `Features`, `BuiltInTools`** is the right shape. Lets the runtime
   pre-validate before spawning. The orchestrator can dispatch on
   capabilities, not on adapter name. Hejlsberg-style "the type system
   tells you what's possible before you call it."

6. **Process-per-session isolation (FR-9, NFR-11)** is the right default.
   Pooling is the optimization, not the baseline. Future you will thank
   present you for not sharing state by default.

7. **Sandbox-in-adapter, not in-harness (FR-31)**. Correct trust boundary.
   Hostile harness is the threat model and the PRD names it.

8. **The "AG-UI wire format compatibility is a non-goal until a real
   consumer exists"** clause (out-of-scope table) is a beautiful piece of
   YAGNI discipline.

9. **Mutually exclusive `model:` and `harness:` (FR-1)** with validation
   rejecting both-or-neither is the right call. Saves a year of "which one
   wins?" support tickets.

10. **Open questions section is well-formed.** Each OQ has a proposed
    answer with a rationale. Reviewers can disagree with the proposal
    rather than having to invent one. This is how you keep architecture
    review from becoming a Socratic seminar.

---

## Summary of asks (ordered by impact / cost)

| # | Change | Cost | Impact |
|---|---|---|---|
| 1 | `kind` → `type` (consistency with existing config) | trivial | high (every user hits this) |
| 2 | `harness_config` → `config` | trivial | medium |
| 3 | Add error-code mapping table to each adapter spec | 1 day | high (cross-adapter consistency) |
| 4 | Move `harness_config` validation to load time with typed adapter configs | 1–2 days | high (catches typos at validate) |
| 5 | Discriminated-union `Event` type instead of fat struct | 1 day | high (Hejlsberg pit-of-success) |
| 6 | Move `HarnessRaw` from event set to separate `RawEventSink` interface | 1 day | medium |
| 7 | Add runtime FSM-enforcer for event ordering | 1 day | high (catches adapter bugs early) |
| 8 | Add `Heartbeat`, fix `rate_limited` / `capability_mismatch` gaps | 0.5 day | medium |
| 9 | Pull replay/record into v1 (move from §10 to in-scope) | 3 days | high (adapter testability) |
| 10 | Add CONTRIBUTING_HARNESSES.md + template adapter package | 2 days | high (community contribution) |
| 11 | Add `docker-agent harness describe` and `harness trace` CLI commands | 2 days | medium (debugging) |
| 12 | Unify naming (Start/End vs Started/Finished) | 0.5 day | low but free |
| 13 | Rename `SubSessionRequest` → `HarnessSessionRequest`; clarify "session" terminology | 0.5 day | low |

If only #1, #2, #3, #5, and #9 ship before v1, the rest can wait. Those five
prevent the worst failure modes and the system stays internally consistent.

The PRD is otherwise solid and ready for arch review. **Approve with the
suggestions above merged.**
