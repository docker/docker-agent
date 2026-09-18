// This example embeds cagent as a library and launches its TUI with a *custom
// tool renderer applied to an MCP-server tool*.
//
// It is fully self-contained: it starts an in-process MCP server (over HTTP via
// httptest, no subprocess) exposing a "search_repositories" tool with canned
// data, attaches it to the agent as a remote MCP toolset named "github" — so the
// tool surfaces as "github_search_repositories" with category "mcp" (see
// pkg/tools/mcp/mcp.go) — and registers a custom renderer for that exposed name.
// When the agent calls the MCP tool, the result is drawn as a bordered repo list
// instead of the generic tool view.
//
// The renderer is wired via tui.WithToolRenderers; the key is just the tool's
// name, so the exact same call styles built-in or Go-SDK tools too. To style
// every MCP tool at once, register under "category:mcp" instead.
//
// Run it (uses a Gemini model, so set GOOGLE_API_KEY):
//
//	GOOGLE_API_KEY=... go run ./examples/golibrary/renderer
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		log.Println(err)
	}
}

// --- the in-process MCP server -------------------------------------------------

type searchInput struct {
	Query string `json:"query" jsonschema:"the repository search query"`
}

type repo struct {
	Name        string `json:"name"`
	Stars       int    `json:"stars"`
	Description string `json:"description"`
}

type searchOutput struct {
	Repositories []repo `json:"repositories"`
}

var repoIndex = []repo{
	{"docker/cagent", 4200, "Build and run multi-agent systems from simple config"},
	{"docker/compose", 35000, "Define and run multi-container applications with Docker"},
	{"moby/moby", 69000, "The Moby Project — a framework to assemble container systems"},
	{"docker/buildx", 3700, "Docker CLI plugin for extended build capabilities with BuildKit"},
	{"kubernetes/kubernetes", 112000, "Production-Grade Container Scheduling and Management"},
}

func searchRepositories(_ context.Context, _ *gomcp.CallToolRequest, in searchInput) (*gomcp.CallToolResult, searchOutput, error) {
	// Simulate latency so the in-flight custom render ("searching…") is visible.
	time.Sleep(3 * time.Second)

	q := strings.ToLower(strings.TrimSpace(in.Query))
	hits := []repo{} // empty (not nil) so no matches marshal as "[]", not "null"
	for _, r := range repoIndex {
		if q == "" || strings.Contains(strings.ToLower(r.Name), q) || strings.Contains(strings.ToLower(r.Description), q) {
			hits = append(hits, r)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Stars > hits[j].Stars })

	// Return the results as TextContent (JSON); cagent surfaces this as the tool
	// result text, which the custom renderer parses.
	payload, _ := json.Marshal(hits)
	return &gomcp.CallToolResult{
		Content: []gomcp.Content{&gomcp.TextContent{Text: string(payload)}},
	}, searchOutput{Repositories: hits}, nil
}

// startMCPServer mounts an in-process MCP server on a local HTTP test server and
// returns it; the caller connects to its URL and closes it when done.
func startMCPServer() *httptest.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "repo-demo", Version: "0.1.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{
		Name:         "search_repositories",
		Description:  "Search GitHub-like repositories by keyword. Returns name, stars, and description.",
		Annotations:  &gomcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema:  tools.MustSchemaFor[searchInput](),
		OutputSchema: tools.MustSchemaFor[searchOutput](),
	}, searchRepositories)

	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server }, nil)
	return httptest.NewServer(handler)
}

// --- the embedded cagent app ---------------------------------------------------

func run(ctx context.Context) error {
	// cagent logs via slog; discard it so log lines don't paint over the TUI.
	// (The CLI does the same when --debug is off; pass a file handler instead to
	// capture logs.)
	slog.SetDefault(slog.New(slog.DiscardHandler))

	mcpHTTP := startMCPServer()
	defer mcpHTTP.Close()

	// Attach the in-process MCP server as a remote toolset named "github". Its
	// "search_repositories" tool is therefore exposed as "github_search_repositories".
	githubTools := mcptools.NewRemoteToolsetWithAllowPrivateIPs("github", mcpHTTP.URL, "streamable", nil, nil, true)

	llm, err := gemini.NewClient(ctx, &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-2.5-flash",
	}, environment.NewDefaultProvider())
	if err != nil {
		return err
	}

	assistant := agent.New(
		"root",
		"You help users find GitHub repositories. Whenever the user asks to find, "+
			"search, or list repositories, call the github_search_repositories tool "+
			"with a short keyword query. Always use the tool rather than answering "+
			"from memory, then summarize the top result in one sentence.",
		agent.WithModel(llm),
		agent.WithToolSets(githubTools),
	)

	t := team.New(team.WithAgents(assistant))
	defer func() { _ = t.StopToolSets(ctx) }()

	rt, err := runtime.New(ctx, t)
	if err != nil {
		return err
	}

	// Send a first message so the MCP tool is called (and the custom renderer
	// shown) on launch; auto-approve tools so the call runs without a prompt. A
	// fixed title avoids the auto-titler.
	const firstMessage = "Find docker repositories"
	sess := session.New(
		session.WithTitle("Repository search demo"),
		session.WithToolsApproved(true),
	)
	a := app.New(ctx, rt, sess, app.WithFirstMessage(firstMessage))

	// The one line that matters: a custom renderer for the MCP tool.
	wd, _ := os.Getwd()
	model := tui.New(ctx, nil, a, wd, func() {}, tui.WithToolRenderers(map[string]tool.Builder{
		"github_search_repositories": newRepoRenderer,
	}))

	p := tea.NewProgram(model, tea.WithContext(ctx))
	if m, ok := model.(interface{ SetProgram(p *tea.Program) }); ok {
		m.SetProgram(p)
	}
	_, err = p.Run()
	return err
}

// --- the custom renderer -------------------------------------------------------

func newRepoRenderer(ar *animation.Runtime, msg *types.Message, ss service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(ar, msg, ss, renderRepoSearch)
}

func renderRepoSearch(msg *types.Message, _ spinner.Spinner, _ service.SessionStateReader, width, _ int) string {
	var args searchInput
	_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)

	title := lipgloss.NewStyle().Bold(true).Foreground(styles.Accent).
		Render("⌬ GitHub repository search  ·  custom MCP renderer")
	sub := styles.MutedStyle.Render("query: ") + lipgloss.NewStyle().Bold(true).Render(args.Query)

	var body string
	switch msg.Content {
	case "":
		body = styles.MutedStyle.Render("searching…")
	default:
		var repos []repo
		switch err := json.Unmarshal([]byte(msg.Content), &repos); {
		case err != nil:
			body = msg.Content // not JSON — show raw
		case len(repos) == 0:
			body = styles.MutedStyle.Render("no repositories found")
		default:
			var b strings.Builder
			for _, r := range repos {
				stars := lipgloss.NewStyle().Foreground(styles.Warning).Render(fmt.Sprintf("★ %d", r.Stars))
				name := lipgloss.NewStyle().Bold(true).Foreground(styles.Highlight).Render(r.Name)
				fmt.Fprintf(&b, "%s  %s\n  %s\n", name, stars, styles.MutedStyle.Render(r.Description))
			}
			body = strings.TrimRight(b.String(), "\n")
		}
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(styles.Accent).
		Padding(0, 1).
		Width(max(width-4, 24))
	return box.Render(title + "\n" + sub + "\n\n" + body)
}
