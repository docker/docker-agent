---
title: "Installation"
description: "Get docker-agent running on your system in minutes."
permalink: /getting-started/installation/
---

# Installation

_Get docker-agent running on your system in minutes._

## Prerequisites

- An API key for at least one AI provider (OpenAI, Anthropic, Google, etc.)
- **Optional:** [Docker Desktop](https://www.docker.com/products/docker-desktop/) — for running containerized MCP tools and Docker Model Runner

## Docker Desktop (Pre-installed)

Starting with [Docker Desktop 4.63](https://docs.docker.com/desktop/release-notes/#4630), **docker-agent is already available**. No separate installation needed — just open a terminal and run:

```bash
$ docker agent version
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Tip
</div>
  <p>Docker Desktop bundles docker-agent and keeps it up to date. This is the easiest way to get started, especially if you want to use Docker MCP tools and Docker Model Runner.</p>

</div>

## Homebrew (macOS / Linux)

Install docker-agent using [Homebrew](https://brew.sh/):

```bash
# Install
$ brew install docker-agent

# Verify
$ docker-agent version
```

You can also install docker-agent as a docker CLI plugin, by copying `docker-agent` binary in `~/.docker/cli-plugins`. You can then run `docker agent version`.

## Download Binary Releases

Download [prebuilt binary releases](https://github.com/docker/docker-agent/releases) for Windows, macOS, and Linux from the GitHub Releases page.

### macOS / Linux

```bash
# Download the latest release
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; esac
curl -L "https://github.com/docker/docker-agent/releases/latest/download/docker-agent-${OS}-${ARCH}" -o docker-agent
chmod +x docker-agent
sudo mv docker-agent /usr/local/bin/
docker-agent version

# or alternatively, instead of moving to /usr/local/bin:
mkdir -p ~/.docker/cli-plugins
sudo mv docker-agent ~/.docker/cli-plugins
docker agent version
```

### Windows

Download `docker-agent-windows-amd64.exe` from the [releases page](https://github.com/docker/docker-agent/releases), rename it to `docker-agent.exe` and add it to your PATH. Alternatively you can move it to `~/.docker/cli-plugins`

## Build from Source

For the latest features, or to contribute, build from source:

### Prerequisites

- [Go 1.26](https://go.dev/dl/) or higher
- [Task](https://taskfile.dev/installation/) (build tool)
- [golangci-lint](https://golangci-lint.run/docs/welcome/install/local/) (for linting)

```bash
# Clone the repository
git clone https://github.com/docker/docker-agent.git
cd docker-agent

# Build the binary
task build

# The binary is at ./bin/docker-agent
./bin/docker-agent --help
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Building on Windows
</div>
  <p>On Windows, use <code>task build-local</code> instead of <code>task build</code>. This builds the binary inside a Docker container using Docker Buildx, which avoids issues with Windows-specific toolchain setup and CGo cross-compilation. The output goes to the <code>./dist</code> directory.</p>

</div>

## Set Up API Keys

docker-agent needs API keys for the model providers you want to use. Set them as environment variables:

```bash
# Pick one (or more) depending on your provider
export OPENAI_API_KEY="sk-..."           # OpenAI
export ANTHROPIC_API_KEY="sk-ant-..."    # Anthropic
export GOOGLE_API_KEY="AI..."            # Google Gemini (or GEMINI_API_KEY)
export MISTRAL_API_KEY="..."             # Mistral
```

See [Configuration Overview]({{ '/configuration/overview/#environment-variables' | relative_url }}) for the full list of supported providers and environment variables.

<div class="callout callout-info" markdown="1">
<div class="callout-title">Note
</div>
  <p>You only need the key(s) for the provider(s) you configure in your agent YAML. If you use Docker Model Runner (DMR), no API key is needed — models run locally.</p>

</div>

## Verify Installation

```bash
# Check the version
$ docker agent version

# Run the default agent
$ docker agent run

# Or try a built-in example
$ docker agent run agentcatalog/pirate
```

## What's Next?

<div class="cards">
  <a class="card" href="{{ '/getting-started/quickstart/' | relative_url }}">
    <div class="card-icon">⚡</div>
    <h3>Quick Start</h3>
    <p>Create and run your first agent in under 5 minutes.</p>
  </a>
  <a class="card" href="{{ '/community/troubleshooting/' | relative_url }}">
    <div class="card-icon">🔧</div>
    <h3>Troubleshooting</h3>
    <p>Something not working? Debug mode, common issues, and solutions.</p>
  </a>
</div>
