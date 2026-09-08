package toolsets

const docsBaseURL = "https://docker.github.io/docker-agent/tools/"

type BuiltinToolsetInfo struct {
	Type    string `json:"type"`
	Summary string `json:"summary"`
	Docs    string `json:"docs"`
}

var BuiltinToolsets = []BuiltinToolsetInfo{
	builtinToolset("a2a", "a2a", "Connect to remote agents via the Agent-to-Agent (A2A) protocol"),
	builtinToolset("api", "api", "Create custom tools that call HTTP APIs"),
	builtinToolset("background_agents", "background-agents", "Dispatch work to sub-agents concurrently and collect results"),
	builtinToolset("background_jobs", "background-jobs", "Run and manage long-running shell commands"),
	builtinToolset("environment", "environment", "Report the OS and resolved shell (read-only, no arguments)"),
	builtinToolset("fetch", "fetch", "Read content from HTTP/HTTPS URLs"),
	builtinToolset("file", "file", "Read, write, and edit individual files"),
	builtinToolset("filesystem", "filesystem", "Read, write, list, search, and navigate files and directories"),
	builtinToolset("git", "git", "Read-only git repository inspection: status, log, branches, show, blame"),
	builtinToolset("lsp", "lsp", "Connect to Language Server Protocol servers for code intelligence"),
	builtinToolset("mcp", "mcp", "Extend agents with external tools via the Model Context Protocol"),
	builtinToolset("mcp_catalog", "mcp-catalog", "Discover and activate remote MCP servers from the Docker MCP Catalog"),
	builtinToolset("memory", "memory", "Persistent key-value storage for cross-session recall"),
	builtinToolset("model_picker", "model-picker", "Let the agent pick between several models per turn"),
	builtinToolset("open_url", "open-url", "Open a fixed URL in the user's default browser"),
	builtinToolset("openapi", "openapi", "Generate tools automatically from an OpenAPI specification"),
	builtinToolset("plan", "plan", "Shared persistent scratchpad for multi-agent collaboration"),
	builtinToolset("rag", "rag", "Search document knowledge bases with hybrid retrieval (RAG)"),
	builtinToolset("scheduler", "scheduler", "Schedule instructions to run at a time or on a recurring interval"),
	builtinToolset("script", "script", "Define custom shell scripts as named tools with typed parameters"),
	builtinToolset("session_context", "session_context", "Reference a previous session as context in the current one"),
	builtinToolset("shell", "shell", "Execute shell commands in the user's environment"),
	builtinToolset("tasks", "tasks", "Persistent task database with priorities and dependencies"),
	builtinToolset("think", "think", "Step-by-step reasoning scratchpad for planning"),
	builtinToolset("todo", "todo", "Manage a task list for complex multi-step workflows"),
	builtinToolset("user_prompt", "user-prompt", "Ask the user questions and collect interactive input"),
	builtinToolset("webhook", "webhook", "Send outbound notifications (Slack, Discord, Telegram, IFTTT, Teams, and more)"),
}

func builtinToolset(toolsetType, docsSlug, summary string) BuiltinToolsetInfo {
	return BuiltinToolsetInfo{
		Type:    toolsetType,
		Summary: summary,
		Docs:    docsBaseURL + docsSlug,
	}
}
