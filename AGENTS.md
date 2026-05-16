# Project Agent Guidelines

This document provides guidance for AI agents and contributors working on the
docker-agent codebase.

## Code Quality Standards

- Write clean, self-documenting code with minimal comments
- Follow existing code style and patterns in the project
- Implement proper error handling and validation
- Consider edge cases and failure scenarios
- Ensure code is maintainable and extensible

### Code Comments Philosophy

Comments are only added when the code's purpose or logic is not immediately
evident. Never write comments that merely restate what the code does (e.g.
`// increment counter` above `counter++`). Comments should explain **why**
something is done a certain way, document non-obvious edge cases, or clarify
complex algorithms that cannot be simplified further.

## Working Approach

- Use tools to gather information rather than relying on assumptions
- Examine existing code before making changes
- Validate all changes before considering tasks complete
- Ask clarifying questions only when truly necessary
- When possible, call independent tools concurrently — it's faster

## Validation Requirements

Before marking work as complete:

- [ ] Code builds successfully (`task build`)
- [ ] All tests pass (`task test`)
- [ ] Linter shows no new issues (`task lint`)
- [ ] Changes meet acceptance criteria
- [ ] Code follows project patterns and conventions
- [ ] Proper error handling is implemented
- [ ] Edge cases are considered

# Development Commands

## Build and Development

- `task build` — Build the application binary (outputs to `./bin/docker-agent`)
- `task test` — Run Go tests (clears API keys to ensure deterministic tests)
- `task lint` — Run golangci-lint (uses `.golangci.yml` configuration)
- `task format` — Format code using golangci-lint fmt
- `task dev` — Run lint, test, and build in sequence

## Docker and Cross-Platform Builds

- `task build-local` — Build binary for local platform using Docker Buildx
- `task cross` — Build binaries for multiple platforms (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64)
- `task build-image` — Build Docker image tagged as `docker/docker-agent`
- `task push-image` — Build and push multi-platform Docker image to registry

## Running docker-agent

- `./bin/docker-agent run <config.yaml>` — Run agent with configuration (launches TUI by default)
- `./bin/docker-agent run <config.yaml> -a <agent_name>` — Run specific agent from multi-agent config
- `./bin/docker-agent run agentcatalog/pirate` — Run agent directly from OCI registry
- `./bin/docker-agent run --exec <config.yaml>` — Execute agent without TUI (non-interactive)
- `./bin/docker-agent new` — Generate new agent configuration interactively
- `./bin/docker-agent new --model openai/gpt-5` — Generate with specific model
- `./bin/docker-agent share push ./agent.yaml namespace/repo` — Push agent to OCI registry
- `./bin/docker-agent share pull namespace/repo` — Pull agent from OCI registry
- `./bin/docker agent serve mcp ./agent.yaml` — Expose agents as MCP tools
- `./bin/docker agent serve a2a <config.yaml>` — Start agent as A2A server
- `./bin/docker agent serve api` — Start docker-agent API server

## Debug and Development Flags

- `--debug` or `-d` — Enable debug logging (logs to `~/.cagent/cagent.debug.log`)
- `--log-file <path>` — Specify custom debug log location
- `--otel` or `-o` — Enable OpenTelemetry tracing
- Example: `./bin/docker-agent run config.yaml --debug --log-file ./debug.log`

# Testing

- Tests are located alongside source files (`*_test.go`)
- Run `task test` to execute the full test suite
- E2E tests live in the `e2e/` directory
- Test fixtures and data live in `testdata/` subdirectories
- Use `github.com/stretchr/testify/assert` and `require` for assertions
- Cover edge cases and error conditions
- Mock external dependencies for unit tests

# Agent Config YAML

- Agent config files follow a strict schema: `./agent-schema.json`
- The schema is **versioned**
- `./pkg/config/v0`, `./pkg/config/v1`, ... packages handle older versions of the config
- `./pkg/config/latest` package handles the current, work-in-progress config format
- When adding new features to the config, **only add them to the latest config**
- Older config types are **frozen** — do not modify them
- When adding new features to the config:
  - Update `./agent-schema.json` accordingly
  - Create an example YAML that demonstrates the new feature

# Git Practices

- Write clear, descriptive commit messages
- Prefer [Conventional Commits](https://www.conventionalcommits.org/) format, e.g. `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`
- Make commits logical and atomic
- Group related changes together; avoid mixing unrelated changes
- Keep branches focused on single features or fixes
- Ensure your branch is up-to-date before submitting
