# docker-agent Examples

This directory is a curated catalog of working agent configurations. Each
example demonstrates one or more features of docker-agent and is meant to be
read as documentation as well as run as-is.

## Running an example

The canonical way to run any of them is:

```console
$ docker agent run examples/<name>.yaml
```

Add `-c <command>` to invoke one of the agent's `commands`, `--exec` to skip
the TUI, or `-a <agent>` to run a specific agent from a multi-agent file.
A handful of examples (e.g. [`tic-tac-toe.yaml`](tic-tac-toe.yaml),
[`elicitation/`](elicitation/), [`eval/`](eval/)) ship a small README of their
own with extra setup instructions — read those before running.

> Most agents need API keys for at least one provider (OpenAI, Anthropic,
> Google, Mistral, Nebius, Groq, GitHub Models, Bedrock, …). Set the matching
> environment variable (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, …) before
> running, or wire the agent to a key your gateway/proxy already holds.

The rest of this document groups examples by **what feature they showcase**.
If you are looking for inspiration to build a real agent, jump to
[Real-world agents](#real-world-agents).

---

## Getting started — minimal agents

The smallest possible configurations. Read these first to learn the YAML
shape: an `agents.root` block with at least a `model`, a `description`, and
an `instruction`.

| File | What it shows |
|------|---------------|
| [`basic_agent.yaml`](basic_agent.yaml) | The bare minimum: model + instruction, no tools. |
| [`echo-agent.yaml`](echo-agent.yaml) | Same shape with an explicit `models:` block and zero temperature. |
| [`pirate.yaml`](pirate.yaml) / [`pirate.hcl`](pirate.hcl) | Persona-only agent in YAML and the equivalent HCL form. |
| [`haiku.yaml`](haiku.yaml) | Writes haikus. |
| [`42.yaml`](42.yaml) | Douglas-Adams-style witty assistant. |
| [`contradict.yaml`](contradict.yaml) | Contrarian. |
| [`silvia.yaml`](silvia.yaml) | Sylvia-Plath-inspired poet. |
| [`mistral.yaml`](mistral.yaml) | Tiny agent pinned to a Mistral model. |
| [`pokemon.yaml`](pokemon.yaml) | French-speaking Pokémon expert with two trainer sub-agents. |
| [`alloy.yaml`](alloy.yaml) | Learning assistant. |
| [`dmr.yaml`](dmr.yaml) | Pirate served by a local Docker Model Runner endpoint. |
| [`welcome_message.yaml`](welcome_message.yaml) | Adds a `welcome_message` shown by the TUI. |
| [`instructions_from_file.hcl`](instructions_from_file.hcl) + [`instructions_from_file.md`](instructions_from_file.md) | Loads the system instruction from an external file via `${file(...)}`. |
| [`gopher.yaml`](gopher.yaml) / [`gopher.hcl`](gopher.hcl) | Same agent in YAML and HCL. |

---

## Built-in toolsets

Examples that wire up one of the toolsets shipped with docker-agent
(`filesystem`, `shell`, `todo`, `think`, `memory`, `fetch`, `script`,
`user_prompt`, `api`, `openapi`, `rag`, `model_picker`, …).

### Filesystem & shell

| File | What it shows |
|------|---------------|
| [`shell.yaml`](shell.yaml) | Plain `shell` toolset. |
| [`filesystem.yaml`](filesystem.yaml) | Plain `filesystem` toolset. |
| [`filesystem_allow_deny.yaml`](filesystem_allow_deny.yaml) | Restricting the filesystem tool with allow/deny path lists. |
| [`script_shell.yaml`](script_shell.yaml) | Defining custom shell commands as named tools via `type: script`. |
| [`pythonista.yaml`](pythonista.yaml) | Python tutor with filesystem + shell. |
| [`diag.yaml`](diag.yaml) | Log analysis using filesystem + shell + `think`. |
| [`typo.yaml`](typo.yaml) | Restricts a `filesystem` toolset to a subset of tools and exposes them as a `fix` command. |
| [`toolset-working-dir.yaml`](toolset-working-dir.yaml) | Setting a `working_dir` for MCP/LSP toolsets. |
| [`post_edit.yaml`](post_edit.yaml) | Auto-runs `gofmt -w` on every `.go` file the agent edits via `filesystem.post_edit`. |
| [`sandbox_agent.yaml`](sandbox_agent.yaml) | Same shell-based agent run with `--sandbox` to confine commands to a Docker container. |
| [`deferred.yaml`](deferred.yaml) | Wraps the agent in a deferred-execution container (`runtime: deferred`). |

### Memory, todo, think, cache

| File | What it shows |
|------|---------------|
| [`mem.yaml`](mem.yaml) | Persistent memory via the `memory` toolset (SQLite-backed). |
| [`todo.yaml`](todo.yaml) | Task manager built on the `todo` toolset. |
| [`shared-todo.yaml`](shared-todo.yaml) | Multiple agents sharing the same todo list. |
| [`cached_responses.yaml`](cached_responses.yaml) | Caching final answers in JSON so repeated questions don't hit the model. |

### Web & API access

| File | What it shows |
|------|---------------|
| [`fetch_docker.yaml`](fetch_docker.yaml) | Uses the built-in `fetch` toolset to summarize a web page. |
| [`fetch_headers.yaml`](fetch_headers.yaml) | Adding custom HTTP headers (e.g. `Authorization`) to `fetch`. |
| [`fetch_domain_filtering.yaml`](fetch_domain_filtering.yaml) | Restricting `fetch` to an allow-list of domains. |
| [`api-tool.yaml`](api-tool.yaml) | Defines a single REST endpoint as a typed tool via `type: api`. |
| [`openapi-petstore.yaml`](openapi-petstore.yaml) | Generates one tool per endpoint from a public OpenAPI spec. |
| [`bio.yaml`](bio.yaml) | Biographer driven by DuckDuckGo + fetch. |
| [`search.yaml`](search.yaml) | Web research powered by Brave + fetch. |
| [`librarian.yaml`](librarian.yaml) | Documentation researcher using context7 + DuckDuckGo + fetch. |

### RAG (Retrieval-Augmented Generation)

| File | What it shows |
|------|---------------|
| [`rag.yaml`](rag.yaml) | End-to-end example with a chunked-embeddings index over local docs. |
| [`rag/bm25.yaml`](rag/bm25.yaml) | Lexical BM25 strategy. |
| [`rag/semantic_embeddings.yaml`](rag/semantic_embeddings.yaml) | Pure semantic-embeddings strategy. |
| [`rag/hybrid.yaml`](rag/hybrid.yaml) | Hybrid BM25 + embeddings retrieval. |
| [`rag/reranking.yaml`](rag/reranking.yaml) | Hybrid retrieval with a re-ranking model on top. |
| [`rag/custom_provider.yaml`](rag/custom_provider.yaml) | Pointing the embeddings model at a custom OpenAI-compatible provider. |

---

## MCP — Model Context Protocol

docker-agent speaks MCP natively. These examples cover everything from
referencing a single Docker MCP server to wiring up the full MCP Toolkit and
remote MCP endpoints.

### Local & catalog MCP servers

| File | What it shows |
|------|---------------|
| [`apify.yaml`](apify.yaml) | Single MCP server (`docker:apify`). |
| [`airbnb.yaml`](airbnb.yaml) | Airbnb search via the openbnb MCP server. |
| [`moby.yaml`](moby.yaml) | Project expert backed by `gitmcp.io/moby/moby`. |
| [`couchbase_agent.yaml`](couchbase_agent.yaml) | Database commands through the Couchbase MCP server. |
| [`github.yaml`](github.yaml) | GitHub assistance via `docker:github-official`. |
| [`github_issue_manager.yaml`](github_issue_manager.yaml) | Same MCP, scoped to issue-management tools. |
| [`github-toon.yaml`](github-toon.yaml) | GitHub MCP with the [TOON](https://github.com/toonsim) compact tool format. |
| [`mcp_generator.yaml`](mcp_generator.yaml) | Researches and emits new MCP server configurations. |
| [`k8s_debugger.yaml`](k8s_debugger.yaml) | Kubernetes triage with the Inspektor-Gadget + kubernetes MCP servers. |
| [`mcp-toolkit.yaml`](mcp-toolkit.yaml) | Discovering and enabling servers from the Docker MCP Toolkit. |
| [`mcp-definitions.yaml`](mcp-definitions.yaml) | Top-level `mcps:` block: define a server once, reference it from many agents. |
| [`podcastgenerator_githubmodel.yaml`](podcastgenerator_githubmodel.yaml) | Multi-agent podcast pipeline using GitHub Models + DuckDuckGo + filesystem MCPs. |
| [`dhi/dhi.yaml`](dhi/dhi.yaml) | Migrates Dockerfiles to Docker Hardened Images. |

### Remote MCP & transports

| File | What it shows |
|------|---------------|
| [`notion-expert.yaml`](notion-expert.yaml) | Remote MCP server with OAuth Dynamic Client Registration. |
| [`miro-expert.yaml`](miro-expert.yaml) | Miro's hosted MCP server (`mcp.miro.com`) over streamable HTTP with OAuth 2.1 DCR, plus four inline board skills (browse / diagram / doc / table). |
| [`remote_mcp_oauth.yaml`](remote_mcp_oauth.yaml) | Remote MCP server with explicit OAuth credentials (Slack/GitHub-style). |
| [`remote_mcp_oauth_callback_redirect.yaml`](remote_mcp_oauth_callback_redirect.yaml) | OAuth flow with a public redirect URL bouncing back to localhost. |
| [`websocket_transport.yaml`](websocket_transport.yaml) | OpenAI Responses API streaming over WebSocket instead of SSE. |
| [`ha.yaml`](ha.yaml) | Home Assistant via remote MCP (streamable HTTP). |
| [`elicitation/`](elicitation/) | Demo MCP server that asks the user for structured input mid-call (forms, confirmations, enums). |

---

## Multi-agent setups

### Coordinator + sub-agents (in-process)

| File | What it shows |
|------|---------------|
| [`blog.yaml`](blog.yaml) | Technical blog pipeline (researcher → writer → reviewer). |
| [`writer.yaml`](writer.yaml) | Story writing supervisor with specialized sub-agents. |
| [`finance.yaml`](finance.yaml) | Financial research orchestrating analysts. |
| [`background_agents.yaml`](background_agents.yaml) | Parallel research delegated to background sub-agents. |
| [`coding_harnesses.yaml`](coding_harnesses.yaml) | Orchestrator delegating coding tasks to external harness-backed sub-agents. |
| [`coding_harness_background_agents.yaml`](coding_harness_background_agents.yaml) | Orchestrator running external coding harnesses concurrently via background agents. |
| [`dev-team.yaml`](dev-team.yaml) | Product-manager-led team (designer + engineer) with shared memory. |
| [`multi-code.yaml`](multi-code.yaml) | Tech-lead routing tasks to a frontend and a Go expert. |
| [`coder.yaml`](coder.yaml) | Coding agent with planner, implementer, and librarian sub-agents. |
| [`pr-reviewer-bedrock.yaml`](pr-reviewer-bedrock.yaml) | PR review toolkit pinned to Bedrock models. |
| [`professional/professional_writing_agent.yaml`](professional/professional_writing_agent.yaml) | English editor + French translator team. |
| [`handoff.yaml`](handoff.yaml) | Strict handoff (`type: handoff`) between specialists. |

### Catalog sub-agents

| File | What it shows |
|------|---------------|
| [`sub-agents-from-catalog.yaml`](sub-agents-from-catalog.yaml) | Mixes locally-defined sub-agents with ones pulled from an OCI catalog (e.g. `agentcatalog/pirate`). |

### Inter-agent protocols (A2A & MCP)

| File | What it shows |
|------|---------------|
| [`tic-tac-toe.yaml`](tic-tac-toe.yaml) | Game-master + two players communicating over **A2A**. |
| [`tic-tac-toe-mcp.yaml`](tic-tac-toe-mcp.yaml) | Same demo, players exposed as **MCP** servers instead. |

---

## Models, providers & routing

| File | What it shows |
|------|---------------|
| [`custom_provider.yaml`](custom_provider.yaml) | Talking to any OpenAI-compatible endpoint via a custom provider. |
| [`compose-secrets.yaml`](compose-secrets.yaml) | Reading API keys from Docker Compose / Swarm secrets. |
| [`env_placeholders.yaml`](env_placeholders.yaml) | `${ENV_VAR}` substitution inside the YAML. |
| [`nebius.yaml`](nebius.yaml) | Nebius cloud provider. |
| [`grok.yaml`](grok.yaml) | xAI Grok model. |
| [`github-copilot.yaml`](github-copilot.yaml) | GitHub Copilot models via OAuth device-flow. |
| [`fallback_models.yaml`](fallback_models.yaml) | Automatic fallback to a secondary model when the primary fails. |
| [`model_picker.yaml`](model_picker.yaml) | Lets the agent itself swap to a stronger model mid-conversation. |
| [`per_tool_model_routing.yaml`](per_tool_model_routing.yaml) | Use a cheap/fast model to interpret tool results, the primary model for reasoning. |
| [`rule_based_routing.yaml`](rule_based_routing.yaml) | Cheap router model dispatches the user message to fast or capable models. |
| [`structured-output.yaml`](structured-output.yaml) | Forces the model to return JSON matching a schema. |
| [`google_search_grounding.yaml`](google_search_grounding.yaml) | Enables Google Search grounding on Gemini models. |
| [`sampling-opts.yaml`](sampling-opts.yaml) | Provider-specific sampling parameters (`top_k`, `repetition_penalty`, …). |
| [`thinking_budget.yaml`](thinking_budget.yaml) | Reasoning/thinking budgets across OpenAI, Anthropic and Google. |
| [`task_budget.yaml`](task_budget.yaml) | Anthropic `task_budget`: cap total tokens spent across a multi-step agentic task. |

---

## Permissions, redaction & sandboxing

| File | What it shows |
|------|---------------|
| [`permissions.yaml`](permissions.yaml) | Top-level `permissions` block with `allow`/`deny` patterns for tool calls. |
| [`llm_judge.yaml`](llm_judge.yaml) | Layered defense: deterministic permissions + an LLM-as-judge `pre_tool_use` hook + user prompts. |
| [`redact_secrets.yaml`](redact_secrets.yaml) | Single-flag (`redact_secrets: true`) scrubbing of detected secrets in args, chat content, and tool output. |
| [`redact_secrets_hooks.yaml`](redact_secrets_hooks.yaml) | The same scrubbing wired manually as three hooks. |
| [`sandbox_agent.yaml`](sandbox_agent.yaml) | Running tools inside a Docker sandbox via `docker agent run --sandbox`. |
| [`filesystem_allow_deny.yaml`](filesystem_allow_deny.yaml) | Path-level allow/deny for the filesystem toolset. |

---

## Hooks & toolset lifecycle

| File | What it shows |
|------|---------------|
| [`hooks.yaml`](hooks.yaml) | Comprehensive tour of every hook event (`pre_tool_use`, `post_tool_use`, `before_llm_call`, `after_llm_call`, `transform`, `notification`, `session_*`, …). |
| [`unload_on_switch.yaml`](unload_on_switch.yaml) | `on_agent_switch` builtin (`command: unload`) that unloads the previous agent's models (DMR `_unload`) so two heavy local models can share a single GPU. |
| [`lifecycle.yaml`](lifecycle.yaml) | Per-toolset lifecycle policies (`strict`, `resilient`, `best-effort`) and restart/back-off tuning. |
| [`post_edit.yaml`](post_edit.yaml) | Filesystem `post_edit` actions running after every file edit. |

---

## Code-mode, skills & runtime knobs

| File | What it shows |
|------|---------------|
| [`code_mode.yaml`](code_mode.yaml) | `code_mode_tools: true` lets the model script multi-step tool calls in code instead of one-shot calls. |
| [`skills_filter.yaml`](skills_filter.yaml) | Restricts which discovered skills the agent is allowed to expose. |
| [`toolset_instructions.yaml`](toolset_instructions.yaml) | Augmenting a toolset's default instructions with `{ORIGINAL_INSTRUCTIONS}`. |

---

## Real-world agents

Larger, opinionated agents you can use as starting points for your own
projects.

| File | What it does |
|------|--------------|
| [`code.yaml`](code.yaml) | Code analysis + modification + validation loop. |
| [`coder.yaml`](coder.yaml) | Coding agent with planner and librarian sub-agents. |
| [`multi-code.yaml`](multi-code.yaml) | Tech-lead routing between web and Go specialists. |
| [`gopher.yaml`](gopher.yaml) / [`gopher.hcl`](gopher.hcl) | Go-specialist coding agent. |
| [`go_packages.yaml`](go_packages.yaml) | Expert in the Go module ecosystem. |
| [`modernize-go-tests.yaml`](modernize-go-tests.yaml) | Mass-modernizes Go test suites to recent Go idioms. |
| [`pr-reviewer-bedrock.yaml`](pr-reviewer-bedrock.yaml) | PR reviewer pinned to Bedrock. |
| [`review.yaml`](review.yaml) | Dockerfile review specialist. |
| [`doc_generator.yaml`](doc_generator.yaml) | Generates documentation from a codebase. |
| [`image_text_extractor.yaml`](image_text_extractor.yaml) | OCR-ish extractor for text in images. |
| [`dhi/dhi.yaml`](dhi/dhi.yaml) | Migrates Dockerfiles to Docker Hardened Images. |
| [`k8s_debugger.yaml`](k8s_debugger.yaml) | Kubernetes incident triage. |

---

## Evaluation

| File | What it shows |
|------|---------------|
| [`eval/`](eval/) | Saved sessions plus the agent under test, runnable with `docker agent eval demo.yaml ./evals`. |

---

## Embedding docker-agent in Go programs

Code samples that import the `docker-agent` Go packages directly.

| Path | What it shows |
|------|---------------|
| [`golibrary/simple/`](golibrary/simple/) | Single agent, single message. |
| [`golibrary/multi/`](golibrary/multi/) | Two agents with a `transfer_task` hand-off. |
| [`golibrary/tool/`](golibrary/tool/) | Registering a custom Go-defined tool. |
| [`golibrary/builtintool/`](golibrary/builtintool/) | Wiring up a built-in toolset (`shell`) from Go. |
| [`golibrary/stream/`](golibrary/stream/) | Consuming the runtime's streaming events (`RunStream`). |
| [`chat/`](chat/) | Tiny OpenAI-SDK client that talks to `docker agent serve chat` — proves any OpenAI-compatible client works against a docker-agent. |

---

## Conventions

- **YAML** is the recommended format. **HCL** is also supported (`*.hcl`); the
  `pirate.{yaml,hcl}`, `gopher.{yaml,hcl}` and `instructions_from_file.hcl`
  pairs show equivalent agents in both formats.
- Every example references the JSON schema at
  [`agent-schema.json`](../agent-schema.json) — point your editor at it for
  autocomplete and validation.
- Examples target the **latest** schema version. Older config versions live
  under `pkg/config/v0`, `v1`, … and are intentionally frozen; new features
  only land on the latest version.
