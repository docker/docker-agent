# Experimental MCP Skills demo

Enable experimental MCP Skills in `~/.config/cagent/config.yaml`:

```yaml
settings:
  experimental:
    mcp_skills: true
```

Run from the repository root:

```sh
./bin/docker-agent run examples/mcp-skills/agent-stdio.yaml
```

Ask: **“List the demo server's skills, load code-review, and read its checklist.”**

## Scope

This implements the MCP Skills extension (`io.modelcontextprotocol/skills`,
[SEP-2640](https://modelcontextprotocol.io/extensions/skills/overview)) for
protocol revision `2026-07-28`, integrated into docker-agent's builtin skills
system (`read_skill`, `read_skill_file`, and `<available_skills>`):

- Discovers skills via `skills/list` (paginated) without prefetching files,
  surfacing them under `mcp:<server>/<uri>` with `<origin>mcp:<server></origin>`.
- Loads listed or unlisted skills by URI via `read_skill` (`skills/get` +
  `resources/read`), verifying raw byte size, SHA-256 digest, and YAML
  frontmatter equality.
- Reads supporting files relative to the skill root via `read_skill_file` with
  manifest enforcement and verified in-memory caching, and supports directory
  listing (from the held manifest, or via `resources/directory/read` for
  `"dynamic"` skills when `directoryRead: true`).
- Isolates MCP skills per server using the host-assigned toolset label, runs
  them through the skill content guard, disables `!` command expansion and
  forking, and tags content as untrusted external data (frontmatter fields such
  as `allowed-tools` do not grant host permissions).
