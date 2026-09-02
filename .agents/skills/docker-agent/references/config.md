# Config schema reference

Grounded in `agent-schema.json` (the authoritative, hand-maintained source —
when this file and the schema disagree, the schema wins) and
`docs/configuration/overview/index.md`. Add
`# yaml-language-server: $schema=https://raw.githubusercontent.com/docker/docker-agent/main/agent-schema.json`
as the first line of a config for editor validation.

Validate any config with `docker-agent debug config <file>` — do this after
every edit, not just at the end.

## Minimal config

```yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: A helpful AI assistant
    instruction: |
      You are a knowledgeable assistant. Be helpful, accurate, and concise.
```

`agents` is the only required top-level key. The entrypoint agent is named
`root` by convention; if no `root` exists, the first agent declared is used.
`model` can be a bare `provider/model` string (as above) or the name of an
entry under the top-level `models:` map (use a named model to share
`max_tokens`/`temperature`/etc. across agents, or to reuse the same model
under different override names).

## Top-level keys

| Key | Purpose |
| --- | --- |
| `version` | Config schema version, `"0"`–`"15"`. **Omit it to target the current latest** (`"15"` today) — don't pin an old version "to be safe". |
| `agents` | Map of agent definitions. At least one required. |
| `models` | Map of named model configs, referenced from `agents.*.model`. |
| `providers` | Map of custom provider defaults (`base_url`, `token_key`, `api_type`) that models can inherit. |
| `toolsets` | Map of reusable, named toolset definitions; an agent pulls one in via `use_toolsets`. |
| `mcps` | Map of reusable MCP server definitions; agents reference by name from an `mcp` toolset's `ref`. |
| `rag` | Map of reusable RAG source definitions. |
| `skills` | Map of reusable, named skill *groups*; an agent pulls one in via `use_skills`. (Distinct from the per-agent `skills:` field, which enables discovery/inline skills — see below.) |
| `commands` | Map of reusable, named `/command` groups; an agent pulls one in via `use_commands`. |
| `permissions` | Tool-approval rules (allow/deny lists) — see `docs/configuration/permissions/index.md`. |
| `budget` / `budgets` | Run-wide spend ceiling / named, shared spend pots an agent opts into. See `examples/budget.yaml`. |
| `runtime` | Execution-time defaults the config author wants (e.g. `sandbox: true`, `network_allowlist`, `safety`) — CLI flags and user config always override these. |
| `flavors` | Named YAML patches (JSON Merge Patch semantics) enabled at run time with `--flavor <name>`. `key+:` appends to an array, `key-:` removes entries. |
| `metadata` | Free-form metadata about the config itself (for distribution). |

## Agent fields (`agents.<name>`)

Grouped by concern; every field is optional except `model`.

**Identity & behavior**
`model`, `description`, `instruction` (or `instruction_file` — mutually
exclusive with `instruction`; accepts a string or list of local paths,
concatenated in order), `welcome_message`, `harness` (run this agent via an
external coding harness instead of a docker-agent provider — see
`docs/features/harnesses/index.md`), `add_date`, `add_environment_info`,
`add_description_parameter`, `add_prompt_files`.

**Tools**
`toolsets` (inline list — see next section), `use_toolsets` (names from the
top-level `toolsets:` map, appended after inline ones), `code_mode_tools`
(expose one JS-calling meta-tool instead of many), `readonly` (filter every
toolset down to read-only-annotated tools).

**Multi-agent**
`sub_agents` (delegation targets: names in this file, an OCI ref, a URL, or
`name:reference`), `handoffs` (same reference forms; hands off the whole
conversation instead of delegating a sub-task), `force_handoff` (name of an
agent that unconditionally receives every final response — deterministic
pipelines; must not cycle or self-reference).

**Skills & commands**
`skills` (bool or list — `true` loads all discovered local skills, a list
mixes sources (`"local"`, an `http(s)://` URL), skill-name filters, and
inline skill objects — see `examples/skills_inline.yaml` and
`examples/skills_filter.yaml`), `use_skills` (named groups from top-level
`skills:`), `commands` (named `/command` prompts), `use_commands`.

**Safety, budget, limits**
`safety` (default mode for new sessions on this agent — `strict` /
`balanced` / `restricted` / `autonomous`; `restricted` is the fail-closed
default recipe for unattended runs), `redact_secrets` (on by default; scrubs
detected secrets from tool args/LLM messages/tool output), `budgets` (names
from top-level `budgets:`), `max_iterations`, `max_consecutive_tool_calls`
(loop guard, default 5), `max_old_tool_call_tokens`, `max_tool_result_tokens`,
`num_history_items`.

**Compaction & structured output**
`session_compaction` (on by default), `compaction_threshold` (fraction of
context window, default 0.9), `compaction_model`, `structured_output`
(constrain responses to a JSON schema; native on OpenAI/Google, prompt-based
on Anthropic).

**Other**
`fallback` (automatic model failover), `hooks` (lifecycle shell commands —
see `docs/configuration/hooks/index.md`), `cache` (replay identical prior
answers).

## Toolsets (`toolsets:` list, inline or under top-level `toolsets:`)

Every toolset entry needs `type:`. Run `docker-agent toolsets` for the
canonical list with one-line summaries; the full enum (verified against
`agent-schema.json`) is:

`mcp`, `mcp_catalog`, `script`, `think`, `memory`, `filesystem`, `file`,
`shell`, `background_jobs`, `tasks`, `plan`, `session_plan`,
`session_context`, `todo`, `fetch`, `api`, `a2a`, `lsp`, `user_prompt`,
`openapi`, `open_url`, `model_picker`, `background_agents`, `scheduler`,
`rag`, `git`, `webhook`.

Common cross-type fields: `instruction` (override/enrich built-in guidance —
use `{ORIGINAL_INSTRUCTIONS}` to enrich rather than replace), `readonly`,
`model` (route this toolset's result-processing turn to a different model),
`defer` (lazy-load tools on demand via `search_tool`/`add_tool`), `toon`
(regex list of tool names to compact-encode).

**`mcp` — three mutually-distinct forms:**

```yaml
# 1. ref — Docker MCP Catalog server or a named top-level `mcps:` entry
- type: mcp
  ref: docker:context7          # or a name from mcps: (examples/mcp-definitions.yaml)

# 2. command/args — arbitrary local MCP server subprocess
- type: mcp
  command: docker
  args: ["mcp", "gateway", "run"]

# 3. remote — HTTP(S) MCP server, with explicit OAuth if it doesn't support
#    Dynamic Client Registration
- type: mcp
  remote:
    url: "https://mcp.example.com/mcp"
    transport_type: streamable
    oauth: {clientId: ..., clientSecret: ..., callbackPort: 8080, scopes: [read, write]}
```

All three forms accept `env`, `tools` (allow-list), `working_dir`, `version`
(pin the auto-installed binary: `owner/repo` or `owner/repo@ver`; `"false"`
disables auto-install).

**Other type-specific fields worth knowing:**
`filesystem`/`file`: `allow_list`/`deny_list` (deny wins on overlap),
`ignore_vcs` (default true), `post_edit`. `fetch`/`api`/`openapi`:
`allowed_domains`/`blocked_domains` (mutually exclusive), `allow_private_ips`,
`timeout` (default 30s), `headers` (supports `${env.VAR}`). `shell`:
`sudo_askpass`. `mcp_catalog`: `allowed_servers`/`blocked_servers`. `memory`/
`tasks`: `path`. `script`: `shell:` map of named commands (`cmd`,
`description`, `args`, `required`, `working_dir`, `env`). `rag`: `rag_config`
(or reference a top-level `rag:` entry). `a2a`: `url`, `name`. `lsp`:
`file_types`. `model_picker`: `models` (allow-list).

## Models (`models.<name>` or inline `provider/model`)

```yaml
models:
  my-model:
    provider: anthropic       # openai, anthropic, google, dmr, or a custom provider name
    model: claude-sonnet-4-5
    max_tokens: 64000          # output budget, NOT context window
    temperature: 0.7
```

Notable fields: `first_available` (priority-ordered fallback candidates;
mutually exclusive with other model fields — good for "cloud, else local
DMR"), `title_model`/`compaction_model` (delegate specific calls to a
cheaper model — note the agent-level `fallback` field, not a model field, is
what handles automatic failover/retry on error; see Agent fields above),
`thinking_budget` (provider-specific reasoning effort — see the schema for
the full per-provider value grammar), `cost` (declare USD pricing for
uncatalogued models so budgets can enforce `max_cost`), `capabilities`
(override auto-detected image/PDF support), `base_url` + `token_key` (point
at a custom OpenAI-compatible endpoint), `bypass_models_gateway`, `routing`
(turn this model into a request router). `provider_opts` carries
provider-specific knobs (`top_k`, `seed`, Anthropic
`interleaved_thinking`/`thinking_display`/`fallbacks`, Google
`google_search`/`code_execution`, etc.) — see the schema for the exhaustive,
frequently-updated list rather than duplicating it here.

## Inline skills

A `skills:` list item can be an object instead of a string — see
`examples/skills_inline.yaml`. Required: `name`, `description`,
`instructions`. Optional: `context: fork` (run as an isolated sub-agent),
`model` (override for fork-mode), `allowed_tools`, `toolsets` (extra
toolsets exposed to a fork-mode skill).

## `${env.VAR}` expansion

`${env.VAR}` (canonical) works in most string fields (headers, env values,
base_url, etc.); `$VAR`, `${VAR}`, and `~` are accepted aliases specifically
on path/env-shaped fields. See
`docs/configuration/overview/index.md#variable-expansion-in-config-fields`
for the full per-field-type table — it differs by field, so check before
assuming a form works everywhere.

## Versioning

`version:` is one of `"0"`–`"15"`; omitting it targets latest (verified:
`docker-agent debug config` on a version-less file fills in the current
latest). Config packages `pkg/config/v0..vN` are frozen; new schema features
land only in `pkg/config/latest`, mirrored into `agent-schema.json`'s
`version` enum on each freeze (see `.agents/skills/bump-config-version/` in
this repo if the version itself needs bumping — that's a different task from
this skill).

## Reusable top-level sections

Define once, reference by name to avoid duplicating config across agents —
see `examples/mcp-definitions.yaml` for the pattern applied to `mcps:`; the
same shape applies to `toolsets:` (via `use_toolsets`), `skills:` (via
`use_skills`), and `commands:` (via `use_commands`). Inline definitions on an
agent always take precedence over a same-named reusable one on conflict.
