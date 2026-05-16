package team

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/permissions"
)

type Team struct {
	agents      []*agent.Agent
	permissions *permissions.Checker
}

type Opt func(*Team)

func WithAgents(agents ...*agent.Agent) Opt {
	return func(t *Team) {
		t.agents = agents
	}
}

func WithPermissions(checker *permissions.Checker) Opt {
	return func(t *Team) {
		t.permissions = checker
	}
}

func New(opts ...Opt) *Team {
	t := &Team{}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *Team) AgentNames() []string {
	var names []string
	for i := range t.agents {
		names = append(names, t.agents[i].Name())
	}
	return names
}

// AgentInfo contains information about an agent
type AgentInfo struct {
	Name        string
	Description string
	Provider    string
	Model       string
	Commands    types.Commands
}

// AgentsInfo returns information about all agents in the team
func (t *Team) AgentsInfo() []AgentInfo {
	var infos []AgentInfo
	for _, a := range t.agents {
		info := AgentInfo{
			Name:        a.Name(),
			Description: a.Description(),
			Commands:    a.Commands(),
		}
		if model := a.Model(context.TODO()); model != nil {
			id := model.ID()
			info.Provider = id.Provider
			info.Model = id.Model
		}
		infos = append(infos, info)
	}
	return infos
}

func (t *Team) DefaultAgent() (*agent.Agent, error) {
	if t.Size() == 0 {
		return nil, errors.New("no agents loaded; ensure your agent configuration defines at least one agent")
	}

	// Before v4, the default agent was the one named "root". If it exists, return it.
	for _, a := range t.agents {
		if a.Name() == "root" {
			return a, nil
		}
	}

	// Otherwise, return the first agent.
	return t.agents[0], nil
}

func (t *Team) Agent(name string) (*agent.Agent, error) {
	if t.Size() == 0 {
		return nil, errors.New("no agents loaded; ensure your agent configuration defines at least one agent")
	}

	for _, a := range t.agents {
		if a.Name() == name {
			return a, nil
		}
	}

	return nil, fmt.Errorf("agent not found: %s (available agents: %s)", name, strings.Join(t.AgentNames(), ", "))
}

// AgentOrDefault returns the agent identified by name, or the team's
// [DefaultAgent] when name is empty. It is a convenience for the many
// call sites that accept an optional agent selector (CLI flag, HTTP
// route, ...) and want "empty means whatever the team considers
// default" semantics without sprinkling the same `if name == ""` check
// everywhere.
func (t *Team) AgentOrDefault(name string) (*agent.Agent, error) {
	if name == "" {
		return t.DefaultAgent()
	}
	return t.Agent(name)
}

func (t *Team) Size() int {
	return len(t.agents)
}

func (t *Team) StopToolSets(ctx context.Context) error {
	for _, agent := range t.agents {
		if err := agent.StopToolSets(ctx); err != nil {
			return fmt.Errorf("failed to stop tool sets: %w", err)
		}
	}

	return nil
}

// Permissions returns the permission checker for this team.
// Returns nil if no permissions are configured.
func (t *Team) Permissions() *permissions.Checker {
	return t.permissions
}

// SetPermissions replaces the team's permission checker.
// This is used to merge additional permission sources (e.g. user-level global
// permissions) into the team's checker after construction.
func (t *Team) SetPermissions(checker *permissions.Checker) {
	t.permissions = checker
}
