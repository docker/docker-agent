---
title: "Run in Docker Sandboxes"
description: "Run Docker Agent in an isolated VM with sbx and Sandbox Kits."
keywords: docker agent, sbx, ai agents, sandbox, sandbox kits
weight: 80
canonical: https://docs.docker.com/ai/docker-agent/configuration/sandbox/
---

Use the [`sbx` CLI](https://docs.docker.com/ai/sandboxes/) to run Docker Agent and its tools inside a sandbox VM. The [Docker Agent workload kit](https://hub.docker.com/r/sbx/docker-agent) supplies the binary and its runtime environment. You don't need Docker Agent installed on the host.

## Start a local sandbox

[Install or update `sbx`](https://docs.docker.com/ai/sandboxes/install/). Sign in to Docker and register an API key for the provider your agent uses. For example, to use an OpenAI model:

```bash
sbx login
sbx secret set openai
```

From your project directory, start the kit:

```bash
sbx run --name my-project docker-agent .
```

Current `sbx` releases resolve `docker-agent` to the published kit. Older releases that bundle Docker Agent also accept this short name, but may reject an explicit kit reference with an `already registered` error.

The `.` shares your current directory with the sandbox. The agent can change files in that directory, and those changes are reflected on the host. Share only what the agent needs. Docker Agent does not copy your host aliases, user settings, prompt files, or skills into the VM. Put project configuration in the workspace, or provide shared context and skills through a kit.

The published workload opens the interactive agent picker with `--yolo`, so it doesn't prompt before tool calls. To use Docker Agent's approval prompts instead, start the sandbox without opening the picker, then run your config directly inside the VM:

```bash
sbx run --detach --name my-project docker-agent .
sbx exec -it my-project docker-agent run --safety strict ./agent.yaml
```

`agent.yaml` must be accessible inside the sandbox. Use `default` instead to run the built-in coding agent. List or remove sandboxes with `sbx ls` and `sbx rm my-project`.

## Run a headless job

Once the sandbox is running and its kit permissions and provider credentials are configured, use `sbx exec` without a TTY. Select an explicit safety mode for unattended runs:

```bash
sbx exec my-project docker-agent run --exec --json --safety restricted ./agent.yaml "Summarize this project"
```

## Customize the kit or run in the cloud

With a current `sbx` release, you can reference the published kit explicitly as `sbx/docker-agent:latest` or pin it to a specific version or digest. The [Docker Agent example in the Sandbox Kit specification](https://github.com/docker/sandbox-kit-spec/tree/main/examples/docker-agent) shows how to define your own v3 workload, including provider credentials and network policy. To run that source kit locally from a checkout of `sandbox-kit-spec`:

```bash
sbx run ./examples/docker-agent /path/to/workspace
```

You can also add compatible v3 mixins to a workload with `sbx run --kit`. See [Use kits](https://docs.docker.com/ai/sandboxes/customize/use-kits/) for kit references, composition, and configuration. If a provider, registry, or MCP server is blocked, inspect `sbx policy log` and update the kit or sandbox's network policy rather than the agent YAML.

For Docker Cloud Sandboxes, configure a cloud provider secret (for example, `sbx --cloud secret set openai`) and use `sbx --cloud run docker-agent` with no local workspace path. Make the project available remotely; local files and secrets are not uploaded automatically. Use `sbx --cloud ls`, `sbx --cloud stop`, and `sbx --cloud rm` to manage the sandbox lifecycle.

## Migrate from the Docker Agent sandbox flags

Replace `docker agent run --sandbox` with `sbx run` followed, if needed, by `sbx exec`. The `--sbx` backend selector and the other Docker Agent sandbox flags are no longer supported. Remove `runtime.sandbox` and `runtime.network_allowlist` from agent configs and `sandbox` from aliases after moving the launch to `sbx`. Move any `sandbox_allowlist` entries you still need to Sandbox Kit or `sbx` network policy. Running `docker-agent run` directly does not provide VM isolation.
