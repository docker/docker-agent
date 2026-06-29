---
title: "Plan Tool"
description: "Per-session plan tracker for the draft, approve, execute workflow."
permalink: /tools/plan/
---

# Plan Tool

_Per-session plan tracker for the "draft, approve, execute" workflow._

## Overview

The plan toolset gives an agent a place to write down the plan it intends to execute, ask the user to approve it, and hand the conversation off to another agent for execution. There is exactly one plan per session, stored as a markdown file at:

```
~/.cagent/plans/<session-id>.md
```

The toolset is built around three tools:

| Tool             | Description                                                                                          |
| ---------------- | ---------------------------------------------------------------------------------------------------- |
| `write_plan`     | Create or replace the session's plan as markdown. There is one plan per session.                     |
| `read_plan`      | Read the plan written for the current session and return it as markdown.                             |
| `exit_plan_mode` | Signal that the plan is ready for review. Does not switch agents on its own.                         |

## Configuration

```yaml
toolsets:
  - type: plan
```

No configuration options. The plan path is derived from the session ID; the agent does not name plans.

Restrict the toolset to a subset of tools the standard way:

```yaml
# An agent that consumes a plan but should not be able to (re)write or
# approve it.
toolsets:
  - type: plan
    tools:
      - read_plan
```

## When to call exit_plan_mode

Call `exit_plan_mode` once the plan is complete and you do not intend to change it on the next turn. It validates that a plan exists for the session and returns a "ready for review" tool result. It does **not** switch agents or solicit user approval on its own — the host application owns the next-turn routing (for example, by reading the tool result, by a UI affordance the user toggles, or by a `handoff` declared on the agent).

This separation keeps the tool reusable across UIs: a CLI that prints tool results inline, a chat UI with a plan-mode toggle, and a server that auto-routes the next turn through a `handoff` can all consume the same signal without one stepping on another.

## Storage and cleanup

- Plans are markdown files written atomically (temp + rename), so concurrent readers — in this process or another — never observe a partial write.
- A best-effort sweep on first use of the toolset removes plan files older than 30 days under the plans directory. Stranded plans for long-gone sessions do not accumulate.
- The session ID identifies the file directly. There is no in-process mutex, revision counter, or named-plan collision check, because two sessions cannot map to the same path.

## Events

A `plan_updated` event is emitted whenever `write_plan` succeeds:

```json
{
  "type": "plan_updated",
  "session_id": "...",
  "path": "/Users/.../.cagent/plans/<session-id>.md",
  "content": "# my plan\n...",
  "agent_name": "planner"
}
```

Embedders that render the plan inline can subscribe and update without re-reading the file.

## Example

A two-agent workflow: `root` executes, `planner` plans. `/plan` hands off to the planner; `exit_plan_mode` hands back.

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Executes approved plans
    instruction: |
      You execute plans the planner has handed off. When you see a message
      that a plan has been approved, read it with read_plan and work through
      its steps in order.
    toolsets:
      - type: plan
        tools:
          - read_plan
      - type: filesystem
      - type: shell
    commands:
      plan:
        description: "Switch to the planner"
        agent: planner

  planner:
    model: anthropic/claude-sonnet-4-5
    description: Investigates and writes plans for approval
    instruction: |
      Investigate the user's request, then write the plan with write_plan.
      Iterate with the user until the plan is complete, then call
      exit_plan_mode to hand off to root for execution.
    toolsets:
      - type: plan
      - type: filesystem
        readonly: true
      - type: user_prompt
```

See [`examples/shared_plan.yaml`](https://github.com/docker/docker-agent/blob/main/examples/shared_plan.yaml) for a complete working example.

## Error Handling

- `read_plan` and `exit_plan_mode` return a "no plan written yet" error when called before `write_plan`, telling the agent (and the user) the path they would write to.
- `write_plan` validates the session ID and refuses to write anything that could escape the plans directory; in practice the runtime generates UUIDs so this only triggers if an embedder supplies a hand-crafted ID.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Plan vs. Todo vs. Tasks
</div>
  <p>Use <strong>plan</strong> when one agent drafts an approach for the user to approve before another agent executes it. Use <a href="{{ '/tools/todo/' | relative_url }}">todo</a> for lightweight in-session task lists. Use <a href="{{ '/tools/tasks/' | relative_url }}">tasks</a> for a structured, persistent task database with priorities and dependencies.</p>
</div>
