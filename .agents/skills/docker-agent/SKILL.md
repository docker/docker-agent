---
name: docker-agent
description: docker-agent (cagent) CLI and agent YAML config. Use to author or fix an agent or multi-agent config (models, toolsets, MCP servers, A2A, skills, sub-agents) against agent-schema.json, or to run, debug, evaluate, share via OCI, or serve (MCP/A2A/ACP/chat/API) a docker-agent agent. Covers every subcommand: run, new, eval, share, serve, doctor, alias, sandbox, sessions. Not for unrelated Go dev in this repo.
---

# docker-agent

`docker-agent` builds and runs YAML-defined AI agents. The binary is
`docker-agent`; as a Docker CLI plugin it is also invoked as `docker agent`
(same commands, either prefix). This skill covers two jobs: **authoring** an
agent config and **operating** the CLI. Exhaustive detail lives in
`references/` — read this file first, then follow a link when you need more.

## Start here

| You want to...                                   | Do this                                                                          |
| ------------------------------------------------- | --------------------------------------------------------------------------------- |
| Write or edit an agent/team YAML                  | Read `references/config.md`, copy a pattern from `references/examples.md`, then validate (below). Do **not** run `docker-agent new` for this — it's an interactive builder, not a way to script a config. |
| Check a config is valid                           | `docker-agent debug config <file>` — fails loudly with the offending field on any schema error. |
| Run an agent and see/send messages yourself       | `docker-agent run <file>` (interactive TUI; only when you have a real terminal). |
| Run an agent unattended (scripts, CI, another agent driving this one) | `docker-agent run <file> --exec "<message>" --safety restricted`. See [Non-negotiable rules](#non-negotiable-rules). |
| Diagnose why an agent won't start                 | `docker-agent doctor <file>` — reports missing credentials/model issues without printing secrets. |
| Look up a CLI command or flag                     | `references/cli.md`. |
| Share a config via an OCI registry                | `docker-agent share push <file> <ref>` / `docker-agent share pull <ref>`. |
| Expose an agent to another tool (MCP/A2A/ACP/OpenAI-chat/HTTP API) | `docker-agent serve mcp\|a2a\|acp\|chat\|api <file>`. |
| Evaluate agent quality / check for regressions    | `docker-agent eval <file> [eval-dir]`, optionally `--baseline <run.json>`. |
| See a full worked example                         | `references/examples.md` and the ~170 real configs under `examples/` in this repo. |

## Non-negotiable rules

1. **Never run these non-interactively — they block forever on a TTY, or launch
   a full-screen UI with no headless mode:** `docker-agent new` (interactive
   agent builder), `docker-agent setup` (interactive model wizard),
   `docker-agent getting-started` (alias `tour`), `docker-agent board`, and bare
   `docker-agent run <file>` with no `--exec` (launches the TUI). If you must
   author a config programmatically, write the YAML by hand from
   `references/config.md` — don't shell out to `new`.
2. **There is no `exec` subcommand.** Headless execution is `run --exec`, not
   `docker-agent exec`. There is also no top-level `push`/`pull`/`chat`/`catalog`
   command — those are `share push`/`share pull`/`serve chat`, and there is no
   Docker MCP catalog browser command in this CLI (`toolsets` lists *built-in*
   toolset types, not MCP catalog servers).
3. **For unattended runs, prefer `--safety restricted`** (auto-approves safe
   tool calls, denies the rest, never prompts) over `--yolo` /
   `--safety autonomous` (approves *everything*) unless the user explicitly
   asked for full auto-approval.
4. **Respect `run`'s mutually-exclusive flag groups** — passing two conflicting
   flags is a hard error, not a merge: `--remote` excludes `--sandbox`,
   `--session`, `--session-db`, `--record`, `--fake`, `--worktree`,
   `--worktree-base`, `--worktree-pr`; `--sandbox` excludes `--worktree`,
   `--worktree-base`, `--worktree-pr`; `--worktree` excludes `--worktree-pr`;
   `--worktree-base` excludes `--worktree-pr`; `--fake` excludes `--record`.
   Full flag reference: `references/cli.md`.
5. **Never invoke or document these — they're hidden on purpose:**
   `run`'s `--exit-after-response`, `--listen`, `--session-workingdir-root`,
   `--cpuprofile`, `--memprofile`, `--force-tui`, `--tour`; `serve api`'s
   `--pprof-addr`; the `__askpass` command (internal, used by the sandbox).
6. **Validate before claiming success.** After writing or editing a config,
   run `docker-agent debug config <file>` and read the error — don't assume
   syntactically-valid YAML is schema-valid.
7. **Omitting `version:` targets the current latest schema** (today `"15"`),
   not an old pinned version — don't add `version: "0"` "to be safe".
8. **Agent references are polymorphic**: `<agent-file>` in any command below
   accepts a local YAML path, an OCI registry ref (`myorg/agent:tag`), or a
   registered alias (`docker-agent alias list`) — try the user's input as
   given rather than assuming it must be a file.

## Task recipes

**Author a config from scratch.** Skeleton: a top-level `agents:` map with at
least one entry named `root` (the entrypoint unless the config says
otherwise), each agent needing `model` and `instruction`. Add `toolsets:` for
capabilities. See `references/config.md` for the full field set and
`references/examples.md` for copyable patterns.

**Validate.** `docker-agent debug config ./agent.yaml`. Prints the canonical,
fully-defaulted config on success; on failure, prints the exact bad field and
line.

**Run headlessly and capture output.**
`docker-agent run ./agent.yaml --exec "do the thing" --safety restricted`.
Add `--json` for machine-readable output, `--model provider/model` to
override the model without editing the file.

**Debug a startup failure.** `docker-agent doctor ./agent.yaml` — check which
env vars/credentials it says are missing before assuming a config bug.

**Add an MCP server or built-in toolset.** List built-ins with
`docker-agent toolsets`; see `references/config.md` for each type's fields and
the three MCP toolset forms (`ref`, `command`/`args`, `remote`).

**Build a multi-agent team.** A coordinator agent lists `sub_agents:` by
name; each sub-agent is its own entry under `agents:`. See
`references/examples.md`.

**Share a config.**
`docker-agent share push ./agent.yaml myorg/agent:tag` /
`docker-agent share pull myorg/agent:tag`.

**Expose an agent as a server.**
`docker-agent serve mcp ./agent.yaml` (MCP tool, stdio by default, `--http`
for streaming HTTP), `serve a2a`/`serve acp` (agent-to-agent / agent-client
protocols), `serve chat` (OpenAI-compatible `/v1/chat/completions`), `serve
api` (docker-agent's own HTTP API). All take `--listen <addr>`.

**Evaluate for regressions.**
`docker-agent eval ./agent.yaml ./evals --baseline ./evals/results/run.json`.

**Isolate a run's file changes.** `--worktree[=name]` runs in a fresh git
worktree; `--sandbox` runs inside a Docker sandbox. Mutually exclusive — pick
one (rule 4 above).

## Notes

- `references/cli.md` — every command and flag, grouped, with the verified
  mutual-exclusion matrix and the hidden/nonexistent-command lists in full.
- `references/config.md` — the full config schema: all top-level keys, every
  `AgentConfig` field, every toolset type, model config, skills, variable
  expansion, versioning.
- `references/examples.md` — end-to-end worked flows, each pointing at a real
  file under `examples/` in this repo.
- `docker-agent new`'s system prompt (`pkg/creator/instructions.txt`) covers
  similar config ground for an LLM builder session; if it and
  `references/config.md` ever disagree, `agent-schema.json` is the tiebreaker.
