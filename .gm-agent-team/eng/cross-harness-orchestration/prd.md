# PRD: Cross-Harness Orchestration

**Owner:** docker-agent eng
**Status:** Draft for arch + DX review
**Target:** v1 ships 5 harnesses (Claude Code, Codex, OpenCode CLI, Copilot CLI via ACP, OpenClaw via ACP). Cursor + OpenCode SSE deferred.
**Insertion point:** `pkg/runtime/agent_delegation.go`, new `runHarnessSession` path branching on `agent.HasHarness()`.

---

## 1. Problem statement

docker-agent today is a Go CLI agent framework where every agent in a team is backed by a **model** -- a raw LLM API call wrapped in docker-agent's own agent loop (tool calling, planning, session memory, TUI).

The model-backed loop is good. But model providers now ship their own native **harnesses** -- Claude Code CLI, Codex CLI, OpenCode, GitHub Copilot CLI, OpenClaw -- that bundle a model with provider-tuned prompts, tool sets, safety policies, and context strategies. For coding work specifically, a vendor harness usually outperforms a generic model call because the vendor has tuned the harness to its own model's strengths.

Mark Cavage (Docker COO, primary user) runs a GM agent team pattern: one orchestrator delegates to specialist subagents. He wants the same pattern but with the parent able to dispatch to harnesses instead of raw models. Concretely: the orchestrator should be able to send a coding task to Claude Code CLI, a separate task to Codex, get structured results back, and continue the conversation -- all inside docker-agent's existing TUI, session model, and team config.

The pain this solves:

- **No way to use a vendor harness as a subagent today.** You either run docker-agent (and lose Claude Code's tuning) or run Claude Code directly (and lose docker-agent's orchestration, TUI, and team config).
- **Manual harness juggling.** Running Claude Code in one terminal, Codex in another, and copy-pasting outputs is what users do today. It does not scale past two harnesses and does not preserve context.
- **Multi-model coding workflows are stuck.** Picking the right harness per task (Claude Code for refactors, Codex for greenfield, Copilot for IDE-adjacent edits) requires an orchestrator that can route. docker-agent is the natural home because it already has the team config, session state, and TUI.

Why now: ACP (Agent Client Protocol) just gave us a stable bidirectional protocol for Copilot and OpenClaw. The Go SDK (`github.com/coder/acp-go-sdk`) is already in our go.mod -- we ship `docker-agent serve acp` today, so the wire format is proven. Self-contained harnesses (Claude Code, Codex, OpenCode) ship stable streaming JSON formats. The technical risk is now low enough to commit.

---

## 2. Goals and non-goals

### Goals (v1)

1. Let a user declare a harness-backed subagent in team YAML and have an orchestrator delegate to it.
2. Ship adapters for 5 harnesses: Claude Code CLI, Codex CLI, OpenCode CLI, Copilot CLI (ACP), OpenClaw (ACP).
3. Normalize every harness to a 12-event canonical event set (AG-UI vocabulary) so the orchestrator and TUI do not need to know which harness ran.
4. Support multi-turn sessions: a harness subagent can be invoked, return, and be invoked again with prior context preserved (when the underlying harness supports it).
5. Surface ACP permission prompts to the docker-agent TUI and route responses back.
6. Sandbox ACP `terminal/*` and filesystem operations to the session's working directory.
7. Make adapter capabilities introspectable (`AdapterCapabilities`) so the orchestrator knows what each harness can and cannot do.

### Non-goals (v1)

1. **Replacing the model-backed runtime.** Harness-backed agents are an additive second backing type, not a rewrite.
2. **Harness-as-orchestrator.** Only model-backed agents can be orchestrators in v1. Harnesses are subagents only.
3. **Custom tool injection into harnesses.** Self-contained harnesses run their own tools. We do not pass docker-agent's tool set into Claude Code.
4. **Cursor adapter.** Deferred -- output schema not stable enough to commit.
5. **OpenCode SSE transport.** Deferred to v1.1 (needed for per-call system prompts).
6. **Sub-harness delegation.** A harness-backed subagent cannot itself spawn harness subagents in v1.
7. **AG-UI wire format compatibility.** We borrow the vocabulary, not the wire format. No promise that docker-agent events serialize to AG-UI JSON.
8. **Cost/usage aggregation across harnesses.** Per-harness usage is surfaced raw; no unified billing view in v1.

---

## 3. User stories (JTBD)

**JTBD 1 -- Coding orchestrator routes to the best harness per task.**
When Mark is doing a multi-part refactor across a Go service, he wants his orchestrator to send the algorithmic core to Claude Code (best at large refactors), the test scaffolding to Codex (best at greenfield), and a config tweak to Copilot, so he gets each part done with the right tool without manually switching terminals.

**JTBD 2 -- Subagent specialization in a team.**
When Mark configures his GM team, he wants to declare `@code-reviewer` as Claude Code CLI-backed and `@prototype-builder` as Codex-backed in YAML, so his existing orchestrator routing works unchanged and the right harness picks up each role.

**JTBD 3 -- Compare two harnesses on the same task.**
When Mark is benchmarking which harness handles a class of problems better, he wants to dispatch the same task to two harness subagents in parallel from a single orchestrator turn, so he can compare outputs side-by-side without scripting it.

**JTBD 4 -- Long-running harness session with checkpointing.**
When a Claude Code subagent runs for 90 seconds on a 30-file refactor, Mark wants to see the streamed text, tool calls, and final summary in the docker-agent TUI in real time, and have docker-agent persist the session so he can resume the conversation later.

**JTBD 5 -- ACP harness with permission prompts.**
When Copilot CLI (running as an ACP subagent) wants to write a file outside the working directory, Mark wants the permission prompt to surface in docker-agent's TUI with the same UX as model-backed permission prompts, so he doesn't lose context-switching to a separate process.

---

## 4. Functional requirements

Numbered, testable. Every requirement is verifiable by a test or a TUI inspection.

### 4.1 Config schema

**FR-1.** A team YAML MUST allow declaring a subagent with `harness:` instead of `model:`. Both keys are mutually exclusive. Validation MUST reject configs that set both or neither.

**FR-2.** The `harness:` field MUST be a struct with at minimum: `kind` (enum: `claude-code` | `codex` | `opencode` | `copilot` | `openclaw`), and optional `command`, `args`, `env`, `working_dir`, `timeout`, and `harness_config` (kind-specific knobs).

**FR-3.** `agent.HasHarness()` MUST return true iff `harness:` is set, and is the branch point in `agent_delegation.go`.

**FR-4.** Config validation MUST verify the harness binary is on PATH (or at the configured `command` path) at team-load time, and MUST emit a clear error naming the missing binary and an install hint.

**FR-5.** `harness_config` MUST be passed through to the adapter as an opaque map. Adapters MUST document their accepted keys and reject unknown keys with a clear error.

### 4.2 Adapter behavior

**FR-6.** Every adapter MUST implement the full `HarnessAdapter` interface:

```go
type HarnessAdapter interface {
    Name() string
    Capabilities() AdapterCapabilities
    Run(ctx context.Context, req SubSessionRequest) error
}
```

**FR-7.** `Capabilities()` MUST be a pure function (no side effects, no process spawn). It returns:

```go
type AdapterCapabilities struct {
    Protocol     ProtocolClass    // "stream" | "acp"
    Requires     HostRequirements // binary name, min version, env vars
    Features     AdapterFeatures  // supports_multi_turn, supports_per_call_system_prompt, streams_text_deltas, streams_reasoning
    BuiltInTools []string         // names only; informational
}
```

**FR-8.** `Run` MUST emit events through the channel/sink supplied in `SubSessionRequest` and MUST NOT panic on the caller's goroutine. All errors MUST be surfaced as `RunError` events; `Run` returns `nil` on clean shutdown and a non-nil error only for adapter-internal bugs that cannot be expressed as `RunError`.

**FR-9.** Adapters MUST be process-per-session. Multiple concurrent subagents of the same kind MUST run in independent processes.

**FR-10.** Adapters MUST clean up child processes on context cancellation. Specifically: cancel context → SIGTERM → wait 5s → SIGKILL. A test MUST verify no orphan processes after cancellation.

**FR-11.** Adapters MUST forward stderr from the child process to a per-session log file under `~/.docker-agent/sessions/<session-id>/harness-<n>.stderr`. Stderr MUST NOT be parsed for events.

### 4.3 Event flow (canonical event set)

**FR-12.** All adapters MUST emit events from this set only:

```
RunStarted, RunFinished, RunError
TextMessageStart, TextMessageDelta, TextMessageEnd
ReasoningStart, ReasoningDelta, ReasoningEnd
ToolCallStarted, ToolCallFinished
ToolCallArgsDelta             (optional, if harness streams tool args)
PermissionPending, PermissionResolved   (ACP only)
HarnessRaw                    (escape hatch, opt-in via config)
```

**FR-13.** Every session MUST start with exactly one `RunStarted` and end with exactly one of `RunFinished` or `RunError`. Tests MUST verify this invariant.

**FR-14.** `TextMessage*` events MUST be balanced: every `TextMessageStart` MUST be followed by zero or more `TextMessageDelta`s and exactly one `TextMessageEnd` with the same message ID. Same rule for `Reasoning*` and `ToolCall*`.

**FR-15.** `HarnessRaw` events MUST be off by default and enabled per-adapter via `harness_config.emit_raw: true`. When on, the raw line/frame is included verbatim alongside the canonical event.

**FR-16.** Codex adapter MUST NOT emit `TextMessageDelta` (Codex does not stream text). It emits `TextMessageStart` → `TextMessageEnd` with the full text in a single delta-equivalent at end, OR sets a `Features.StreamsTextDeltas = false` capability and emits a single combined message event. Decision deferred to adapter spec section -- see §7.2.

**FR-17.** The orchestrator MUST be able to consume the event stream without knowing which harness produced it. A test MUST replay a recorded event stream through the orchestrator and verify identical behavior for each adapter.

### 4.4 Session continuity (multi-turn)

**FR-18.** Adapters whose `Features.SupportsMultiTurn = true` MUST accept a `SubSessionRequest.SessionToken` opaque to docker-agent, returned from a prior `RunFinished` event, and use it to resume.

**FR-19.** For harnesses without native multi-turn (e.g. Codex CLI), the adapter MUST simulate multi-turn by prepending prior turns to the prompt up to a configurable token budget (default 50% of harness context window). When the budget is exceeded, the adapter MUST emit a `RunError` with code `context_exhausted`.

**FR-20.** docker-agent MUST persist per-subagent session state (token, last N turns, working dir, env snapshot) in the existing session store under `subsessions/<agent-name>/`.

### 4.5 Error handling

**FR-21.** `RunError` MUST carry: `code` (enum below), `message` (human-readable), `retryable` (bool), `cause` (optional underlying error string).

Error codes: `binary_not_found`, `binary_version_mismatch`, `auth_failed`, `network_error`, `timeout`, `context_exhausted`, `permission_denied`, `harness_crashed`, `protocol_error`, `cancelled`, `unknown`.

**FR-22.** Timeouts default to 5 minutes per `Run` call, configurable per agent. Hitting the timeout MUST emit `RunError{code: timeout, retryable: true}` and terminate the child process per FR-10.

**FR-23.** If a harness emits malformed JSON/JSON-RPC, the adapter MUST emit `RunError{code: protocol_error, retryable: false}`, include the offending bytes (truncated to 1KB) in the cause field, and tear down.

**FR-24.** If a harness process exits with non-zero status before `RunFinished`, the adapter MUST emit `RunError{code: harness_crashed}` with the exit code and last 4KB of stderr in the cause field.

**FR-25.** The orchestrator MUST receive every `RunError` as a tool-call failure (analogous to a model tool error), so existing retry/fallback logic in the model-backed loop applies unchanged.

### 4.6 Permission handling (ACP)

**FR-26.** ACP adapters MUST forward every `session/request_permission` JSON-RPC call from the harness as a `PermissionPending` canonical event with: request ID, operation (e.g. `fs/write_text_file`, `terminal/create`), target path or command, and `reason` text from the harness.

**FR-27.** The TUI (and any orchestrator policy layer) MUST be able to respond with `PermissionResolved{decision: allow | deny, scope: once | session | always}`. The adapter MUST translate this to the ACP `session/permission_response` reply within 30s; otherwise the harness's own timeout takes over and the adapter MUST emit `RunError{code: permission_denied}`.

**FR-28.** Policy hooks: an agent's config MAY specify `permission_policy: auto_allow | auto_deny | prompt` per operation kind. Default is `prompt`. `auto_allow` MUST be available only with an explicit `i_understand_the_risk: true` field; otherwise config validation rejects the agent.

### 4.7 Sandboxing (ACP terminal/* and fs)

**FR-29.** All ACP `fs/read_text_file`, `fs/write_text_file`, and `fs/list_dir` operations MUST be resolved against an explicit sandbox root (the agent's `working_dir`, defaulting to the docker-agent session's working dir). Paths that resolve outside the sandbox root (after symlink resolution) MUST be rejected with ACP error `permission_denied` and a `PermissionPending` event MUST NOT be raised.

**FR-30.** `terminal/create` MUST set the child shell's CWD to the sandbox root and MUST refuse commands that contain `cd` to a path outside the root (best-effort string match) unless `permission_policy.terminal = allow_unrestricted` is explicitly set.

**FR-31.** All sandbox enforcement MUST occur in the adapter (not the harness). Tests MUST verify that a hostile harness sending a `..`-traversal path is rejected.

**FR-32.** Environment variables exposed to the harness child process MUST be filtered through an allowlist: `PATH`, `HOME`, `USER`, `LANG`, `LC_*`, `TERM`, plus any explicitly listed in `harness.env`. Docker-agent's own secrets (API keys for other providers) MUST NOT leak unless explicitly passed.

---

## 5. Non-functional requirements

### 5.1 Performance

**NFR-1.** Cold start budget per harness: ≤3s from `Run` invocation to first event for Claude Code; ≤2s for Codex, OpenCode; ≤1.5s for ACP harnesses (no model warmup on adapter side). A startup slower than budget MUST be logged as a warning but is not a failure.

**NFR-2.** Adapter overhead (event normalization, JSON parse, channel send) MUST be ≤5ms p99 per event on a developer laptop. Measured via benchmark.

**NFR-3.** The adapter MUST stream events through the orchestrator as they arrive. End-to-end latency from harness stdout to TUI render MUST be ≤50ms p99.

### 5.2 Reliability

**NFR-4.** Adapter MUST recover from a single transient stderr/stdout read error (EAGAIN, partial line) without terminating. Two consecutive read errors → `RunError{code: protocol_error}`.

**NFR-5.** No goroutine leaks. Every `Run` invocation MUST cleanly stop all goroutines it started before returning. Verified by `goleak` in tests.

**NFR-6.** Cancellation MUST be observed within 200ms (context cancel → all goroutines exit, child process signaled).

### 5.3 Security

**NFR-7.** Sandbox enforcement (FR-29 through FR-32) is a security boundary, not a courtesy. Bypass is a P0 bug.

**NFR-8.** Harness binaries are NOT verified by checksum in v1 (out of scope). PATH lookup is logged so the user can audit which binary was loaded.

**NFR-9.** Credentials for vendor harnesses (e.g. Anthropic API key for Claude Code, OpenAI key for Codex) are the harness's responsibility -- docker-agent does not store or forward them. The adapter MAY pass an env var name → value mapping from `harness.env`.

### 5.4 Concurrency

**NFR-10.** Multiple harness subagents MUST be able to run in parallel from one orchestrator turn. Default concurrency limit per team: 4 (configurable). Exceeding it MUST queue, not error.

**NFR-11.** Two subagents of the same kind (e.g. two Claude Code instances) MUST be isolated -- separate working dirs unless explicitly configured to share, separate processes, separate ACP connections.

---

## 6. Config schema

### 6.1 Schema reference

```yaml
agents:
  - name: string                    # required, unique per team
    harness:                        # required if model: omitted
      kind: enum                    # claude-code | codex | opencode | copilot | openclaw
      command: string               # optional, override binary path
      args: [string]                # optional, additional args appended to adapter defaults
      env: map[string]string        # optional, allowlisted env vars
      working_dir: string           # optional, defaults to session working dir
      timeout: duration             # optional, default 5m
      permission_policy:            # optional, ACP only
        fs_write: enum              # prompt | auto_allow | auto_deny
        terminal: enum              # prompt | auto_allow | allow_unrestricted | auto_deny
        i_understand_the_risk: bool # required if any auto_allow or allow_unrestricted
      harness_config:               # optional, adapter-specific map
        emit_raw: bool              # default false
        # ... kind-specific keys, see §7
```

### 6.2 Examples

**Claude Code subagent:**

```yaml
agents:
  - name: code-reviewer
    description: Deep code review using Claude Code
    harness:
      kind: claude-code
      timeout: 10m
      harness_config:
        max_turns: 20
        system_append: "Focus on security and correctness."
```

**Codex subagent for greenfield work:**

```yaml
agents:
  - name: prototype-builder
    description: New feature prototyping with Codex
    harness:
      kind: codex
      working_dir: /tmp/proto
      harness_config:
        model: gpt-5-codex   # passthrough to codex --model
        reasoning_effort: high
```

**OpenCode CLI subagent:**

```yaml
agents:
  - name: refactor-helper
    description: OpenCode-backed refactoring
    harness:
      kind: opencode
      harness_config:
        # OpenCode CLI has no per-call system prompt; warn surfaced at load
        task_prefix: "You are a refactoring assistant. "
```

**Copilot CLI via ACP:**

```yaml
agents:
  - name: copilot-edit
    description: GitHub Copilot CLI in ACP mode
    harness:
      kind: copilot
      working_dir: ./src
      permission_policy:
        fs_write: prompt
        terminal: auto_deny
      harness_config:
        acp_handshake_timeout: 5s
```

**OpenClaw subagent with auto-allow (explicit risk acknowledgment):**

```yaml
agents:
  - name: openclaw-batch
    description: OpenClaw running batch fs ops in a sandbox
    harness:
      kind: openclaw
      working_dir: ./scratch
      permission_policy:
        fs_write: auto_allow
        terminal: prompt
        i_understand_the_risk: true
```

### 6.3 Validation rules

- `model:` and `harness:` are mutually exclusive (FR-1).
- `harness.kind` MUST be one of the v1 enum values.
- `permission_policy.i_understand_the_risk` MUST be `true` if any nested policy is `auto_allow` or `allow_unrestricted`.
- `working_dir` MUST be an absolute path or relative to the team config dir; resolved at load time.
- `harness_config` unknown keys → load error with the unknown key name.

---

## 7. Adapter specs

One section per v1 harness. Each covers: invocation, flags, event mapping, gaps, multi-turn.

### 7.1 Claude Code CLI

**Binary:** `claude` (Anthropic Claude Code CLI). Min version: latest stable at integration time, pinned in `Capabilities().Requires`.

**Invocation:**
```
claude --output-format stream-json --print "<task>" [--resume <session-id>] [--max-turns N] [--append-system-prompt <text>]
```

**Why these flags:**
- `--output-format stream-json` → NDJSON to stdout, one event per line.
- `--print` → non-interactive, single task, exits after run.
- `--resume <id>` → multi-turn resume (Claude Code supports native session IDs).
- `--max-turns` → bound runaway loops.
- `--append-system-prompt` → injects orchestrator-level guidance.

**Event mapping (Claude Code → canonical):**

| Claude Code event | Canonical event |
|---|---|
| `system` (init) | `RunStarted` (extract session_id, model, tools) |
| `assistant.message_start` | `TextMessageStart` |
| `assistant.message_delta` (text) | `TextMessageDelta` |
| `assistant.message_delta` (thinking) | `ReasoningDelta` |
| `assistant.message_stop` | `TextMessageEnd` |
| `tool_use_start` | `ToolCallStarted` |
| `tool_use_delta` | `ToolCallArgsDelta` |
| `tool_result` | `ToolCallFinished` |
| `result` (final) | `RunFinished` (with usage, cost, session_id) |
| Stream close before `result` | `RunError{code: harness_crashed}` |

**Known gaps:**
- None blocking. Native multi-turn, native streaming, native reasoning.
- Cost: cold start 1-3s (model load on Anthropic side).

**Multi-turn:** Use native `--resume <session-id>`. Adapter persists session_id in `SubSessionRequest.SessionToken` on `RunFinished`.

**Capabilities:**
```go
AdapterCapabilities{
  Protocol: "stream",
  Requires: HostRequirements{Binary: "claude", EnvVars: []string{"ANTHROPIC_API_KEY"}},
  Features: AdapterFeatures{
    SupportsMultiTurn: true,
    SupportsPerCallSystemPrompt: true,
    StreamsTextDeltas: true,
    StreamsReasoning: true,
  },
  BuiltInTools: []string{"bash", "edit", "read", "write", "grep", "glob", ...},
}
```

### 7.2 Codex CLI

**Binary:** `codex`. Min version: pinned.

**Invocation:**
```
codex --json [--model <id>] [--reasoning-effort <low|medium|high>] "<task>"
```

**Why these flags:**
- `--json` → JSONL stdout, atomic tool+result objects.
- `--model`, `--reasoning-effort` → passthrough from `harness_config`.

**Event mapping:**

| Codex event | Canonical event |
|---|---|
| `session.start` | `RunStarted` |
| `reasoning.start` / `reasoning.delta` / `reasoning.end` | `Reasoning*` |
| `message` (final text, no streaming) | `TextMessageStart` + single combined → `TextMessageEnd`. See FR-16. |
| `tool_call` (atomic, includes args + result) | `ToolCallStarted` immediately followed by `ToolCallFinished` |
| `session.end` | `RunFinished` |

**Known gaps:**
- **No text deltas.** Codex emits final messages only. Adapter sets `Features.StreamsTextDeltas = false`. TUI MUST treat absence of deltas as expected, not as a bug. Decision (FR-16 resolution): emit `TextMessageStart` → `TextMessageEnd` with the full text attached to `TextMessageEnd.Content`. No synthetic delta.
- **No native session resume in CLI.** Adapter simulates multi-turn via prompt prepending (FR-19).

**Multi-turn:** Adapter-managed transcript replay. Token budget defaults to 50% of context window; configurable via `harness_config.multi_turn_budget_tokens`.

**Capabilities:**
```go
Features: AdapterFeatures{
  SupportsMultiTurn: true,           // simulated
  SupportsPerCallSystemPrompt: true, // codex supports system via flag or env
  StreamsTextDeltas: false,
  StreamsReasoning: true,
}
```

### 7.3 OpenCode CLI

**Binary:** `opencode`. Min version: pinned.

**Invocation:**
```
opencode --format json "<task>"
```

**Why these flags:**
- `--format json` → NDJSON output.

**Event mapping:** Similar to Claude Code; OpenCode emits atomic tool+result pairs, text in chunks (not deltas in all cases -- treat as deltas where present, single message otherwise).

**Known gaps:**
- **No per-call system prompt** in CLI mode. Adapter prepends `harness_config.task_prefix` to the task string and emits a one-time warning at team-load time: `"opencode CLI does not support per-call system prompts; using task prefix. See OpenCode SSE in v1.1."`
- SSE transport (which fixes this) is deferred to v1.1.

**Multi-turn:** Simulated via prompt prepending (same as Codex).

**Capabilities:**
```go
Features: AdapterFeatures{
  SupportsMultiTurn: true,           // simulated
  SupportsPerCallSystemPrompt: false,
  StreamsTextDeltas: true,           // best-effort
  StreamsReasoning: false,
}
```

### 7.4 Copilot CLI (ACP)

**Binary:** `copilot` (GitHub Copilot CLI in ACP mode). Min version: pinned.

**Invocation:**
```
copilot --acp
```
Adapter then establishes JSON-RPC 2.0 over the binary's stdio using `acp-go-sdk` `NewClientSideConnection`.

**Wire:** JSON-RPC 2.0. Bidirectional. Adapter is the **client**; harness is the **server**.

**ACP methods the adapter calls (outbound):**
- `initialize` → handshake, capability exchange. Timeout configurable via `harness_config.acp_handshake_timeout` (default 5s).
- `session/new` → start a session.
- `session/prompt` → send the task.
- `session/cancel` → cancellation.

**ACP methods the adapter handles (inbound from harness):**
- `session/update` → stream events. Adapter maps these to canonical `TextMessage*`, `ToolCall*`, `Reasoning*`.
- `fs/read_text_file`, `fs/write_text_file`, `fs/list_dir` → adapter executes against sandbox (FR-29).
- `terminal/create`, `terminal/write_stdin`, `terminal/wait`, `terminal/kill` → adapter executes inside sandbox (FR-30).
- `session/request_permission` → emit `PermissionPending`, await TUI/policy response, reply (FR-26, FR-27).

**Event mapping (`session/update` → canonical):**
| ACP update | Canonical event |
|---|---|
| `agent_message_chunk` | `TextMessageDelta` (bracketed by Start/End per message) |
| `agent_thought_chunk` | `ReasoningDelta` |
| `tool_call` | `ToolCallStarted` |
| `tool_call_update` (status: in_progress) | `ToolCallArgsDelta` |
| `tool_call_update` (status: completed/failed) | `ToolCallFinished` |
| `plan` | `HarnessRaw` (if `emit_raw`) |

**Known gaps:**
- ACP `terminal/*` and `fs/*` execution is a security surface. See §4.7. This is the main reason ACP has a stricter sandbox spec.
- Some Copilot tool calls do not have a stable display name; adapter MUST use the ACP-provided `name` field verbatim.

**Multi-turn:** Native via ACP session lifetime. Session token is the ACP session ID. Reconnecting to a closed session is not supported in v1 -- multi-turn requires the same adapter process to stay alive across orchestrator turns. The runtime MUST keep the adapter process pooled until the team session ends or an idle timeout (default 10 min) elapses.

**Capabilities:**
```go
AdapterCapabilities{
  Protocol: "acp",
  Requires: HostRequirements{Binary: "copilot", EnvVars: []string{"GITHUB_TOKEN"}},
  Features: AdapterFeatures{
    SupportsMultiTurn: true,
    SupportsPerCallSystemPrompt: true,
    StreamsTextDeltas: true,
    StreamsReasoning: true,
  },
}
```

### 7.5 OpenClaw (ACP)

**Binary:** `openclaw`. Min version: pinned.

**Invocation, wire, event mapping:** Identical pattern to Copilot (§7.4) -- both use ACP via `acp-go-sdk`.

**Differences from Copilot:**
- No `GITHUB_TOKEN` requirement; uses its own auth.
- Different built-in tool set; adapter must not hardcode tool names.
- Plan events from OpenClaw are more verbose; advise `emit_raw: false` by default.

**Capabilities:** Same shape as Copilot.

---

## 8. Success metrics

What we measure to know v1 shipped successfully.

**Adoption (90 days post-GA):**
- ≥10 docker-agent users have a harness-backed agent in their team YAML.
- ≥3 distinct harness kinds in use across the active user base (proves the multi-harness premise, not just "Claude Code wrapper").
- Mark's GM team config includes ≥2 harness-backed subagents (dogfood signal).

**Reliability:**
- p99 successful `RunFinished` rate ≥98% across all harnesses (excluding user-cancellation and auth errors).
- Zero goroutine/process leaks in CI over 1000 consecutive session runs.
- Zero sandbox escapes reported (security boundary, FR-29-FR-32).

**Performance:**
- p99 cold start within NFR-1 budgets.
- p99 event-stream latency (harness stdout → TUI render) ≤50ms.

**Developer experience:**
- A new adapter (e.g. Cursor in v1.1) can be added in ≤500 LOC and ≤2 weeks for one engineer, measured by the Cursor adapter PR.
- ≥80% of adapter logic is shared (event normalization, sandbox, process lifecycle); per-harness code is wire-format + capability mapping only.

**Output quality (qualitative):**
- Mark's two-harness side-by-side benchmark (JTBD 3) is achievable end-to-end without scripting.
- ACP permission prompts surface in TUI with same latency feel as model-backed prompts.

---

## 9. Open questions

For engineering and arch review to decide.

**OQ-1.** Process pooling for ACP adapters: keep alive across orchestrator turns (current proposal, §7.4) or spawn fresh per turn? Trade-off: pooling preserves session state and saves 1-2s startup, but holds a child process and memory. **Proposed answer:** pool with 10-min idle timeout, configurable. Needs arch sign-off.

**OQ-2.** Cancellation propagation when an orchestrator cancels one subagent in a parallel fan-out: cancel only that one, or fail-fast and cancel siblings? **Proposed answer:** cancel-one. Siblings continue. Orchestrator decides whether to wait or abandon.

**OQ-3.** `HarnessRaw` event contents: full raw frame, or just the unmapped fields? **Proposed answer:** full raw frame as bytes. Smaller surface area for adapter bugs.

**OQ-4.** ACP `fs/write_text_file` with `auto_allow` policy: do we still emit a `PermissionResolved{decision: allow}` event for observability, or short-circuit silently? **Proposed answer:** emit it. TUI and audit logs need the record.

**OQ-5.** Codex's lack of text deltas (FR-16) -- do we want a synthetic delta stream (chunked by sentence) to make the TUI feel uniform, or stay faithful to the harness? **Proposed answer:** stay faithful. Document the gap. Don't fake streaming.

**OQ-6.** Where does the adapter registry live? `pkg/runtime/harness/` (alongside delegation) or a new `pkg/harness/`? **Proposed answer:** `pkg/harness/` with one subpackage per adapter, registered into the runtime. Cleaner separation.

**OQ-7.** Should `HasHarness()` agents be allowed to be the team orchestrator? Non-goal in v1 (§2), but the gating: hard reject at config validation, or runtime error? **Proposed answer:** hard reject at validation. Cleaner error.

**OQ-8.** Per-harness usage/cost surfacing: do we attach a `usage` field to `RunFinished` and let the TUI render it, or omit entirely in v1? **Proposed answer:** attach raw usage (whatever the harness reports), no aggregation. Aggregation is v1.1+.

**OQ-9.** Multi-turn transcript budget for Codex/OpenCode (FR-19): 50% of context window is the proposed default -- is that right, or should it be a per-harness tuned value? Needs measurement during impl.

---

## 10. Out of scope (v1)

Explicit list. Each is deferred with a reason.

| Item | Reason | Target |
|---|---|---|
| Cursor adapter | NDJSON schema not stable; high risk of churn | v1.1 if schema stabilizes |
| OpenCode SSE transport | Needed for per-call system prompts; HTTP/SSE adds infra complexity | v1.1 |
| Harness-as-orchestrator | Adds a recursion/protocol problem; not in primary user need | v2 |
| Sub-harness delegation (harness → harness) | Same recursion problem | v2 |
| Custom tool injection into self-contained harnesses | Each harness has different injection mechanisms; large per-harness surface | TBD |
| Unified cost/usage aggregation across harnesses | Each harness reports usage differently; aggregation needs schema work | v1.1 |
| Harness binary checksum verification | Defer to OS package manager / user trust | TBD |
| AG-UI wire format compatibility | Adopting wire format adds a serialization layer with no current consumer | When a real AG-UI consumer exists |
| ACP "shared session" between multiple subagents | No clear user need yet | TBD |
| Streaming token counts during a run | Most harnesses report only at end | When harnesses support it |
| Remote/network harnesses (non-stdio) | All v1 harnesses are local stdio; remote adds auth/transport surface | v2 |
| Recording/replay of harness sessions for tests | Useful but not blocking | v1.1 (orthogonal infra) |

---

## Appendix A: Interface sketch (informative, not normative)

```go
package harness

type SubSessionRequest struct {
    Task          string
    SystemPrompt  string
    SessionToken  string                // empty on first turn
    WorkingDir    string
    Env           map[string]string
    Events        chan<- Event
    ToolExecutor  ToolExecutor          // ACP only: fs/terminal
    Permission    PermissionGate        // ACP only: prompt routing
    HarnessConfig map[string]any        // adapter-specific
}

type Event struct {
    Kind        EventKind
    Timestamp   time.Time
    MessageID   string                  // for *Message* and ToolCall* events
    Text        string                  // for *Delta and *End events
    ToolName    string                  // for ToolCall*
    ToolArgs    json.RawMessage         // for ToolCall*
    ToolResult  json.RawMessage         // for ToolCallFinished
    Permission  *PermissionDetail       // for Permission*
    Raw         json.RawMessage         // for HarnessRaw
    Error       *ErrorDetail            // for RunError
    Usage       *UsageDetail            // for RunFinished
}
```

This is illustrative. Final shapes are in the arch spec.

---

## Appendix B: Test plan summary

- **Unit:** each adapter's event-mapping function against recorded harness output fixtures.
- **Integration:** real binary per adapter, in CI behind a build tag (CI runners have binaries pre-installed).
- **Sandbox:** hostile-path tests for FR-29-FR-31 (symlink traversal, `..`, absolute paths outside root).
- **Lifecycle:** goleak + process-orphan tests for FR-10, NFR-5, NFR-6.
- **Multi-turn:** assert session-token round-trip for native harnesses; assert prompt prepending + budget for simulated harnesses.
- **Concurrency:** N parallel subagents (N=8) under load, assert no event interleaving across sessions.
