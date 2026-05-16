---
title: "Sandbox Mode"
description: "Run agents in an isolated Docker sandbox VM for enhanced security."
permalink: /configuration/sandbox/
---

# Sandbox Mode

_Run agents in an isolated Docker sandbox VM for enhanced security._

## Overview

Sandbox mode runs the entire agent inside a disposable sandbox VM instead of directly on the host system. All shell, filesystem, and process activity happens inside that VM, so a misbehaving agent cannot touch files outside the mounted working directory or reach long-lived host state.

The backend is provided by the [`docker sandbox`](https://docs.docker.com/desktop/features/sandbox/) CLI plugin (ships with Docker Desktop) or the standalone [`sbx`](https://github.com/docker/sbx) CLI if it is on `PATH`.

<div class="callout callout-info" markdown="1">
<div class="callout-title">Requirements
</div>
  <p>Sandbox mode requires Docker Desktop with sandbox support (or a working <code>sbx</code> CLI). docker-agent shells out to these tools, it does not start raw <code>docker run</code> containers.</p>

</div>

## Usage

Enable sandbox mode with the `--sandbox` flag on the `docker agent run` command:

```bash
docker agent run --sandbox agent.yaml
```

docker-agent launches a sandbox VM, copies itself into it, mounts the current working directory, and re-runs the agent from inside.

## Flags

| Flag          | Default                                      | Description                                                                                               |
| ------------- | -------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `--sandbox`   | `false`                                      | Enable sandbox mode.                                                                                      |
| `--template`  | `docker/sandbox-templates:docker-agent`      | OCI image used as the sandbox template. Passed to `docker sandbox create -t` / `sbx create -t`.           |
| `--sbx`       | `true`                                       | Prefer the `sbx` CLI backend when it is available. Set `--sbx=false` to always use `docker sandbox`.      |

```bash
# Use a custom template image
docker agent run --sandbox --template myorg/custom-agent-template:latest agent.yaml

# Force the docker sandbox backend even if sbx is on PATH
docker agent run --sandbox --sbx=false agent.yaml
```

## Example

```yaml
# agent.yaml
agents:
  root:
    model: openai/gpt-4o
    description: Agent with sandboxed shell
    instruction: You are a helpful assistant.
    toolsets:
      - type: shell
```

```bash
docker agent run --sandbox agent.yaml
```

## How It Works

1. `--sandbox` tells docker-agent to prefer the `sbx` CLI (if available and `--sbx` is true), otherwise it falls back to `docker sandbox`.
2. A new sandbox VM is created from the image passed via `--template`.
3. The current working directory is mounted into the VM; the agent binary is copied in.
4. All tools (shell, filesystem, background jobs, etc.) run inside the VM.
5. When the session ends, the sandbox VM is stopped and removed.

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Limitations
</div>

- Sandbox VMs start fresh each session (no persistence between sessions).
- Only the working directory is mounted; files outside it are not visible to the agent.
- Network egress is constrained by the sandbox backend's policy.

</div>
