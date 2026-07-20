// Package a2a provides a toolset implementation for connecting to remote A2A agents.
package a2a

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2aclient/agentcard"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/upstream"
)

// Toolset implements tools.ToolSet for A2A remote agents.
type Toolset struct {
	name            string
	url             string
	headers         map[string]string
	timeout         time.Duration
	allowPrivateIPs bool
	expander        *js.Expander
	client          *a2aclient.Client
	card            *a2a.AgentCard
	mu              sync.RWMutex
}

// Option configures a Toolset.
type Option func(*Toolset)

// WithTimeout overrides the default HTTP client timeout (see
// [httpclient.DefaultToolHTTPTimeout]) used both for fetching the agent
// card and for streaming messages.
func WithTimeout(d time.Duration) Option {
	return func(t *Toolset) { t.timeout = d }
}

// WithAllowPrivateIPs disables SSRF dial-time protection so the a2a tool
// can reach internal services. Off by default; matches the behaviour of
// the same flag on `fetch`, `api`, `openapi` and remote `mcp`.
func WithAllowPrivateIPs(allow bool) Option {
	return func(t *Toolset) { t.allowPrivateIPs = allow }
}

func WithExpander(expander *js.Expander) Option {
	return func(t *Toolset) { t.expander = expander }
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Toolset)(nil)
	_ tools.Startable    = (*Toolset)(nil)
	_ tools.Instructable = (*Toolset)(nil)
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	var opts []Option

	if toolset.Timeout > 0 {
		opts = append(opts, WithTimeout(time.Duration(toolset.Timeout)*time.Second))
	}
	if toolset.AllowPrivateIPsEnabled() {
		opts = append(opts, WithAllowPrivateIPs(true))
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())
	opts = append(opts, WithExpander(expander))

	return NewToolset(toolset.Name, toolset.URL, toolset.Headers, opts...), nil
}

// NewToolset creates a new A2A toolset for the given URL.
func NewToolset(name, url string, headers map[string]string, opts ...Option) *Toolset {
	t := &Toolset{
		name:    name,
		url:     url,
		headers: headers,
		timeout: httpclient.DefaultToolHTTPTimeout,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Instructions returns instructions for using the A2A toolset.
func (t *Toolset) Instructions() string {
	t.mu.RLock()
	card := t.card
	t.mu.RUnlock()

	if card == nil {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s\n\n%s\n", card.Name, card.Description)

	for _, skill := range card.Skills {
		fmt.Fprintf(&sb, "- **%s**: %s\n", skill.Name, skill.Description)
	}

	return sb.String()
}

// Tools returns the tools available from the remote A2A agent.
func (t *Toolset) Tools(_ context.Context) ([]tools.Tool, error) {
	t.mu.RLock()
	card := t.card
	t.mu.RUnlock()

	if card == nil {
		return nil, errors.New("A2A toolset not started")
	}

	// If skills are defined, create a tool for each skill; otherwise create one tool for the agent
	skills := card.Skills
	if len(skills) == 0 {
		skills = []a2a.AgentSkill{{ID: card.Name, Name: card.Name, Description: card.Description}}
	}

	result := make([]tools.Tool, 0, len(skills))
	for _, skill := range skills {
		name := cmp.Or(skill.ID, skill.Name)
		if t.name != "" {
			name = fmt.Sprintf("%s_%s", t.name, name)
		}
		name = sanitizeToolName(name)

		result = append(result, tools.Tool{
			Name:        name,
			Category:    "a2a",
			Description: fmt.Sprintf("Calls the '%s' skill of the %s agent. %s", skill.Name, card.Name, skill.Description),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{
						"type":        "string",
						"description": "The message or request to send to the agent",
					},
				},
				"required": []string{"message"},
			},
			Handler:     t.createHandler(),
			Annotations: tools.ToolAnnotations{Title: name},
		})
	}

	return result, nil
}

// Start connects to the A2A agent and fetches the agent card.
func (t *Toolset) Start(ctx context.Context) error {
	slog.DebugContext(ctx, "Starting A2A toolset", "url", t.url, "timeout", t.timeout, "allow_private_ips", t.allowPrivateIPs)

	// Use the SSRF-safe client to fetch the agent card so a malicious or
	// misconfigured `url:` cannot reach loopback / RFC1918 / link-local
	// addresses (cloud metadata at 169.254.169.254 in particular). The
	// `allow_private_ips: true` opt-in disables this for legitimate
	// internal-service use.
	resolver := agentcard.NewResolver(httpclient.NewSafeClient(t.timeout, t.allowPrivateIPs))
	card, err := resolver.Resolve(ctx, t.url)
	if err != nil {
		return fmt.Errorf("failed to fetch A2A agent card: %w", err)
	}

	httpClient := httpclient.NewSafeClient(t.timeout, t.allowPrivateIPs)
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}

	headers := t.expander.ExpandMap(ctx, t.headers)
	httpClient.Transport = upstream.NewHeaderTransport(base, headers)

	client, err := a2aclient.NewFromCard(
		ctx, card,
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(httpClient),
	)
	if err != nil {
		return fmt.Errorf("failed to create A2A client: %w", err)
	}

	t.mu.Lock()
	t.client = client
	t.card = card
	t.mu.Unlock()

	slog.DebugContext(ctx, "A2A toolset started", "agent", card.Name, "skills", len(card.Skills))
	return nil
}

// Stop disconnects from the A2A agent.
func (t *Toolset) Stop(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.client = nil
	t.card = nil
	return nil
}

func (t *Toolset) createHandler() tools.ToolHandler {
	return func(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		var args struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("failed to parse arguments: %w", err)
		}

		t.mu.RLock()
		client := t.client
		t.mu.RUnlock()

		if client == nil {
			return nil, errors.New("A2A client not initialized")
		}

		params := &a2a.MessageSendParams{
			Message: a2a.NewMessage(a2a.MessageRoleUser, &a2a.TextPart{Text: args.Message}),
		}

		var response strings.Builder
		for event, err := range client.SendStreamingMessage(ctx, params) {
			if err != nil {
				return nil, fmt.Errorf("A2A call failed: %w", err)
			}
			response.WriteString(extractText(event))
		}

		if response.Len() == 0 {
			return tools.ResultError("No response from agent"), nil
		}

		return tools.ResultSuccess(response.String()), nil
	}
}

func extractText(event a2a.Event) string {
	var parts a2a.ContentParts

	switch e := event.(type) {
	case *a2a.TaskStatusUpdateEvent:
		if e.Status.Message != nil {
			parts = e.Status.Message.Parts
		}
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact != nil {
			parts = e.Artifact.Parts
		}
	case *a2a.Message:
		parts = e.Parts
	case *a2a.Task:
		if e.Status.Message != nil {
			parts = e.Status.Message.Parts
		}
	}

	var sb strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *a2a.TextPart:
			sb.WriteString(p.Text)
		case a2a.TextPart:
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func sanitizeToolName(name string) string {
	result := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, name)

	result = strings.Trim(result, "_")
	for strings.Contains(result, "__") {
		result = strings.ReplaceAll(result, "__", "_")
	}

	return strings.ToLower(result)
}
