# Architecture Review: Cross-Harness Orchestration PRD

**Reviewer:** GM arch review (Mark's COO agent team)
**Date:** 2026-05-13
**PRD reviewed:** `prd.md` (Draft for arch + DX review)
**Codebase HEAD:** docker-agent main, May 2026

## Verdict

**REVISE.** The PRD is directionally right and most of the technical claims
hold up against the code. But the insertion point is named wrong (the work
is not in `agent_delegation.go`), three of the open questions resolve
against the proposed answer once you read the code, and the PRD is silent
on several things engineering will need on day one (hooks, persistence,
session-store schema migration, event scoping, tool dispatcher
registration, TUI rendering contract). Specifics in §1, §6, §7 below.

This is a healthy "revise once and ship," not a structural rework. The
ACP head start is real, the canonical event model is the right call, and
the v1 scope is sensible. Net opinion: land the revisions, then approve.

---

## 1. Insertion point assessment

**Claim in PRD:** "Insertion point: `pkg/runtime/agent_delegation.go`, new
`runHarnessSession` path branching on `agent.HasHarness()`."

**Reality:**

`pkg/runtime/agent_delegation.go` is the right *neighborhood* but the wrong
*function*. The branch point is not the file, it is `runForwarding` and
`runCollecting` — the two functions in that file that today take a child
agent built around a model + the model-loop runtime and stream its events
back to the parent.

What needs to change, concretely:

1. **`pkg/runtime/agent_delegation.go:248` `(*LocalRuntime).runForwarding`**
   — split into two branches inside the function:
   - If `child.HasHarness()` → call a new `runHarnessForwarding(ctx, parent, evts, child, req)` that builds a `SubSessionRequest`, drives the adapter, and emits canonical-mapped runtime events.
   - Else → the existing model-loop path (build `newSubSession`, call `r.RunStream(ctx, s)`, forward events).
   Either branch must still emit the same parent-visible events: `AgentSwitching` (entry), `SubSessionCompleted` (exit), and a `tools.ToolCallResult` return for the orchestrator's tool-call slot. The PRD is silent on this contract; engineering will need to satisfy it for FR-25 ("orchestrator MUST receive every `RunError` as a tool-call failure") to work without re-plumbing.

2. **`pkg/runtime/agent_delegation.go:310` `(*LocalRuntime).runCollecting`**
   — same split. Background agents (`RunAgent`) go through this path. The
   PRD does not say whether harness-backed agents are allowed as
   background agents; default should be **yes**, because JTBD 3 (parallel
   benchmark) and JTBD 4 (long-running 90s harness) both want async
   dispatch.

3. **`pkg/runtime/loop.go:35` `registerDefaultTools`** — no change for
   `transfer_task` (it dispatches by agent name; the branch is downstream
   in `runForwarding`). But if harnesses ever become callable via a
   distinct tool (e.g. a future `dispatch_to_harness`), this is where it
   plugs in. v1 should explicitly piggyback on `transfer_task` to avoid a
   new top-level tool.

4. **`pkg/agent/agent.go:20` `Agent` struct** — add `harness *HarnessSpec`
   field (kept opaque from the runtime; consumed only by the adapter
   layer) and `HasHarness() bool` method. The struct already mirrors the
   PRD pattern of "field for the backing kind + accessor"
   (`models`, `Model()`, `HasModelOverride()`). The PRD names
   `HasHarness()` as the branch primitive — that's correct, but it is a
   one-line method on `*Agent` and the PRD should say so.

5. **`pkg/agent/opts.go`** — add `WithHarness(spec *HarnessSpec) Opt`.
   Mirrors `WithModel`. The PRD doesn't enumerate this; engineering will
   discover it on first pass but the design doc should call it out.

6. **`pkg/teamloader/teamloader.go`** — build the `*Agent` with a harness
   when config carries one and skip the model resolution path. The PRD
   says "validation MUST verify the harness binary is on PATH ... at
   team-load time" (FR-4), which means the binary check happens here, not
   in `pkg/config/latest/validate.go` (which is pure config schema, no
   filesystem I/O — see e.g. how `Toolset.validate()` deliberately stays
   PATH-free at lines 73-242). FR-4 needs to be split: schema validation
   in `validate.go`, PATH check in teamloader.

7. **New package: `pkg/harness/`** (per OQ-6 — agreed) with subpackages
   per adapter. The HarnessAdapter interface, the canonical Event type,
   the SubSessionRequest type, and the registry live here. The runtime
   then imports this package and consumes adapters through the
   interface; the runtime does **not** depend on individual adapter
   subpackages (no `_ "github.com/.../claude_code"` blank imports — that's
   an anti-pattern when adapters need credentials or PATH lookups at init
   time).

8. **`pkg/session/session.go`** — needs new fields for the harness session
   token (§5 below).

9. **`pkg/config/latest/types.go` + `pkg/config/latest/validate.go`** —
   schema additions (§2 below).

10. **`pkg/config/v8` and earlier** — version migration shims so older
    YAML configs without `harness:` still load. The PRD does not mention
    this; it is required by the existing versioned config pipeline (each
    `pkg/config/vN/types.go` defines a snapshot and the upgrade path is
    explicit — see e.g. v8 -> latest already in the tree).

Bottom line on insertion point: the PRD's file pointer is approximately
right, but the actual edit list is ~10 files across 4 packages, not "one
new function." Engineering needs the breakdown above before they start.

---

## 2. Config schema assessment

**The proposed `harness:` key fits the existing pattern, with one snag.**

What works:

- `AgentConfig` (`pkg/config/latest/types.go:372-407`) already mirrors
  this shape: a sibling key (`Model string` at line 374) selects the
  backing type, structured sub-config keys (`Fallback *FallbackConfig`,
  `Hooks *HooksConfig`, `Cache *CacheConfig`) hang off pointers. Adding
  `Harness *HarnessConfig` at the same level is idiomatic.
- The mutual-exclusion rule in FR-1 (`model` and `harness` cannot both
  appear) belongs in `validate.go` `Validate()` (line 21) — the existing
  `validateFallback()` (line 57) is the template.
- `harness_config map[string]any` as an opaque adapter-specific bag
  matches `Toolset.Config any` (line 815) and `ModelConfig.ProviderOpts
  map[string]any` (line 623). Established pattern.

What doesn't work as written:

1. **Versioned config.** docker-agent uses an explicit version field
   (`Version` constant at `pkg/config/latest/types.go:19`, current value
   `"9"`) and keeps frozen snapshots per version (`pkg/config/v5..v8`).
   Adding `harness:` bumps the schema. Migration path:
   - Bump `Version` to `"10"`.
   - Copy `pkg/config/latest/` snapshot into a new `pkg/config/v9/`
     before introducing `harness:` so loaders for older configs still
     work.
   - Add a v9 -> v10 upgrade: a config without `harness:` round-trips
     unchanged. This is a no-op upgrade but the version pump is
     required.
   The PRD says nothing about this. It is mechanical but missing.

2. **`Toolset.UnmarshalYAML` strict-mode field check.** Look at
   `Agents.UnmarshalYAML` (line 147) — it uses
   `yaml.DisallowUnknownField()`. If `harness:` is added at the agent
   level, any YAML field at the top of an agent must be in the struct;
   FR-2's `harness:` field has its own sub-struct with strict unknown-key
   rejection (FR-5: "Adapters MUST document their accepted keys and
   reject unknown keys"). The `harness_config` map should be `map[string]any`
   on the config side but the adapter's parse step rejects unknowns. The
   PRD lumps both into "validation MUST reject unknown keys" without
   making the layering clear: schema accepts opaque map, adapter
   tightens. State it explicitly so the impl doesn't bake unknown-key
   checks into `validate.go`.

3. **`permission_policy` nested struct + `i_understand_the_risk` guard.**
   The pattern is fine but watch the validation. FR-28 says
   "auto_allow MUST be available only with an explicit
   `i_understand_the_risk: true`; otherwise config validation rejects the
   agent." That's a cross-field invariant similar to the existing
   `validateFallback()` (line 57). Easy to implement. Make sure
   `i_understand_the_risk: true` with no `auto_allow` is *also* an error
   ("you said you understood a risk you didn't take") — cleaner UX and
   no policy drift if someone later swaps a real policy in.

4. **`working_dir` resolution.** Section 6.3 says "absolute path or
   relative to the team config dir; resolved at load time." Today the
   filesystem toolset uses path expansion (`pkg/config/latest/types.go`
   line 849-857 for `AllowList`). Reuse that, do not invent a new
   resolver. The PRD should reference the pattern.

5. **Duration parsing for `timeout`.** Use the existing
   `latest.Duration` wrapper (line 295). It already handles "5m" / "30s"
   YAML strings and integer-seconds. Free.

6. **Env allowlist (FR-32).** The schema field should be a typed struct
   that supports both pass-through (`env: {GITHUB_TOKEN: $env}`) and
   value injection. Look at how `Toolset.Env map[string]string`
   (line 829) and how `ProviderConfig.TokenKey` (line 244) work — there
   are two different established patterns. Pick one and reuse; don't
   invent.

Net: schema fits. Version bump and migration shim is the only
non-obvious work; everything else is "copy a sibling field's pattern."

---

## 3. ACP SDK assessment

**The PRD is broadly accurate about ACP, with two corrections.**

What's right:

- `github.com/coder/acp-go-sdk@v0.13.0` is in go.mod.
- The server side (`pkg/acp/run.go`, `pkg/acp/agent.go`) is up and
  proven. `acp.NewAgentSideConnection(acpAgent, stdout, stdin)` at
  `pkg/acp/run.go:34` is the agent-side mirror of what the harness
  adapter needs to do on the client side.
- `NewClientSideConnection` exists exactly as the PRD describes
  (`client.go:16`). Signature:
  ```go
  func NewClientSideConnection(client Client, peerInput io.Writer, peerOutput io.Reader) *ClientSideConnection
  ```
  Note the param ordering — `peerInput` is `io.Writer` (we write to the
  harness's stdin), `peerOutput` is `io.Reader` (we read from the
  harness's stdout). The PRD doesn't show this; the adapter code is
  trivial to get wrong if you map "input/output" the wrong way.

What needs correction:

1. **The PRD says the adapter is the *client* and the harness is the
   *server* (line 461).** Correct under ACP terminology. But docker-agent
   already implements `acp.Agent` (the server interface) in
   `pkg/acp/agent.go`. The client-side handler the adapter implements is
   `acp.Client`, which has a different surface — see `client_gen.go`
   dispatch table at lines 10-206:
   - `SessionUpdate` — the only stream-of-events method; everything else
     is request/response.
   - `RequestPermission` — synchronous, blocking. The PRD's "emit
     `PermissionPending`, wait for TUI/policy, reply" (FR-26-27) maps
     1:1 onto `client.RequestPermission(ctx, req) -> (resp, error)`.
   - `ReadTextFile`, `WriteTextFile`, `CreateTerminal`, `KillTerminal`,
     `TerminalOutput`, `ReleaseTerminal`, `WaitForTerminalExit` — these
     are the sandboxed operations from §4.7. **The PRD's "fs/list_dir"
     in FR-29 does not appear in the v0.13.0 SDK** (no `ListDir` method
     in `client_gen.go`). Either the PRD is referring to a future
     method, the user-space ACP spec includes more than the Go SDK
     surfaces, or this is just wrong. Engineering will hit it on day
     one. Drop `fs/list_dir` from FR-29 or annotate that v1 sandboxing
     only covers the methods the SDK exposes.

2. **The PRD doesn't note that `pkg/acp/` is server-only today.** It
   imports `acp.NewAgentSideConnection`, never `NewClientSideConnection`.
   So the head start is the **types and methods** (the SDK is shared),
   not reusable code in `pkg/acp/`. Don't put the client adapter in
   `pkg/acp/` — that package's job is "expose docker-agent as an ACP
   server to other clients" (line 31-35 of `run.go`). The adapter that
   *talks to* third-party ACP servers belongs in
   `pkg/harness/copilot/` and `pkg/harness/openclaw/`, with shared
   plumbing (sandbox enforcement, permission gate) in `pkg/harness/acp/`
   or `pkg/harness/internal/acp/`. The PRD's OQ-6 ("registry in
   `pkg/harness/`") is right; the adapter location follows from it.

3. **`ClientSideConnection` lifecycle.** The connection exposes
   `Done() <-chan struct{}` (`client.go:24`). v1 should follow the
   pattern from `pkg/acp/run.go:40-45`: `select` on `ctx.Done()` /
   `conn.Done()`. The PRD's process-pool design (OQ-1, idle timeout) needs
   to also handle the conn's `Done()` channel firing for non-idle
   reasons (peer crash, JSON-RPC framing error). The PRD does not
   surface this; add a sentence in §7.4 multi-turn.

4. **Capability negotiation.** Look at `pkg/acp/agent.go:88-112`
   `Initialize` — the server side advertises `LoadSession: false`,
   `SessionCapabilities`, `PromptCapabilities`, `McpCapabilities`. The
   client-side `Initialize` call (`client_gen.go:226`) returns the same
   shape from the harness. The adapter must honor what the harness
   reports (e.g. don't call `ResumeSession` if `Resume` capability is
   absent). The PRD's `AdapterCapabilities` (FR-7) is a *static* function
   ("MUST be a pure function, no process spawn") but ACP capabilities are
   *negotiated at runtime*. This is a tension the PRD doesn't address.
   Resolution: `AdapterCapabilities()` returns the adapter's *static*
   support surface (what we will use if available); per-session ACP
   capability negotiation happens inside `Run` and may downgrade the
   actual session (e.g. emit a `RunError{code: protocol_error}` if the
   harness lacks a capability we require). Document this split.

5. **Cancellation.** `client_gen.go:264` `Cancel(ctx, params
   CancelNotification)` is the right escape valve for FR-22 / FR-10
   timeouts. Use it before the SIGTERM/SIGKILL sequence; some harnesses
   will clean up gracefully on `Cancel` and the rest fall through to
   process kill. The PRD's FR-10 jumps straight to signals — that's
   fine as the floor, but the polite cancel is also worth a sentence.

Net: the head start is real (the SDK is the same), but `pkg/acp/` itself
is not the code to extend; the adapters are net-new and must not live
under `pkg/acp/`.

---

## 4. Event mapping assessment

**The canonical event vocabulary is sensible. The translation layer to
docker-agent's existing events needs more than the PRD admits.**

Existing event surface (`pkg/runtime/event.go`):

| docker-agent runtime event | Maps from canonical | Notes |
|---|---|---|
| `StreamStartedEvent` (line 146) | `RunStarted` | Sub-session entry. |
| `AgentChoiceEvent` (line 163) | `TextMessageDelta` accumulated | TUI consumes `Content` as a streamed assistant message. |
| `AgentChoiceReasoningEvent` (line 182) | `ReasoningDelta` | Already a separate event from text. |
| `PartialToolCallEvent` (line 68) | `ToolCallStarted` + `ToolCallArgsDelta` | Existing event was designed for streaming tool-arg deltas. |
| `ToolCallEvent` (line 91) | end of `ToolCallStarted` (args complete) | When args are done, an atomic tool call event is emitted. |
| `ToolCallResponseEvent` (line 125) | `ToolCallFinished` | Carries result. |
| `ToolCallConfirmationEvent` (line 108) | `PermissionPending` | Already wired to TUI consent flow. |
| `AuthorizationEvent` (line 450) | `PermissionResolved` | Approval/denial response. |
| `StreamStoppedEvent` (line 405) | `RunFinished` | Carries a `Reason`. |
| `ErrorEvent` (line 212) | `RunError` | Already has classification codes (line 203-210: `ErrorCodeModelError`, `ErrorCodeRateLimited`, `ErrorCodeContextExceeded`, `ErrorCodeToolFailed`, ...). |
| `TokenUsageEvent` (line 293) | `RunFinished.Usage` | Per-session usage stays separate from per-run usage. |
| `WarningEvent` (line 250) | adapter-emitted warnings | E.g. "Codex does not stream deltas, single message at end." |

The PRD says "translation layer." Engineering should hear it as:
**the adapter does not get to invent its own event channel. It writes
into the runtime's existing channel (the `Events chan<- Event` in the
PRD's `SubSessionRequest` appendix), and the events it writes are the
existing `runtime.Event` types — not new canonical types.**

Two ways to do this:

**Option A: Canonical events are an internal adapter shape; the
adapter translates inside `Run`.** Adapter consumes harness output,
constructs canonical events, immediately converts each to the matching
`runtime.Event`, sends on the channel. No third type leaks out. Pro:
TUI and orchestrator code do not change. Con: every adapter has the
translation code; harder to test.

**Option B: Canonical events are public; the runtime translates at the
boundary.** Adapter emits `harness.Event` (the PRD's Appendix A type);
a runtime-side translator converts to `runtime.Event` before the
sub-session event loop. Pro: adapter code is uniform and testable in
isolation against the canonical model. Con: one more type to keep in
sync; the translation is the seam everyone fights over for the next
year.

Recommendation: **Option B.** The PRD already implies it (`harness.Event`
struct in Appendix A, `Kind EventKind`). Codify it. The translator goes
in `pkg/harness/translate.go` or in the runtime's harness branch
(`runHarnessForwarding`). FR-17 ("orchestrator must consume without
knowing which harness") is automatic once events are uniform at the
boundary.

Specific gaps the PRD does not address:

1. **`SessionScoped` interface.** `pkg/runtime/event.go:21` —
   sub-session events implement `GetSessionID() string` so the
   persistence observer can filter out sub-session events from the
   parent's persisted history. Every harness-emitted event must satisfy
   this; the translator needs to stamp `sess.ID`. The PRD says nothing.
2. **`AgentContext`** (line 26): every event carries
   `AgentName + Timestamp`. The translator stamps `child.Name()` and
   `time.Now()` (or `r.now()` for testability). PRD silent.
3. **`MessageAddedEvent`** (line 727): the runtime emits this when a
   message is added to the session, and the persistence observer uses
   it to write rows. Harness sub-sessions need to emit one
   `MessageAddedEvent` per `TextMessageEnd` so the conversation reads
   back correctly. The PRD does not mention `session.Message` writing.
4. **`SubSessionCompletedEvent`** (line 748): emitted when the
   sub-session finishes so the parent persists it as a child. The
   harness path must emit this exactly once on clean exit (matching the
   existing `runForwarding` behavior at line 295). PRD silent.
5. **Order invariant on `RunStarted` / `RunFinished` (FR-13) maps to
   the existing `StreamStarted` + `StreamStopped` pair**, and the
   runtime's stream depth balancing (the comment at `agent_delegation.go`
   line 283-285 about "Drain remaining events ... so the TUI's
   streamDepth counter stays balanced" is critical). Translator must
   preserve this. PRD silent.

Net: the event vocabulary is right, the *plumbing* into the existing
event channel is half-specified. Add Option-B language to the PRD and
enumerate the four runtime events the harness path must emit
(`StreamStarted`, `MessageAdded`, `SubSessionCompleted`,
`StreamStopped`).

---

## 5. Session continuity assessment

**Existing session model gets us 80% there. The session-token storage
and the schema migration are the missing pieces.**

What's already in `pkg/session/session.go`:

- `Session.ID` (line 79) — unique. Reusable as the parent ID.
- `Session.ParentID` (line 173) — already wired for sub-sessions.
  Harness sub-sessions just set this.
- `Session.WorkingDir` (line 109) — propagates naturally.
- `Session.AttachedFiles` (line 157) — handled by `newSubSession` at
  `agent_delegation.go:148-152`. Harness sub-sessions should inherit
  the same way.
- `Session.NonInteractive` (line 103) — supports the "background
  harness" path.
- `Item.SubSession` (line 47) — embedded sub-sessions in parent
  history. Harness sub-sessions slot in as full `*Session` values, not
  as a third item type. Good.

What's missing:

1. **Harness session token.** FR-18: "Adapters MUST accept a
   `SubSessionRequest.SessionToken` ... returned from a prior
   `RunFinished` event ... and use it to resume." `Session` has no
   place to store this today. Options:
   - **Add `Session.HarnessSession map[string]string`** (keyed by
     agent name, value is the adapter-opaque token). Persists across
     restarts via the existing SQLite session store.
   - **Add a sibling table `harness_sessions`** in the session store
     schema. More normalized, but for v1 the in-session map is
     enough and avoids a new migration.
   Recommendation: in-session `HarnessSession map[string]string`,
   serialised in the existing `messages` JSON. One field add, zero
   schema migration. PRD silent on this.
2. **`subsessions/<agent-name>/` directory referenced in FR-20.** That
   path is invented — the runtime today does not persist sub-sessions
   to a filesystem directory; sub-sessions are embedded as
   `Item.SubSession` in the parent's `messages` table. FR-20 needs
   rewriting to match reality:
   > docker-agent MUST persist per-subagent harness session tokens on
   > the parent `Session` via `HarnessSession[agentName] = token`. No
   > separate filesystem layout is required.
3. **Stderr log file (FR-11).** `~/.docker-agent/sessions/<session-id>/harness-<n>.stderr`
   — that *is* a filesystem path engineering would have to invent,
   because nothing today writes per-session files. Fine, but call it
   what it is: a *new* filesystem layout, not piggybacking on
   anything. Pick a more discoverable location too — there's no
   precedent in the tree for `~/.docker-agent/sessions/<id>/...`; the
   ACP server uses a SQLite DB at a configurable path
   (`run.go:24`). Suggest `${XDG_STATE_HOME:-~/.local/state}/docker-agent/sessions/<id>/harness-<n>.stderr`
   or sidecar to the session store; bring the maintainer of
   `pkg/session/store/` into the call.
4. **Multi-turn budget for simulated harnesses (FR-19).** Replaying
   prior turns via prompt prepending is fine, but `Session.GetAllMessages`
   already walks the message tree (`session.go:470`) and the harness
   adapter must feed only the *parent's* relevant context, not the
   full team's. Decision: the harness adapter receives the
   `parent.GetAllMessages()` snapshot via `SubSessionRequest`, picks the
   last-N-tokens, prepends. Encode this in the
   `SubSessionRequest` struct (PRD Appendix A) — add
   `PriorTurns []chat.Message`. PRD currently has only `Task` and
   `SystemPrompt`, which is not enough for OQ-9's budget logic.

Net: session model supports the v1 design with one field
(`HarnessSession`) and one new on-disk artifact (stderr log). FR-20 as
written misrepresents the storage layout — rewrite.

---

## 6. Open question answers

**OQ-1 (ACP process pooling, idle timeout):** Pool with 10-min idle
**but make idle timeout per-adapter, not global.** Copilot warms up
slowly (GitHub auth roundtrip); OpenClaw doesn't. Per-adapter
`Capabilities().IdleTimeout time.Duration` with sane defaults: 10m for
Copilot, 2m for OpenClaw. Also: pool keyed by `(agent name, working
dir)` — two subagents of the same kind with different working dirs
MUST NOT share a process (cf NFR-11). Document this in the pool design.

**OQ-2 (cancellation propagation in parallel fan-out):** **Cancel-one,
not cancel-siblings.** Agree with PRD. But add: the orchestrator-level
context that fans out the sibling subagents must NOT be the same
`ctx`; each subagent gets its own derived context with its own
`cancel`. The runtime today does this correctly in
`runCollecting`/`runForwarding` (separate goroutines, separate
contexts) — engineering just needs to preserve it on the harness path.

**OQ-3 (HarnessRaw contents):** **Full raw frame as bytes.** Agree.
But also: a `Source string` field ("opencode-line", "acp-update",
"claude-stream-json") so the consumer knows the wire format. One extra
field, big debugging payoff.

**OQ-4 (auto_allow + observability):** **Emit `PermissionResolved` even
on auto-allow.** Agree. The TUI's existing
`ToolCallConfirmationEvent` flow already follows this pattern
(approval still emits an `AuthorizationEvent` at line 450). Be
consistent.

**OQ-5 (Codex synthetic deltas):** **Stay faithful, document the gap.**
Agree. Faking streaming where the model didn't stream is a debugging
nightmare and lies about timing. Add a UI affordance — a one-time
notice the first time the user sees a non-streaming subagent: "Codex
emits final messages only; this is expected." That's a TUI task, not
an adapter task, but the PRD should call it out.

**OQ-6 (registry location):** **`pkg/harness/` with subpackages per
adapter.** Agree. Concrete layout:
```
pkg/harness/
  harness.go            # HarnessAdapter interface, Event type, SubSessionRequest
  registry.go           # registry by kind
  translate.go          # canonical Event → runtime.Event (Option B from §4)
  sandbox/              # path resolution, env allowlist, terminal guard (FR-29-32)
  claude/               # adapter
  codex/                # adapter
  opencode/             # adapter
  acp/                  # ACP client adapter base (shared by copilot, openclaw)
    copilot/
    openclaw/
```

**OQ-7 (harness-as-orchestrator gating):** **Hard reject at config
validation.** Agree. Add a single rule in `validate.go`: an agent with
`harness:` set cannot have non-empty `sub_agents` or `handoffs`. The
PRD's v1 non-goal becomes a structural invariant, not a runtime check.

**OQ-8 (usage on `RunFinished`):** **Attach raw, no aggregation.**
Agree. Schema: `RunFinished.Usage map[string]any` — same opacity
choice as `harness_config`. Adapter docs say what keys they emit. v1.1
aggregation has a defined source of truth.

**OQ-9 (50% context budget default):** **Defer the answer to impl,
measure on real workloads.** PRD already says this. Concrete
suggestion: instrument the adapter to emit a `Warning` event when
prepending exceeds 60% so we collect data on which budget level
matters. Ship with 50% default and one knob; revisit at v1.1 with
real numbers.

---

## 7. Missing requirements

What engineering will need that the PRD doesn't cover. Not nice-to-haves
— blockers.

1. **Hooks integration.** Every model-backed sub-session today runs
   through the hooks pipeline (pre_tool_use, before_llm_call,
   tool_response_transform, on_agent_switch, subagent_stop —
   `runtime.go:184-205` and `agent_delegation.go:273`,
   `agent_delegation.go:325`). Harness sub-sessions need an explicit
   policy: which hooks fire and where? At minimum, **`on_agent_switch`
   and `subagent_stop` MUST fire** so existing hook configs (snapshot,
   audit, redact-secrets) keep working. Internal hooks
   (`pre_tool_use`, `before_llm_call`) cannot fire because the harness
   owns its own loop. Document this in §4.2. Mark's GM team config has
   hooks attached; he will be the first to hit this.

2. **Permissions / team-level permission patterns.** `team.Permissions()`
   today defines team-wide `allow / ask / deny` patterns
   (`runtime.go:938`) applied to model-driven tool calls. Harness
   tools (Claude Code's `bash`, `edit`, ...) **bypass these patterns
   entirely** because the harness runs the tool itself. The PRD's
   `permission_policy` block (§6.1) is harness-side but silent on
   team-level. Decision needed: do team permissions apply to harness
   ACP `terminal/*` calls? Strong recommendation: **yes**, ACP
   permission prompts go through `team.Permissions()` first, then the
   per-agent `permission_policy`, then the TUI. Otherwise the security
   posture regresses for users who already configured deny patterns.

3. **Telemetry.** `runtime.go:252` `telemetry Telemetry` records
   session start/end/tool calls/errors. Harness sub-sessions must
   record equivalent telemetry: harness kind, cold start latency,
   per-event latency, error code distribution. The success metrics in
   §8 of the PRD ("p99 cold start within NFR-1 budgets",
   "p99 event-stream latency ≤50ms") cannot be measured without it.
   Add `r.telemetry.RecordHarnessStart/Finish/Event` analogues.

4. **Tracing.** Every sub-session today opens an OTel span
   (`agent_delegation.go:411`). Harness sub-sessions need the same:
   `runtime.harness_session` span, attributes for kind, working dir,
   resume vs new. Wire it through `r.startSpan` (`runtime.go:1242`).

5. **`run_skill` and `transfer_task` interaction with harnesses.**
   `run_skill` (registered at `loop.go:40`) spins up a sub-session
   with a skill's system prompt. Can a skill target a harness-backed
   subagent? The PRD doesn't say. Default for v1: **no**, skills
   require model-backed agents. Reject at validation. Otherwise the
   skill system prompt has no clean place to land (FR-3 says
   harnesses are subagents only; skills are not subagents).

6. **TUI rendering contract.** Section 5 success metric says
   "ACP permission prompts surface in TUI with same latency feel as
   model-backed prompts." Today the TUI subscribes to
   `ToolCallConfirmationEvent` and renders an inline dialog. The
   harness path must emit the same event type (per §4 Option B above);
   the TUI then needs no changes. Make this an explicit invariant in
   the PRD; "same latency feel" is unmeasurable, "same event type"
   is enforceable.

7. **Working-directory default.** If `harness.working_dir` is unset,
   FR-2 says it defaults to "the session's working dir". Cross-ref:
   `session.WorkingDir` (`session.go:109`) can also be empty (see
   `acp/agent.go:166-181` for the empty-cwd case). Spec the fallback
   chain explicitly: `harness.working_dir` ?? `session.WorkingDir` ??
   `os.Getwd()`. The PRD waves at this.

8. **`AdapterCapabilities` as a registry-time vs run-time concern.**
   See §3 point 4 above — split static capabilities (what the adapter
   *will use* if available) from negotiated capabilities (what the
   harness session *actually has*). Without this split, FR-7's "pure
   function, no side effects" conflicts with the real ACP behavior.

9. **Concurrency limit enforcement (NFR-10).** "Default concurrency
   limit per team: 4." Where does this live? The bgAgents handler
   (`runtime.go:238`, `agenttool.NewHandler(r)`) has its own
   concurrency model. Decision: harness concurrency rides on bgAgents
   for the parallel-fanout case (JTBD 3) and is unlimited for
   sequential `transfer_task`. Spec which one applies when.

10. **`Run` returning `nil` on clean shutdown vs returning a non-nil
    error.** FR-8 says: "All errors MUST be surfaced as `RunError`
    events; `Run` returns `nil` on clean shutdown and a non-nil error
    only for adapter-internal bugs that cannot be expressed as
    `RunError`." Good rule. But the runtime needs to know what to do
    with a non-nil error: log? `panic`? Convert to an
    `ErrorWithCode(ErrorCodeToolFailed, ...)`? Decision: convert
    silently to an `ErrorEvent` with code `harness_crashed`; never
    propagate to the orchestrator loop. State it.

11. **Two harness instances of the same kind with the same working
    dir.** NFR-11 says they must be isolated processes. What about
    the *session token* — does Claude Code allow two concurrent
    sessions resuming the same `--resume` ID? Probably not. Spec the
    contract: an agent's session token is owned by one process at a
    time; concurrent reuse is an error. Otherwise users will deploy
    two instances of `@code-reviewer` and corrupt the multi-turn
    history.

12. **Test infrastructure for adapter integration tests.** Appendix B
    says "real binary per adapter, in CI behind a build tag." Today
    CI does not have `claude`, `codex`, `opencode`, `copilot`,
    `openclaw` on the runners. This is a meaningful infra ask
    (image build, secret management for `ANTHROPIC_API_KEY` /
    `OPENAI_API_KEY` / `GITHUB_TOKEN`, cost budget for CI calls).
    Surface to the platform team before the PRD lands.

---

## 8. Recommended implementation order

The critical path is **plumbing first, adapters second.** Adapters are
parallelizable once the runtime branch and the canonical type model
exist. Adapters built against a missing runtime branch are wasted work.

**Phase 0 — Foundations (1 engineer, 1 week)**

1. Bump config version to `"10"`; freeze `pkg/config/v9` snapshot.
2. Add `HarnessConfig` to `pkg/config/latest/types.go`, `Validate()`
   rule for `model:`/`harness:` exclusivity, sub-agent / handoff
   rejection for harness-backed agents.
3. Add `WithHarness` to `pkg/agent/opts.go` and `HasHarness()` /
   `harness` field to `*Agent`.
4. Add `Session.HarnessSession map[string]string` field.
5. Wire `pkg/teamloader/teamloader.go` to build harness-backed
   `*Agent` instances (no resolution of `Model`, no fallbacks).
6. Stub `pkg/harness/`: `HarnessAdapter` interface, `Event` /
   `EventKind`, `SubSessionRequest`, empty registry.

**Phase 1 — Runtime branch + first adapter (1 engineer, 2 weeks)**

7. Implement `runHarnessForwarding` in
   `pkg/runtime/agent_delegation.go`. Translator
   (`pkg/harness/translate.go`) emits the four required runtime
   events (`StreamStarted`, `MessageAdded`, `SubSessionCompleted`,
   `StreamStopped`) and the optional ones (`AgentChoice*`,
   `ToolCall*`, `ToolCallConfirmation`, `Error`, `Warning`).
8. Implement `runHarnessCollecting` for the background-agent path.
9. Implement Claude Code adapter end-to-end (lowest gap count per
   §7.1, native multi-turn). This is the "prove the whole stack
   works" milestone. It is also Mark's most-used harness — dogfood
   value is highest here.
10. Hooks integration: wire `on_agent_switch` and `subagent_stop`
    around the harness path so existing hook configs keep working.

**Phase 2 — Parallel adapter build (3 engineers, 2 weeks)**

These can ship independently once Phase 1 lands.

11. Codex adapter (simulated multi-turn, no streaming deltas; tests
    the simulated-multi-turn budget logic).
12. OpenCode CLI adapter (mostly clone of Codex with different
    parser; surfaces the "no per-call system prompt" warning UX).
13. ACP base in `pkg/harness/acp/` (the `acp.Client` impl, sandbox
    enforcement, permission gate) + Copilot adapter on top.

**Phase 3 — Last adapter + hardening (1 engineer, 1 week)**

14. OpenClaw adapter (delta from Copilot is small).
15. Sandbox hostile-path tests (FR-29-31): symlink, `..`, absolute
    outside root. P0 security tests.
16. Goleak / process-orphan tests (FR-10, NFR-5, NFR-6). Must pass
    1000 consecutive runs in CI.
17. Telemetry, tracing, and the `/harness` TUI affordance (status
    panel, stderr log access).

**Phase 4 — Dogfood + GA (1 week)**

18. Migrate Mark's GM team config to use ≥2 harness-backed subagents
    (success metric §8).
19. Two-harness side-by-side benchmark (JTBD 3) verified end-to-end.
20. Doc page in the OpenCode docs site (cross-link from
    /docs/agents).

**Critical-path dependencies:**

- Phase 0 → Phase 1 (hard).
- Phase 1 → Phase 2 (hard; nothing else builds on the runtime
  branch).
- Within Phase 2, the three adapters are independent.
- Phase 4 is gated on Phase 2 + Phase 3.

**Total elapsed: 6-7 weeks with 3 engineers** (1 dedicated to runtime
plumbing, 2 on adapters, overlap during Phase 2). Maps cleanly onto the
PRD's "v1 ships 5 harnesses" target.

**Watch-items / risks engineering should escalate:**

- CI runner provisioning for adapter integration tests (§7 point 12).
  Surface this **at Phase 0** so it's solved by Phase 2.
- ACP `fs/list_dir` not in the v0.13.0 SDK (§3 point 1). Resolve
  before locking the sandbox spec.
- Hooks policy on harness sub-sessions (§7 point 1). Mark's team
  config will hit this; get a decision from product before Phase 1.
- Team-level permission patterns interacting with ACP permission
  prompts (§7 point 2). Security implication, route through CSO
  review.

---

## Summary

| Section | Status | Required changes |
|---|---|---|
| 1. Insertion point | Partial | Rewrite as the file *and the two functions inside it*; enumerate the ~10 files actually touched. |
| 2. Config schema | Approved with addenda | Add version bump to "10"; freeze v9 snapshot; clarify schema-vs-adapter strict-unknown-keys layering. |
| 3. ACP SDK | Mostly correct | Drop `fs/list_dir` from FR-29 (not in v0.13.0 SDK); clarify client adapter does NOT live in `pkg/acp/`; resolve static-vs-negotiated capabilities tension. |
| 4. Event mapping | Partial | Adopt Option B (canonical events public, translator at the runtime boundary). Enumerate the four runtime events the harness path must emit. |
| 5. Session continuity | Partial | Add `Session.HarnessSession` field; rewrite FR-20 to match actual storage (no `subsessions/` directory exists); spec the stderr log path. |
| 6. Open questions | All answered | OQ-1: pool with per-adapter idle timeout, keyed by (agent, wd). OQ-2: agree. OQ-3: agree + `Source` field. OQ-4: agree. OQ-5: agree + TUI notice. OQ-6: agree, with concrete layout. OQ-7: agree, structural validation rule. OQ-8: agree. OQ-9: ship 50%, instrument, revisit. |
| 7. Missing requirements | 12 items | Hooks, team permissions, telemetry, tracing, skill interaction, TUI contract, working-dir fallback, capability split, concurrency limit owner, error return semantics, same-kind session-token ownership, CI infra. |
| 8. Impl order | Recommended | Phase 0 (foundations) → Phase 1 (runtime branch + Claude Code) → Phase 2 (parallel adapters) → Phase 3 (OpenClaw + hardening) → Phase 4 (dogfood + GA). 6-7 weeks with 3 engineers. |

**Recommendation: REVISE. Land the items in the table, then approve.**
The hardest revision is §4 (event mapping translation layer); the rest
are mechanical or "add a sentence." Engineering should not start
adapter work until §1, §4, and §5 are resolved on paper.
