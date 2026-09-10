---
title: "Agent Distribution"
description: "Package, share, and run agents via OCI-compatible registries — just like container images."
keywords: docker agent, ai agents, concepts, agent distribution
weight: 50
canonical: https://docs.docker.com/ai/docker-agent/concepts/distribution/
aliases:
  - /ai/docker-agent/sharing-agents/
---

_Package, share, and run agents via OCI-compatible registries — just like container images._

## Overview

Docker Agent agents can be pushed to any OCI-compatible registry (Docker Hub, GitHub Container Registry, etc.) and pulled/run anywhere. This makes sharing agents as easy as sharing Docker images.

> [!TIP]
> For CLI commands related to distribution, see [CLI Reference](../../features/cli/index.md) (`docker agent share push`, `docker agent share pull`, `docker agent alias`).

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

# Pull from Docker Hub shorthand
$ docker agent share pull myorg/agent:tag
```

## Signing and Encrypting Agents

`share push --key <key>` protects the agent so that pullers holding the matching key can check it was published by you and has not been altered. The YAML is **always pushed in clear**; only the proof goes into the OCI manifest annotations. `share pull --key <key>` performs the check and refuses the artifact if it fails.

The key is given inline, or as a path prefixed with `file://` (a plain prefix, not a URL: `file://./agent.key`, `file:///etc/agent.key`, `file://~/.ssh/id_ed25519` and `file://C:\keys\agent.key` all work; there is no percent-decoding). Inline material that itself starts with `file://` cannot be passed inline — use the file form instead. `DOCKER_AGENT_ENCRYPT_KEY` accepts the same forms and is used when `--key` is not set.

```bash
# Asymmetric: sign with a private key, verify with the public key
$ docker agent share push ./agent.yaml myorg/agent:v1 --key file://~/.ssh/id_ed25519
$ docker agent share pull myorg/agent:v1 --key file://~/.ssh/id_ed25519.pub

# Symmetric: same secret on both sides
$ openssl rand -hex 32 > agent.key
$ docker agent share push ./agent.yaml myorg/agent:v1 --key file://agent.key
$ docker agent share pull myorg/agent:v1 --key file://agent.key

# Symmetric, inline
$ docker agent share push ./agent.yaml myorg/agent:v1 --key "$(openssl rand -hex 32)"
```

### Key formats

The key kind is detected from its contents:

| Key contents                                               | Kind       | Sign | Verify | `--encrypt` |
| ---------------------------------------------------------- | ---------- | ---- | ------ | ----------- |
| PEM / OpenSSH **Ed25519** private key                      | asymmetric | ✓    | ✓      | ✗           |
| PEM / OpenSSH **ECDSA** or **RSA** private key             | asymmetric | ✓    | ✓      | ✓           |
| PEM / OpenSSH public key (`.pub`)                          | asymmetric | ✗    | ✓      | ✗           |
| Anything else: a raw **secret** of at least 16 bytes       | symmetric  | ✓    | ✓      | ✓           |

Passphrase-protected keys are not supported. Anything containing a PEM boundary (`-----BEGIN`) or an OpenSSH key-type marker (`ssh-`, `ecdsa-sha2-`, `sk-ssh-`, `sk-ecdsa-`) anywhere is treated as a key and rejected if it does not parse — a broken public key is never silently used as a secret. Symmetric secrets can be guessed offline against the public YAML, so use random material (`openssl rand -hex 32`), not a password.

### Modes

- **Sign** (default): records a signature (private key) or an HMAC (secret) of the YAML. Anyone with the public key or secret can verify integrity and provenance.
- **Encrypt** (`--encrypt`): additionally records an authenticated encrypted copy of the whole YAML. Holders of the secret or private key can recover the YAML from the annotation alone, without the layer. With an asymmetric key this requires the private key and a signature is still recorded — a copy encrypted to a public key could have been produced by anyone, so it proves nothing on its own.

The pull side never needs to choose: the annotations describe what was recorded, and verification checks whatever is present. With an asymmetric key the artifact must carry a signature, which also prevents downgrading a signed artifact to an encrypted-only one.

### Verifying when running

Programs embedding Docker Agent can pass `ocisource.WithVerificationKey(key)` to `sources.Resolve` or `ocisource.New` so an OCI-sourced agent is verified on every read. Import `pkg/config/sources` and `pkg/config/ocisource` from `github.com/docker/docker-agent`.

### Limitations

Signatures cover the YAML bytes only. Re-tagging a signed artifact, or serving an older signed version under the same tag, is not detected — pin digests (`myorg/agent@sha256:…`) when that matters.

## Running from a Registry

Run agents directly from a registry without pulling first:

```bash
# Run directly from Docker Hub
$ docker agent run docker.io/username/my-agent:latest

# Docker Hub shorthand (docker.io is implied)
$ docker agent run myorg/agent:tag

# Run with a specific agent from a multi-agent config
$ docker agent run docker.io/username/dev-team:latest -a developer
```

## Using as Sub-Agents

Registry agents can be used directly as sub-agents in a multi-agent configuration — no need to define them locally:

```yaml
agents:
  root:
    model: openai/gpt-5
    description: Coordinator
    instruction: Delegate tasks to the right sub-agent.
    sub_agents:
      - myorg/agent:tag             # auto-named "agent"
      - my_reviewer:myorg/reviewer  # explicitly named "my_reviewer"
```

External sub-agents are automatically named after their last path segment. Use the `name:reference` syntax to give them a custom name.

Tag references are checked against the registry on every `docker agent run`, which adds a network round-trip per sub-agent at startup. Pin them to a digest (`myorg/agent@sha256:…`) to serve them from cache instead.

See [Pin external sub-agents to a digest](../multi-agent/index.md#pin-external-sub-agents-to-a-digest) and [External Sub-Agents](../multi-agent/index.md#external-sub-agents-from-registries) for details.

## Using with Aliases

Combine OCI references with aliases for convenient access:

```bash
# Create an alias for a registry agent
$ docker agent alias add coder myorg/coder --yolo

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

Docker Agent supports pulling from private GitHub repositories and registries that require authentication. Use standard Docker login or GitHub authentication:

```bash
# Login to a registry
$ docker login docker.io

# Now push/pull works with private repos
$ docker agent share push ./agent.yaml docker.io/myorg/private-agent:latest
$ docker agent run docker.io/myorg/private-agent:latest
```

> [!NOTE]
> **Docker authentication**
>
> When pulling or running an agent from a `docker.com` or `*.docker.com` HTTPS URL (e.g. `desktop.docker.com`), Docker Agent automatically forwards a Docker token for authentication. If Docker Desktop is running and signed in, its token is used; otherwise, Docker Agent exchanges the access token stored by `docker login` for a fresh Docker token. Either way, no explicit login step is required beyond `docker login` (or being signed into Docker Desktop).
>
> Note: `docker.io` (the standard Docker Hub registry domain) is a separate domain and is **not** covered by automatic token forwarding. Agents pulled from `docker.io` or `registry-1.docker.io` still require `docker login docker.io` for private repositories.

> [!NOTE]
> **Troubleshooting**
>
> Having issues with push/pull? See [Troubleshooting](../../community/troubleshooting/index.md) for common registry issues.

## Local Development

For local development and testing, you can run an agent directly from a local HTTP server without a registry:

```bash
# Serve an agent config locally
$ python3 -m http.server 8080

# Run it directly via HTTP
$ docker agent run http://localhost:8080/agent.yaml
$ docker agent run http://127.0.0.1:8080/agent.yaml
```

This is useful for iterating on agent configs served from a local dev server before pushing to a registry. Both `localhost` and `127.0.0.1` addresses are supported with plain `http://` URLs.

Agent configurations loaded from HTTP(S) URLs or OCI artifacts are limited to 32 MiB after decompression.
