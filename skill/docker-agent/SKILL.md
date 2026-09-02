---
name: docker-agent
description: Delegate to or operate the docker-agent (cagent) CLI as an external tool from any project or harness: run/exec a sub-agent non-interactively, author or validate an agent YAML config against agent-schema.json, or expose/consume agents via MCP/A2A/chat/API. Covers every subcommand: run, new, eval, share, serve, doctor, alias, sandbox, sessions.
---

# docker-agent

`docker-agent` is a standalone CLI (and Docker CLI plugin, invoked as
`docker agent` — same commands, either prefix) that runs YAML-defined AI
agents. This skill is for using it **as an external tool from any project**
— you don't need to be working inside docker-agent's own source code for any
of this to apply. Two jobs: **delegating work to a docker-agent agent** and
**authoring/operating** an agent config. Exhaustive detail lives in
`references/` — read this file first, then follow a link when you need more.

Check it's available: `docker-agent version` or `docker agent version`. If
neither resolves, point the user at
<https://docs.docker.com/ai/docker-agent/getting-started/installation/>.

## Start here

| You want to...                                    | Do this                                                                          |
| --------------------------------------------------- | --------------------------------------------------------------------------------- |
| Delegate a task to another agent (you are the orchestrator) | See [Delegating to another agent](#delegating-to-another-agent) below. |
| Write or edit an agent/team YAML                  | Read `references/config.md`, copy a pattern from `references/examples.md`, then validate (below). Do **not** run `docker-agent new` for this — it's an interactive builder, not a way to script a config. |
| Check a config is valid                           | `docker-agent debug config <file>` — fails loudly with the offending field on any schema error. |
| Run an agent and see/send messages yourself       | `docker-agent run <file>` (interactive TUI; only when you have a real terminal). |
| Run an agent unattended (scripts, CI, another agent driving this one) | `docker-agent run <file> --exec "<message>" --safety restricted`. See [Non-negotiable rules](#non-negotiable-rules). |
| Diagnose why an agent won't start                 | `docker-agent doctor <file>` — reports missing credentials/model issues without printing secrets. |
| Look up a CLI command or flag                     | `references/cli.md`. |
| Share a config via an OCI registry                | `docker-agent share push <file> <ref>` / `docker-agent share pull <ref>`. |
| Expose an agent to another tool (MCP/A2A/ACP/OpenAI-chat/HTTP API) | `docker-agent serve mcp\|a2a\|acp\|chat\|api <file>`. |
| Evaluate agent quality / check for regressions    | `docker-agent eval <file> [eval-dir]`, optionally `--baseline <run.json>`. |
| See a full worked example                         | `references/examples.md` — self-contained, copyable YAML for every pattern below. |

## Delegating to another agent

The most common reason to reach for `docker-agent` from inside another
agent's session: you (an orchestrator, reviewer, or any agent) want a
*different*, independently-configured agent to do a sub-task. Three ways,
pick by how long-lived the relationship is:

1. **One-shot delegation (default choice).** Shell out and capture the
   result:
   ```console
   $ docker-agent run ./reviewer.yaml --exec "review this diff: <diff>" --safety restricted --json
   ```
   `--exec` is required (no TTY); `--json` gives you a machine-parseable
   result; `--safety restricted` auto-approves safe tool calls and denies
   the rest without prompting — the right default when nothing is watching
   the terminal. Use `--model` to override the sub-agent's model without
   editing its file, and `--working-dir` to point it at a specific
   directory. See `references/examples.md` for a full worked example.
2. **In-config delegation.** If you're authoring the config yourself (not
   just invoking someone else's), declare the relationship directly: the
   orchestrator agent's `sub_agents:` list names delegation targets (which
   can themselves be OCI refs, e.g. `myorg/reviewer:latest`, not just local
   agents), and the model decides when to delegate. `handoffs:` is similar
   but transfers the whole conversation instead of a sub-task. See
   `references/config.md`.
3. **Long-lived, tool-shaped delegation.** If the target agent should look
   like a registered tool to your harness (rather than something you shell
   out to per task), expose it once with `docker-agent serve mcp ./agent.yaml`
   (or `serve a2a`) and have your harness call it like any other MCP/A2A
   tool instead of re-invoking the CLI each time.

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
   `--pprof-addr`; the `__askpass` command (internal).
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
- `references/examples.md` — end-to-end worked flows as self-contained,
  copyable YAML — including a full agent-delegation example.
