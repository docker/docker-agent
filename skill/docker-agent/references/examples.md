# Worked examples

Every pattern below is self-contained, copyable YAML — save it to a file and
run it. (The upstream `docker/docker-agent` repository also ships a larger,
curated example set at
<https://github.com/docker/docker-agent/tree/main/examples> if you want more
variety; not required for any of this.)

## 1. Minimal single agent

```yaml
# agent.yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: A helpful AI assistant
    instruction: |
      You are a knowledgeable assistant. Be helpful, accurate, and concise.
```

```console
$ docker-agent debug config agent.yaml   # validate
$ docker-agent run agent.yaml --exec "hello" --safety restricted
```

## 2. Delegating to another agent (full flow)

The flagship pattern for using `docker-agent` from inside another agent's
session: you're an orchestrator (in any harness, any language) and want a
separately-configured `docker-agent` agent to do a sub-task, then hand its
answer back to you.

```yaml
# reviewer.yaml — the delegate. Nothing about it is special-cased for
# delegation; it's just an ordinary single-purpose agent.
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Reviews a code diff for correctness and style issues.
    instruction: |
      You review code diffs. Given a diff, list concrete issues (bugs,
      style violations, missing tests) as a short bullet list. If the diff
      looks fine, say so in one sentence — don't invent problems.
```

Invoke it non-interactively and capture structured output:

```console
$ docker-agent run ./reviewer.yaml \
    --exec "review this diff:$(git diff)" \
    --safety restricted \
    --json > review.json

$ jq -rs '[.[] | select(.type=="agent_choice") | .content] | join("")' review.json
# => the delegate's answer, reassembled from its streamed response chunks
```

- `--exec` is mandatory here — without it, `run` launches the interactive
  TUI and hangs waiting for a terminal that doesn't exist.
- `--json` streams one JSON object per line (NDJSON) — team/toolset info,
  `user_message`, then one `agent_choice` event per response chunk, then
  `token_usage`/`stream_stopped`. There is no single top-level "result"
  field; join the `agent_choice` events' `content` in order to get the full
  answer, as shown above.
- `--safety restricted` auto-approves the delegate's safe tool calls and
  denies the rest without prompting — required because nothing is watching
  the terminal to answer an approval prompt.
- Swap `./reviewer.yaml` for an OCI ref (`myorg/reviewer:latest`) to
  delegate to a shared, versioned agent instead of a local file.

If the delegate should look like a long-lived, callable tool to your
harness instead of something you shell out to per task, expose it once and
call it repeatedly instead:

```console
$ docker-agent serve mcp ./reviewer.yaml --http --listen 127.0.0.1:9090
```

`--http` is required for a network listener — without it, `serve mcp` talks
MCP over stdio regardless of `--listen`. With `--http`, your harness then
talks MCP to `127.0.0.1:9090` rather than re-invoking the CLI for every
review.

## 3. Multi-agent team with in-config delegation

When you're authoring the config yourself (not just invoking someone
else's), declare the team directly and let the model decide when to
delegate:

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Lead developer
    instruction: You are the lead developer. Coordinate the team.
    sub_agents: [frontend, backend]
    toolsets: [{type: filesystem}, {type: think}]
  frontend:
    model: anthropic/claude-sonnet-4-5
    description: Frontend engineer
    instruction: You are a frontend engineer.
    toolsets: [{type: filesystem}, {type: shell}]
  backend:
    model: anthropic/claude-sonnet-4-5
    description: Backend engineer
    instruction: You are a backend engineer.
    toolsets: [{type: filesystem}, {type: shell}]
```

`sub_agents` entries can also be an OCI ref (`myorg/frontend-expert:latest`)
or `name:reference` to pull in an external agent under a local name,
mixing local and remote delegates in the same team.

## 4. Reusable MCP server definitions

Define an MCP server once under the top-level `mcps:` map, reference it by
name from several agents' `toolsets:`, optionally narrowing `tools:` per
agent — avoids repeating `env`/`command`/`args` on every agent that needs
the same server.

```yaml
mcps:
  context7:
    ref: docker:context7

agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Lead developer
    instruction: You are the lead developer.
    sub_agents: [frontend]
    toolsets:
      - type: filesystem
      - type: mcp
        ref: context7
  frontend:
    model: anthropic/claude-sonnet-4-5
    description: Frontend engineer
    instruction: You are a frontend engineer.
    toolsets:
      - type: filesystem
      - type: mcp
        ref: context7
        tools: [get_docs]   # narrow the shared server's tools for this agent
```

## 5. Remote MCP server with explicit OAuth

For a remote MCP server that doesn't support Dynamic Client Registration
(e.g. Slack, GitHub):

```yaml
agents:
  root:
    model: openai/gpt-4.1-mini
    description: Assistant with remote MCP tools using explicit OAuth credentials
    instruction: You are a helpful assistant with access to remote tools.
    toolsets:
      - type: mcp
        remote:
          url: "https://mcp.example.com/mcp"
          transport_type: streamable
          oauth:
            clientId: "your-client-id"
            clientSecret: "your-client-secret"
            callbackPort: 8080
            scopes: ["read", "write"]
```

## 6. Always-sandboxed agent

`runtime: {sandbox: true, ...}` makes `docker-agent run <file>` behave like
`docker-agent run --sandbox <file>` without the caller remembering the
flag. An explicit `--sandbox=false` on the command line still wins over the
config default.

```yaml
runtime:
  sandbox: true
  network_allowlist: [api.example.com, registry.npmjs.org]

agents:
  root:
    model: openai/gpt-4o
    description: A helpful assistant that runs shell commands in a sandboxed environment.
    instruction: You are a helpful assistant with access to a sandboxed shell environment.
    toolsets:
      - type: shell
```

```console
$ docker-agent run agent.yaml --exec "list files" --safety restricted
```

If a sandboxed agent hits a "Blocked by network policy" error for a host
not in `network_allowlist`, the fix is `docker-agent sandbox allow <host>`
(persists across runs), not editing the config for a one-off.

## 7. Inline skills (no SKILL.md files needed)

Define a skill directly in the agent config as an object under `skills:`,
mixed freely with `"local"` and string name filters.

```yaml
agents:
  root:
    model: openai/gpt-4o-mini
    description: An agent that demonstrates inline skill definitions.
    instruction: |
      You are a helpful assistant. Use the available skills when the
      user's request matches one.
    skills:
      - name: changelog
        description: Write a concise changelog entry from a diff or description.
        instructions: |
          Produce a single changelog entry in Keep a Changelog style:
          pick Added/Changed/Fixed/Removed, then one imperative sentence.
      - name: triage
        description: Triage a bug report in an isolated context.
        context: fork   # runs as an isolated sub-agent, not inline
        instructions: |
          Restate the problem in one sentence, list likely root causes
          most-probable-first, propose the smallest reproduction.
    toolsets:
      - type: filesystem
```

## 8. Run-wide and shared spend budgets

A top-level `budget:` caps total spend across every agent in the run; named
entries under `budgets:` are shared pots multiple agents can opt into via
their own `budgets: [name]` field (so a fan-out to N sub-agents can't
multiply the ceiling by N).

```yaml
budgets:
  shell-work:
    max_cost: 0.03
    max_tokens: 8000

budget:
  max_cost: 0.05
  max_tokens: 15000
  max_time: 2m

agents:
  root:
    model: openai/gpt-4o-mini
    description: An agent on a short leash.
    instruction: You are a helpful assistant with shell access. Be concise.
    toolsets:
      - type: shell
    budgets: [shell-work]
```

Costs on an uncatalogued model (custom `base_url`) must be declared
explicitly via the model's `cost:` field, or `max_cost` never triggers.

## 9. Share a config via an OCI registry

```console
$ docker-agent share push ./agent.yaml myorg/greeter:latest
$ docker-agent share pull myorg/greeter:latest
$ docker-agent run myorg/greeter:latest --exec "hi" --safety restricted
```

## 10. Expose an agent as a server

```console
$ docker-agent serve mcp ./agent.yaml                          # MCP over stdio
$ docker-agent serve mcp ./agent.yaml --http --listen 127.0.0.1:9090
$ docker-agent serve chat ./agent.yaml --listen 127.0.0.1:9090  # OpenAI-compatible
```

Any OpenAI-SDK-compatible client can talk to `serve chat`'s
`/v1/chat/completions` endpoint without custom integration work.

## 11. Evaluate an agent against saved sessions

Evaluations replay saved session JSON files (each a recorded conversation
plus scoring criteria) against the current agent and score the results.
By default docker-agent looks in an `evals/` directory next to the agent
file. Create fixtures from real conversations: run the agent interactively
(`docker-agent run agent.yaml`), have the conversation you want to test,
then use the `/eval` slash command in the TUI to save it — this captures
the user message and the tool calls actually made. Add scoring criteria by
hand afterward:

```json
// evals/example.json (abbreviated — /eval fills in the messages array)
{
  "id": "...",
  "messages": [ /* captured by /eval */ ],
  "evals": {
    "relevance": ["The response mentions exactly 2 files"],
    "assertions": [
      {"name": "used list_directory", "type": "tool_called", "value": "list_directory"}
    ]
  }
}
```

```console
$ docker-agent eval ./agent.yaml ./evals
$ docker-agent eval ./agent.yaml ./evals --baseline ./evals/results/<prior-run>.json  # regression check
```

Requires a Docker-compatible container runtime (evaluations run inside
containers for isolation).

## Format note

YAML is the recommended and most common format for docker-agent configs;
HCL (`*.hcl`) is also supported with equivalent structure. Everything in
this skill assumes YAML unless the file extension says otherwise.
