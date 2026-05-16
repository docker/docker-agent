---
title: "Agent Distribution"
description: "Package, share, and run agents via OCI-compatible registries — just like container images."
permalink: /concepts/distribution/
---

# Agent Distribution

_Package, share, and run agents via OCI-compatible registries — just like container images._

## Overview

docker-agent agents can be pushed to any OCI-compatible registry (Docker Hub, GitHub Container Registry, etc.) and pulled/run anywhere. This makes sharing agents as easy as sharing Docker images.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Tip
</div>
  <p>For CLI commands related to distribution, see <a href="{{ '/features/cli/' | relative_url }}">CLI Reference</a> (<code>docker agent share push</code>, <code>docker agent share pull</code>, <code>docker agent alias</code>).</p>

</div>

## Pushing Agents

```bash
# Push to Docker Hub
$ docker agent share push ./agent.yaml docker.io/username/my-agent:latest

# Push to GitHub Container Registry
$ docker agent share push ./agent.yaml ghcr.io/username/my-agent:v1.0
```

## Pulling Agents

```bash
# Pull an agent
$ docker agent share pull docker.io/username/my-agent:latest

# Pull from the agent catalog
$ docker agent share pull agentcatalog/pirate
```

## Running from a Registry

Run agents directly from a registry without pulling first:

```bash
# Run directly from Docker Hub
$ docker agent run docker.io/username/my-agent:latest

# Run from the agent catalog
$ docker agent run agentcatalog/pirate

# Run with a specific agent from a multi-agent config
$ docker agent run docker.io/username/dev-team:latest -a developer
```

## Agent Catalog

The `agentcatalog` namespace on Docker Hub hosts pre-built agents you can try:

```bash
# Try the pirate-themed assistant
$ docker agent run agentcatalog/pirate

# Try the coding agent
$ docker agent run agentcatalog/coder
```

## Using as Sub-Agents

Registry agents can be used directly as sub-agents in a multi-agent configuration — no need to define them locally:

```yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: Coordinator
    instruction: Delegate tasks to the right sub-agent.
    sub_agents:
      - agentcatalog/pirate         # auto-named "pirate"
      - my_reviewer:myorg/reviewer  # explicitly named "my_reviewer"
```

External sub-agents are automatically named after their last path segment. Use the `name:reference` syntax to give them a custom name.

See [External Sub-Agents]({{ '/concepts/multi-agent/#external-sub-agents-from-registries' | relative_url }}) for details.

## Using with Aliases

Combine OCI references with aliases for convenient access:

```bash
# Create an alias for a registry agent
$ docker agent alias add coder agentcatalog/coder --yolo

# Now just run
$ docker agent run coder
```

## Using with API Server

The API server supports OCI references with auto-refresh:

```bash
# Start API from registry, auto-pull every 10 minutes
$ docker agent serve api docker.io/username/agent:latest --pull-interval 10
```

## Private Repositories

docker-agent supports pulling from private GitHub repositories and registries that require authentication. Use standard Docker login or GitHub authentication:

```bash
# Login to a registry
$ docker login docker.io

# Now push/pull works with private repos
$ docker agent share push ./agent.yaml docker.io/myorg/private-agent:latest
$ docker agent run docker.io/myorg/private-agent:latest
```

<div class="callout callout-info" markdown="1">
<div class="callout-title">Troubleshooting
</div>
  <p>Having issues with push/pull? See <a href="{{ '/community/troubleshooting/' | relative_url }}">Troubleshooting</a> for common registry issues.</p>

</div>
