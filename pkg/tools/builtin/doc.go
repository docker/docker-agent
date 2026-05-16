// Package builtin contains the stock tool implementations shipped with
// docker-agent. Each tool lives in its own sub-package:
//
//   - filesystem — file read/write/edit/search/list/tree
//   - shell      — shell command execution, background jobs
//   - lsp        — Language Server Protocol client tools
//   - think      — structured thinking/scratchpad
//   - todo       — task list management
//   - fetch      — HTTP fetch with domain restrictions
//   - handoff    — conversation handoff between agents
//   - transfertask — task delegation to sub-agents
//   - skills     — skill runner
//   - tasks      — persistent task management
//   - memory     — persistent agent memory (sqlite)
//   - rag        — retrieval-augmented generation (sqlite)
//   - modelpicker — runtime model switching
//   - userprompt — ask the user for input
//   - api        — external API tool
//   - openapi    — OpenAPI spec-driven tool
//   - deferred   — deferred toolset wrapper
package builtin
