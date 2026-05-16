---
title: "Background Agents Tool"
description: "Dispatch work to sub-agents concurrently and collect results asynchronously."
permalink: /tools/background-agents/
---

# Background Agents Tool

_Dispatch work to sub-agents concurrently and collect results asynchronously._

## Overview

The background agents tool lets an orchestrator dispatch work to sub-agents concurrently and collect results asynchronously. Unlike [transfer_task]({{ '/tools/transfer-task/' | relative_url }}) (which blocks until the sub-agent finishes), background agent tasks run in parallel — the orchestrator can start several tasks, do other work, and check on them later.

## Available Tools

| Tool                     | Description                                                     |
| ------------------------ | --------------------------------------------------------------- |
| `run_background_agent`   | Start a sub-agent task in the background; returns a task ID     |
| `list_background_agents` | List all background tasks with their status and runtime         |
| `view_background_agent`  | View live output or final result of a task by ID                |
| `stop_background_agent`  | Cancel a running task by ID                                     |

### `run_background_agent` parameters

| Parameter         | Type   | Required | Description                                                                 |
| ----------------- | ------ | -------- | --------------------------------------------------------------------------- |
| `agent`           | string | ✓        | Name of the sub-agent to run. Must be listed under the caller's `sub_agents`. |
| `task`            | string | ✓        | Clear, concise description of the task the sub-agent should achieve.        |
| `expected_output` | string | ✗        | Optional description of the result format the caller expects.               |

`run_background_agent` returns a **task ID** string. Tools run by the sub-agent are pre-approved, so only dispatch to trusted sub-agents with well-scoped tasks.

### `view_background_agent` and `stop_background_agent` parameters

| Parameter | Type   | Required | Description                                                    |
| --------- | ------ | -------- | -------------------------------------------------------------- |
| `task_id` | string | ✓        | Task ID returned by `run_background_agent` or `list_background_agents`. |

`list_background_agents` takes no parameters.

## Configuration

```yaml
toolsets:
  - type: background_agents
```

No configuration options. Requires the agent to have `sub_agents` configured so the background tasks have agents to dispatch to.

## Example

```yaml
agents:
  coordinator:
    model: openai/gpt-4o
    description: Orchestrates parallel research
    instruction: Fan out research tasks and synthesize results.
    sub_agents: [researcher]
    toolsets:
      - type: background_agents
      - type: think

  researcher:
    model: openai/gpt-4o
    description: Web researcher
    instruction: Research topics thoroughly.
    toolsets:
      - type: mcp
        ref: docker:duckduckgo
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">When to Use
</div>
  <p>Use <code>background_agents</code> when your orchestrator needs to fan out work to multiple specialists in parallel — for example, researching several topics simultaneously or running independent code analyses side by side.</p>
</div>
