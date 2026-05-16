---
title: "Skills"
description: "Skills provide specialized instructions that agents can load on demand when a task matches a skill's description."
permalink: /features/skills/
---

# Skills

_Skills provide specialized instructions that agents can load on demand when a task matches a skill's description._

## How Skills Work

1. docker-agent scans standard directories for `SKILL.md` files
2. Skill metadata (name, description) is injected into the agent's system prompt
3. When a user request matches a skill, the agent reads the full instructions
4. The agent follows the skill's detailed instructions to complete the task

## Enabling Skills

```yaml
agents:
  root:
    model: openai/gpt-4o
    instruction: You are a helpful assistant.
    skills: true
    toolsets:
      - type: filesystem # required for reading skill files
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Tip
</div>
  <p>Skills are perfect for encoding team-specific workflows (PR review, deployment, coding standards) that apply across projects.</p>

</div>

## Filtering Skills

The `skills` field also accepts a list, letting you restrict the agent to a specific subset of skills instead of exposing every discovered one. List items are classified automatically:

- `"local"` or any `http://` / `https://` URL → a **source** to load skills from
- any other string → the **name** of a skill to include

When only names are given, local sources are used by default.

```yaml
agents:
  # Load every discovered local skill (same as `skills: true`).
  full:
    skills: true

  # Load local skills, but only expose "commit" and "poem".
  scoped:
    skills:
      - commit
      - poem

  # Combine an explicit source with a name filter.
  remote_filtered:
    skills:
      - https://skills.example.com
      - commit

  # Disable skills entirely.
  none:
    skills: false
```

A name that doesn't match any discovered skill is logged as a warning at startup but is otherwise ignored.

## SKILL.md Format

<!-- yaml-lint:skip -->
```yaml
---
name: create-dockerfile
description: Create optimized Dockerfiles for applications
license: Apache-2.0
metadata:
  author: my-org
  version: "1.0"
---

# Creating Dockerfiles

When asked to create a Dockerfile:

1. Analyze the application type and language
2. Use multi-stage builds for compiled languages
3. Minimize image size by using slim base images
4. Follow security best practices (non-root user, etc.)
```

### Frontmatter Fields

| Field            | Required | Description                                                                 |
| ---------------- | -------- | --------------------------------------------------------------------------- |
| `name`           | Yes      | Unique skill identifier                                                     |
| `description`    | Yes      | Short description shown to the agent for skill matching                     |
| `context`        | No       | Set to `fork` to run the skill as an isolated sub-agent (see below)         |
| `model`          | No       | Override the model used while running the skill as a sub-agent (fork only)  |
| `allowed-tools`  | No       | List of tools the skill needs (YAML list or comma-separated string)         |
| `license`        | No       | License identifier (e.g. `Apache-2.0`)                                      |
| `compatibility`  | No       | Free-text compatibility notes                                               |
| `metadata`       | No       | Arbitrary key-value pairs (e.g. `author`, `version`)                        |

## Running a Skill as a Sub-Agent

By default, when an agent invokes a skill it reads the instructions inline into its own conversation. For complex, multi-step skills this can consume a large portion of the agent's context window and pollute the parent conversation with intermediate tool calls.

Adding `context: fork` to the SKILL.md frontmatter tells the agent to run the skill in an **isolated sub-agent** instead:

<!-- yaml-lint:skip -->
```yaml
---
name: bump-go-dependencies
description: Update Go module dependencies one by one
context: fork
---

# Bump Dependencies

1. List outdated deps
2. Update each one, run tests, commit or revert
3. Produce a summary table
```

When the agent encounters a task that matches a `context: fork` skill, it uses the `run_skill` tool instead of `read_skill`. This:

- **Spawns a child session** with the skill content as the system prompt and the caller's task as the user message
- **Isolates the context window** — the sub-agent has its own conversation history, so lengthy tool-call chains don't eat into the parent's token budget
- **Folds the result** — only the sub-agent's final answer is returned to the parent as the tool result
- **Inherits the parent's model and tools** — the sub-agent can use all tools available to the parent agent

<div class="callout callout-tip" markdown="1">
<div class="callout-title">When to use context: fork
</div>
  <p>Use <code>context: fork</code> for skills that involve many steps, heavy tool usage, or that should not clutter the main conversation — for example dependency bumping, large refactors, or code generation pipelines.</p>

</div>

### Overriding the model for a fork skill

Fork skills can declare a `model` field in their frontmatter to use a
different model than the parent agent for the duration of the sub-session.
This is useful when a skill is best handled by a faster, cheaper, or more
specialised model — for example a powerful reasoning model for refactors,
or a fast model for routine bookkeeping work. The override only applies
while the skill is running; the parent agent keeps its own model.

The `model` value accepts either a named model from the agent config or
an inline `provider/model` reference (and the same comma-separated alloy
syntax as the rest of the agent config):

<!-- yaml-lint:skip -->
```yaml
---
name: bump-go-dependencies
description: Update Go module dependencies one by one
context: fork
model: openai/gpt-4o-mini
---

# Bump Dependencies

1. ...
```

If the model reference cannot be resolved (unknown name, missing
credentials, runtime not configured for model switching, …) the skill
falls back to the agent's currently-active model (its configured
default, or any override the user previously set via the model picker)
and a warning is logged.

When the skill completes, the agent's previous model is restored — but
only if no one else changed the model in the meantime. If the user
switches the model via the TUI model picker while the fork skill is
running, their choice is preserved (the deferred restore becomes a
no-op).

## Search Paths

Skills are discovered from these locations (later overrides earlier):

### Global

| Path                | Search Type                             |
| ------------------- | --------------------------------------- |
| `~/.codex/skills/`  | Recursive (searches all subdirectories) |
| `~/.claude/skills/` | Flat (immediate children only)          |
| `~/.agents/skills/` | Recursive (searches all subdirectories) |

### Project (from git root to current directory)

| Path              | Search Type                                |
| ----------------- | ------------------------------------------ |
| `.claude/skills/` | Flat (cwd only)                            |
| `.agents/skills/` | Flat (each directory from git root to cwd) |

## Invoking Skills

Skills can be invoked in multiple ways:

- **Automatic:** The agent detects when your request matches a skill's description and loads it automatically
- **Explicit:** Reference the skill name in your prompt: "Use the create-dockerfile skill to..."
- **Slash command:** Use `/{skill-name}` to invoke a skill directly

```bash
# In the TUI, invoke skill directly:
/create-dockerfile

# Or mention it in your message:
"Create a dockerfile for my Python app (use the create-dockerfile skill)"
```

## Precedence

When multiple skills share the same name:

1. Global skills load first
2. Project skills load next, from git root toward current directory
3. Skills closer to the current directory override those further away

## Creating a Skill

```bash
# Create the skill directory
$ mkdir -p ~/.agents/skills/create-dockerfile

# Write the SKILL.md file
$ cat > ~/.agents/skills/create-dockerfile/SKILL.md << 'EOF'
---
name: create-dockerfile
description: Create optimized Dockerfiles for applications
---

# Creating Dockerfiles

When asked to create a Dockerfile:

1. Analyze the application type and language
2. Use multi-stage builds for compiled languages
3. Use slim base images to minimize size
4. Run as non-root user for security
EOF
```

The skill will automatically be available to any agent with skills enabled (`skills: true`, or a list that targets its name — see [Filtering Skills](#filtering-skills)).

<div class="callout callout-info" markdown="1">
<div class="callout-title">See also
</div>
  <p>Skills are enabled in the <a href="{{ '/configuration/agents/' | relative_url }}">Agent Config</a> with the <code>skills</code> property (boolean or list). For tool-based capabilities, see <a href="{{ '/concepts/tools/' | relative_url }}">Tools</a>.</p>

</div>
