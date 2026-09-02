# CLI reference

Verified against the built binary's `--help` output. `docker-agent` and
`docker agent` are interchangeable throughout — the plugin is the same binary.

## Global flags (every command)

| Flag | Meaning |
| --- | --- |
| `--cache-dir <dir>` | Override the cache directory (default varies by OS, e.g. `~/Library/Caches/cagent` on macOS). |
| `--config-dir <dir>` | Override the config directory (default `~/.config/cagent`). |
| `--data-dir <dir>` | Override the data directory (default `~/.cagent`). |
| `-d, --debug` | Enable debug logging. |
| `--log-file <path>` | Debug log location (default `~/.cagent/cagent.debug.log`; only used with `--debug`). |
| `-o, --otel` | Enable OpenTelemetry tracing. |

Commands that load an agent config also accept these (omitted from the
per-command tables below to avoid repetition): `--code-mode-tools`,
`--env-from-file <path>` (repeatable), `--flavor <name>` (repeatable),
`--hook-pre-tool-use`, `--hook-post-tool-use`, `--hook-session-start`,
`--hook-session-end`, `--hook-on-user-input`, `--hook-stop <cmd>` (all
repeatable), `--mcp-oauth-redirect-uri <uri>`, `--models-gateway <addr>`,
`--working-dir <path>`.

## Commands that do not exist

Don't guess these — they look plausible but aren't there:

- **No top-level `exec`.** Headless execution is `run --exec`.
- **No top-level `push`/`pull`.** They are `share push`/`share pull`.
- **No top-level `chat`.** OpenAI-compatible serving is `serve chat`.
- **No `catalog`/`feedback`/`readme` command.**
- **`toolsets` lists built-in toolset *types*** (filesystem, shell, mcp, …),
  not a browsable list of MCP Catalog servers. There is no CLI command to
  browse the Docker MCP Catalog from this repo's binary.

## `run` — run an agent

```
docker-agent run [<agent-file>|<registry-ref>] [message]... [flags]
```

Bare `run` (no `--exec`) launches the **interactive TUI** — never invoke it
this way non-interactively; it will hang waiting for a terminal. Use
`--exec` for anything scripted or agent-driven:

```
docker-agent run ./agent.yaml --exec "do the thing" --safety restricted
```

Selected flags (grouped by purpose — see `--help` for the complete list of
~50):

**Execution mode**

| Flag | Meaning |
| --- | --- |
| `--exec` | Execute without the TUI. Required for any non-interactive use. |
| `-a, --agent <name>` | Which agent to run in a multi-agent file (defaults to the first/`root`). |
| `[message]...` | One or more prompts, sent in sequence. `-` reads a message from stdin. |
| `--json` | JSON output (only meaningful with `--exec`). |
| `--dry-run` | Initialize the agent without executing anything. |
| `--model [agent=]provider/model` | Override a model without editing the file (repeatable, e.g. `--model reviewer=openai/gpt-4o`). |

**Safety / approval**

| Flag | Meaning |
| --- | --- |
| `--safety strict\|balanced\|restricted\|autonomous` | `strict` asks for everything; `balanced` auto-approves safe calls; **`restricted` auto-approves safe calls and denies the rest — the right default for unattended runs**; `autonomous` approves everything. |
| `--yolo` | Shorthand for `--safety autonomous`. Avoid unless the user explicitly wants full auto-approval. |

**Isolation (mutually exclusive with each other — see the matrix below)**

| Flag | Meaning |
| --- | --- |
| `--remote <addr>` | Use a remote runtime at `<addr>`. |
| `--sandbox` | Run inside a Docker sandbox (requires Docker Desktop sandbox support). `--sbx` (default true) prefers the `sbx` CLI backend when available; `--sbx=false` forces the Docker sandbox path. `--template <img>` overrides the sandbox template image. |
| `-w, --worktree[=name]` | Run in a fresh git worktree of the working directory. `--worktree-base <ref>` branches it from a specific ref (fetches remote-tracking refs first). |
| `--worktree-pr <number\|url>` | Run in a worktree checked out on an existing GitHub PR (needs `gh`). |

**Session / recording**

| Flag | Meaning |
| --- | --- |
| `--session <id\|offset>` | Resume a session by ID or relative offset (`-1` = last). An unknown explicit ID is created fresh. |
| `-s, --session-db <path>` | Session database path (default `<data-dir>/session.db`). |
| `--session-read-only` | Open the session read-only (view history, send nothing new). |
| `--record[=path]` | Record AI API interactions to a cassette and generate a TUI e2e test. |
| `--fake <path>` | Replay AI responses from a cassette (testing). |
| `--fake-stream[=ms]` | Simulate streaming delay when replaying with `--fake` (default 15ms). |

**TUI display (ignored under `--exec`)**

`--app-name`, `--lean`, `--sidebar[=false]`, `--theme <name>`,
`--disable-commands <list>`, `--hide-tool-calls`, `--hide-tool-results`,
`--agent-picker[=list]` (full-screen agent picker before launch).

**Other**

`--attach <path>` (attach an image file to the message), `--prompt-file
<path>` (repeatable, append file contents to the prompt), `--on-event
<type>=<cmd>` (repeatable shell hook), `--no-kit` (don't stage a
docker-agent kit — skills, prompt files — when running in a sandbox).

### `run`'s mutually-exclusive flag pairs (verified)

Passing two of these together is a hard cobra error, not a silent merge:

- `--remote` excludes: `--sandbox`, `--session-db`, `--session`, `--record`,
  `--fake`, `--worktree`, `--worktree-base`, `--worktree-pr`.
- `--sandbox` excludes: `--worktree`, `--worktree-base`, `--worktree-pr`.
- `--worktree` excludes: `--worktree-pr`.
- `--worktree-base` excludes: `--worktree-pr`.
- `--fake` excludes: `--record`.

### Hidden flags on `run` (deliberately undocumented — never use or mention)

`--exit-after-response`, `--listen`, `--session-workingdir-root`,
`--cpuprofile`, `--memprofile`, `--force-tui`, `--tour`.

## `new` — interactive agent builder

```
docker-agent new [description] [flags]
```

**Interactive — asks follow-up questions in a chat loop.** Do not invoke
this to script config generation; write the YAML directly instead (see
`references/config.md`). Even with a `description` argument, it still runs
an interactive conversation to refine details before writing the file.
Useful flags if a human is actually driving it: `--model <provider/model>`,
`--max-iterations <n>`.

## `eval` — run evaluations

```
docker-agent eval <agent-file>|<registry-ref> [<eval-dir>|./evals] [flags]
```

| Flag | Meaning |
| --- | --- |
| `-c, --concurrency <n>` | Concurrent evaluation runs (default 4). |
| `-e, --env KEY[=VALUE]` | Env vars passed to the container (repeatable). |
| `--only <pattern>` | Only run evals whose file name matches (repeatable). |
| `--output <dir>` | Results/logs directory (default `<eval-dir>/results`). |
| `--repeat <n>` | Repeat each evaluation N times (useful for baselines). |
| `--baseline <run.json>` | Compare against a previous run; exits non-zero on regression. |
| `--regression-tolerance <0-1>` | How far the aggregate quality rate may fall before `--baseline` reports a regression. |
| `--judge-model <provider/model>` | Model used for relevance judging (default `anthropic/claude-opus-5`). |
| `--keep-containers` | Don't `--rm` containers after the run. |
| `--container-runtime <bin>` | Container runtime executable (default `docker`). |

## `share push` / `share pull` — OCI registry

```
docker-agent share push <agent-file> <registry-ref>
docker-agent share pull <registry-ref> [--force]
```

`--force` on `pull` overwrites an existing local copy.

## `serve` — expose an agent as a server

All `serve` subcommands take `<agent-file>|<registry-ref>`, `-a/--agent
<name>` (which agent to expose), and `-l/--listen <addr>`.

| Subcommand | Protocol | Default listen addr | Notable flags |
| --- | --- | --- | --- |
| `serve mcp` | MCP, stdio by default | `127.0.0.1:8081` | `--http` (streaming HTTP instead of stdio), `--attach[=latest]` (attach to a running TUI session instead of a file), `--auth-token`, `--tool-name` (override the exposed tool name), `--mcp-keepalive`, `--insecure-no-auth` (allow non-loopback HTTP without auth), `--safety` (only with `--http`) |
| `serve a2a` | Agent-to-Agent | `127.0.0.1:8082` | `--auth-token`, `--cors-origin`, `--insecure-no-auth`, `--safety`, `-s/--session-db` |
| `serve acp` | Agent Client Protocol | (stdio) | `-s/--session-db` |
| `serve chat` | OpenAI-compatible `/v1/chat/completions`, `/v1/models` | `127.0.0.1:8083` | `--api-key`, `--api-key-env`, `--conversations-max`, `--conversation-ttl`, `--cors-origin`, `--insecure-no-auth`, `--max-idle-runtimes`, `--max-request-size`, `--request-timeout`, `--safety` |
| `serve api` <agent-file\|agents-dir> | docker-agent's own HTTP API | `127.0.0.1:8080` | `--auth-token`, `--max-request-size`, `--pull-interval` (auto-pull an OCI ref every N minutes), `-s/--session-db`, `--session-workingdir-root` (restrict session working dirs; recommended multi-user), `--record`/`--fake` (mutually exclusive) |

`serve api --pprof-addr` is a hidden flag — never mention or use it.

## Diagnose commands

`doctor [<agent-file>|<registry-ref>] [--json]` — reports which providers
have credentials and where from, DMR reachability, which model `auto` would
pick, and (with a file) required env vars and their status. Never prints
secret values. Exits non-zero if something would prevent the agent running.

`models [list] [-a/--all] [-p/--provider <name>] [--format table|json]` —
lists models usable with `run --model`/`new --model`. Default scope is
providers you have credentials for; `--all` includes every provider.

`toolsets [--format table|json]` — lists built-in toolset *types* (not MCP
catalog servers).

## `alias` — named shortcuts for agent refs

```
docker-agent alias add <name> <agent-path-or-ref> [--yolo] [--safety <mode>] [--model <ref>] [--hide-tool-results] [--sandbox]
docker-agent alias list [--json]
docker-agent alias remove <name>
```

An alias bakes in `run` options; using the alias with `run` applies them as
defaults (explicit `run` flags still win).

## `plans` — shared and session plans

Two kinds: **shared plans** (named, versioned, fully managed here) and
**session plans** (one per session, read-only from the CLI — get/list/export
only; use `--session <id>` and `--scope session`).

```
docker-agent plans list [--session <id>]
docker-agent plans get <name>|--session <id> [--json]
docker-agent plans create <name> --file <path|-> [--title ...] [--status ...] [--author ...]
docker-agent plans update <name> --file <path|-> (--expected-version <n>|--force)
docker-agent plans status <name> <status> (--expected-version <n>|--force)
docker-agent plans export <name>|--session <id> --output <path> [--force]
docker-agent plans delete <name> (--expected-version <n>|--force)
```

Mutation commands never prompt: **exactly one** of `--expected-version <n>`
(optimistic lock; fails with exit code 3 if the plan changed) or `--force`
(write unconditionally) is required on `update`/`status`/`delete`. Every
subcommand supports `--json` for stable machine-readable output (failures
then print a single JSON object on stderr).

## `sandbox` — persistent network allowlist

```
docker-agent sandbox allow <host[:port]> [<host>...]
docker-agent sandbox deny <host>
docker-agent sandbox list
```

Fixes "Blocked by network policy" 403s in `--sandbox` runs for a host the
auto-installer can't infer. Persisted in `~/.config/cagent/config.yaml`.

## `sessions diff` — compare two recorded sessions

```
docker-agent sessions diff <session-a> <session-b> [--json] [--fail-on-divergence] [-s/--session-db <path>]
```

Compares the *sequence of tool calls*, not assistant prose (which is
nondeterministic across identical runs). Stops reporting at the first
divergence.

## `debug` — introspection tools

```
docker-agent debug config <agent-file>|<registry-ref>      # canonical, fully-defaulted config; fails loud on schema errors
docker-agent debug toolsets <agent-file>|<registry-ref>    # resolved toolsets for an agent
docker-agent debug skills <agent-file>|<registry-ref>       # resolved skills for an agent
docker-agent debug title <agent-file>|<registry-ref> <question> [--model ...]  # generate a session title
docker-agent debug auth [--json]                             # Docker auth info, never prints secrets
docker-agent debug oauth list|login|remove                   # stored MCP OAuth tokens
```

`debug config` is the primary validation tool — run it after every config
edit.

## Interactive-only commands — never invoke non-interactively

These block on a TTY or are full-screen UIs with no headless mode. Don't
run them from a script or on an agent's behalf; if the goal is config
authoring, write YAML directly instead.

- `docker-agent setup` — interactive model setup wizard (4 paths: built-in
  cloud provider, local DMR model, custom OpenAI-compatible endpoint, or the
  Claude Code harness).
- `docker-agent getting-started` (alias `tour`) — ~2 minute interactive
  product tour.
- `docker-agent board` — Kanban TUI orchestrating agents across tmux
  sessions and git worktrees.
- `docker-agent new` — see above.
- bare `docker-agent run` (no `--exec`) — see above.

## Other top-level commands

`docker-agent version` — version and commit hash.
`docker-agent completion bash|zsh|fish|powershell` — shell completion
scripts.
`docker-agent __askpass` — internal `SUDO_ASKPASS` helper for the `shell`
toolset's `sudo_askpass` opt-in (any shell toolset, not sandbox-exclusive):
bridges a `sudo` password prompt back to the host UI over a private unix
socket. Never call directly.
