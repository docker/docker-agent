# Worked examples

Every pattern below points at a real, runnable file under `examples/` in
this repo (170+ configs total — `examples/README.md` is the curated index).
Prefer pointing the user at the real file over retyping it; it's kept
schema-valid by the repo's own tests.

## 1. Minimal single agent

`examples/basic_agent.yaml` — the smallest valid config: one `root` agent,
a model, an instruction, no toolsets.

```console
$ docker-agent debug config examples/basic_agent.yaml   # validate
$ docker-agent run examples/basic_agent.yaml --exec "hello" --safety restricted
```

## 2. Multi-agent team with delegation

`examples/mcp-definitions.yaml` shows the clean shape: a `root` with
`sub_agents: [frontend, backend]`, each sub-agent its own `agents.<name>`
entry. (`examples/dev-team.yaml` shows a larger four-agent team with long,
narrative instructions — useful as an example of *style*, not a template to
copy verbatim.)

```yaml
agents:
  root:
    model: model
    description: Lead developer
    instruction: You are the lead developer. Coordinate the team.
    sub_agents: [frontend, backend]
    toolsets: [{type: filesystem}, {type: think}]
  frontend:
    model: model
    description: Frontend engineer
    instruction: You are a frontend engineer.
    toolsets: [{type: filesystem}, {type: shell}]
  backend:
    model: model
    description: Backend engineer
    instruction: You are a backend engineer.
    toolsets: [{type: filesystem}, {type: shell}]
```

## 3. Reusable MCP server definitions

`examples/mcp-definitions.yaml` — define an MCP server once under the
top-level `mcps:` map, reference it by name from several agents'
`toolsets:` (`ref: context7`), optionally narrowing `tools:` per agent.
Avoids repeating `env`/`command`/`args` on every agent that needs the same
server.

## 4. Remote MCP server with explicit OAuth

`examples/remote_mcp_oauth.yaml` — for a remote MCP server that doesn't
support Dynamic Client Registration (e.g. Slack, GitHub): a `type: mcp`
toolset with `remote: {url, transport_type, oauth: {clientId, clientSecret,
callbackPort, scopes}}`. See also `examples/remote_mcp_reconnect.yaml` and
`examples/remote_mcp_allow_private_ips.yaml` for related remote-MCP
variants.

## 5. Always-sandboxed agent

`examples/sandbox_agent.yaml` — `runtime: {sandbox: true, network_allowlist:
[...]}` makes `docker-agent run <file>` behave like
`docker-agent run --sandbox <file>` without the caller remembering the flag.
An explicit `--sandbox=false` on the command line still wins over the
config default.

```console
$ docker-agent run examples/sandbox_agent.yaml --exec "list files" --safety restricted
```

If a sandboxed agent hits a "Blocked by network policy" error for a host
not in `network_allowlist`, the fix is `docker-agent sandbox allow
<host>` (persists across runs), not editing the config for a one-off.

## 6. Inline skills (no SKILL.md files needed)

`examples/skills_inline.yaml` — define a skill directly in the agent config
as an object under `skills:`, mixed freely with `"local"` and string name
filters. Required fields: `name`, `description`, `instructions`. Use
`context: fork` to run the skill as an isolated sub-agent. See also
`examples/skills_filter.yaml` (restrict to named skills) and
`examples/skills_fork_toolsets.yaml` (extra toolsets for a fork-mode skill).

## 7. Run-wide and shared spend budgets

`examples/budget.yaml` — a top-level `budget:` caps total spend across every
agent in the run; named entries under `budgets:` are shared pots multiple
agents can opt into via their own `budgets: [name]` field (so a fan-out to N
sub-agents can't multiply the ceiling by N). Costs on an uncatalogued model
(custom `base_url`) must be declared explicitly via the model's `cost:`
field or `max_cost` never triggers.

## 8. Share a config via an OCI registry

No dedicated example file — any config works:

```console
$ docker-agent share push examples/basic_agent.yaml myorg/greeter:latest
$ docker-agent share pull myorg/greeter:latest
$ docker-agent run myorg/greeter:latest --exec "hi" --safety restricted
```

## 9. Expose an agent as a server

```console
$ docker-agent serve mcp examples/basic_agent.yaml            # MCP over stdio
$ docker-agent serve mcp examples/basic_agent.yaml --http --listen 127.0.0.1:9090
$ docker-agent serve chat examples/basic_agent.yaml --listen 127.0.0.1:9090
```

`examples/chat/` has a minimal OpenAI-SDK client proving any
OpenAI-compatible client works against `serve chat`. `examples/tic-tac-toe-mcp.yaml`
is a worked `serve mcp` target agent.

## 10. Evaluate an agent against saved sessions

`examples/eval/` — `demo.yaml` (agent under test) plus `evals/` (saved
session JSON fixtures).

```console
$ docker-agent eval examples/eval/demo.yaml examples/eval/evals
```

## Format note

YAML is the recommended and most common format; HCL (`*.hcl`) is also
supported — `examples/pirate.yaml`/`examples/pirate.hcl` and
`examples/gopher.yaml`/`examples/gopher.hcl` show equivalent agents in both.
Everything in this skill assumes YAML unless the user's file says otherwise.
