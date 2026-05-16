---
title: "Shell Tool"
description: "Execute arbitrary shell commands in the user's environment."
permalink: /tools/shell/
---

# Shell Tool

_Execute arbitrary shell commands in the user's environment._

## Overview

The shell tool allows agents to execute arbitrary shell commands. This is one of the most powerful tools — it lets agents run builds, install dependencies, query APIs, and interact with the system. Each call runs in a fresh, isolated shell session — no state persists between calls.

Commands have a default 30-second timeout and require user confirmation unless `--yolo` is used.

## Configuration

```yaml
toolsets:
  - type: shell
```

### Options

| Property | Type   | Description                                         |
| -------- | ------ | --------------------------------------------------- |
| `env`    | object | Environment variables to set for all shell commands |

### Custom Environment Variables

```yaml
toolsets:
  - type: shell
    env:
      MY_VAR: "value"
      PATH: "${PATH}:/custom/bin"
```

## Available Tools

The shell toolset exposes five tools:

| Tool Name              | Description                                                                                    |
| ---------------------- | ---------------------------------------------------------------------------------------------- |
| `shell`                | Run a command synchronously and return its combined output when it finishes.                   |
| `run_background_job`   | Start a command asynchronously and return a job ID immediately. Use for servers/watchers/etc. |
| `list_background_jobs` | List all background jobs with their status, runtime, and metadata.                             |
| `view_background_job`  | View the buffered output and status of a specific background job by ID.                        |
| `stop_background_job`  | Stop a running background job. Child processes are terminated too.                             |

Background job output is captured up to 10 MB per job. All background jobs are automatically terminated when the agent session ends.

### `shell` parameters

| Parameter | Type    | Required | Description                                                               |
| --------- | ------- | -------- | ------------------------------------------------------------------------- |
| `cmd`     | string  | ✓        | The shell command to execute.                                             |
| `cwd`     | string  | ✗        | Working directory to run the command in (default: `.`).                   |
| `timeout` | integer | ✗        | Per-call execution timeout in seconds (default: `30`).                    |

### `run_background_job` parameters

| Parameter | Type   | Required | Description                                                             |
| --------- | ------ | -------- | ----------------------------------------------------------------------- |
| `cmd`     | string | ✓        | The shell command to execute in the background.                         |
| `cwd`     | string | ✗        | Working directory to run the command in (default: `.`).                 |

`view_background_job` and `stop_background_job` each take a single required `job_id` string returned by `run_background_job` or `list_background_jobs`.

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Safety
</div>
  <p>The shell tool gives agents full access to the system shell. Always set <code>max_iterations</code> on agents that use the shell tool to prevent infinite loops. A value of 20–50 is typical for development agents. Use <a href="{{ '/configuration/sandbox/' | relative_url }}">Sandbox Mode</a> for additional isolation.</p>
</div>

<div class="callout callout-info" markdown="1">
<div class="callout-title">Tool Confirmation
</div>
  <p>By default, docker-agent asks for user confirmation before executing shell commands. Use <code>--yolo</code> to auto-approve all tool calls.</p>
</div>
