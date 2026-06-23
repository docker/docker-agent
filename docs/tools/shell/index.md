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

| Property       | Type    | Description                                                                                          |
| -------------- | ------- | --------------------------------------------------------------------------------------------------- |
| `env`          | object  | Environment variables to set for all shell commands                                                 |
| `sudo_askpass` | boolean | Opt in to prompting for a `sudo` password (see [Sudo support](#sudo-support)). Default `false`.     |

### Custom Environment Variables

```yaml
toolsets:
  - type: shell
    env:
      MY_VAR: "value"
      PATH: "${PATH}:/custom/bin"
```

### Sudo support

By default a shell command has no controlling terminal, so a `sudo` command that needs a password hangs until it times out (the agent usually gives up and falls back to printing manual instructions).

Set `sudo_askpass: true` to enable a sudo privilege escalation flow:

```yaml
toolsets:
  - type: shell
    sudo_askpass: true
```

When enabled, `sudo` commands prompt you for your password through the host UI (the input is masked). The password is handed to `sudo` over a private, per-session socket via the standard `SUDO_ASKPASS` mechanism — it is never written to the command line, the logs, or stored by the agent.

The bridge environment variables (`SUDO_ASKPASS`, `CAGENT_ASKPASS_SOCKET`, `CAGENT_ASKPASS_TOKEN`) are added only to commands that invoke `sudo`, but within such a command they are visible to every child process, not just `sudo`. They carry a socket path and a session token, not the password; the socket lives in a `0700` directory, so only your own user can reach it.

Notes and limitations:

- Unix only. The flag has no effect on Windows.
- Interactive UI only. In headless / non-interactive runs the prompt is declined automatically and `sudo` fails as before.
- Only a bare `sudo ...` invocation in a POSIX shell (`sh`, `bash`, `zsh`, ...) is handled. `sudo` called by absolute path (`/usr/bin/sudo`), via `env sudo`, from inside a nested script, or under a non-POSIX shell (e.g. `fish`) is not intercepted and behaves as before.
- Caching is `sudo`'s own. Because each shell tool call runs in a fresh shell with no controlling terminal, `sudo`'s credential cache does not persist across separate tool calls: you are prompted once per shell command that uses `sudo`. Within a single command, multiple `sudo` calls (e.g. `sudo a && sudo b`) usually share one prompt, subject to `sudo`'s own timestamp configuration.
- The prompt must be answered within the command's timeout; raise the `timeout` parameter for `sudo` commands that may wait on input. Background jobs (`run_background_job`) are wired too, but their prompt only works while the originating turn is still active.
- Prompts are serialized: if a single command runs two `sudo` calls in parallel (e.g. `sudo a & sudo b`), the second waits for the first prompt to be answered rather than opening two dialogs at once.

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
