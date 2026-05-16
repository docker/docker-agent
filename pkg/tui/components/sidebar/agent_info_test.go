package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSetAgentInfo_UpdatesProviderAndModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		agentName       string
		modelID         string
		wantProvider    string
		wantModel       string
		initialProvider string
		initialModel    string
	}{
		{
			name:            "standard provider/model format",
			agentName:       "root",
			modelID:         "openai/gpt-4o",
			wantProvider:    "openai",
			wantModel:       "gpt-4o",
			initialProvider: "anthropic",
			initialModel:    "claude-sonnet-4-0",
		},
		{
			name:            "cross-provider fallback updates provider",
			agentName:       "root",
			modelID:         "anthropic/claude-sonnet-4-0",
			wantProvider:    "anthropic",
			wantModel:       "claude-sonnet-4-0",
			initialProvider: "openai",
			initialModel:    "gpt-4o",
		},
		{
			name:            "model name containing slash",
			agentName:       "root",
			modelID:         "dmr/ai/llama3.2",
			wantProvider:    "dmr",
			wantModel:       "ai/llama3.2",
			initialProvider: "openai",
			initialModel:    "gpt-4o",
		},
		{
			name:            "model name with multiple slashes",
			agentName:       "root",
			modelID:         "provider/namespace/model/version",
			wantProvider:    "provider",
			wantModel:       "namespace/model/version",
			initialProvider: "old",
			initialModel:    "old-model",
		},
		{
			name:            "model without slash keeps only model",
			agentName:       "root",
			modelID:         "gpt-4o",
			wantProvider:    "original", // Provider should not be modified
			wantModel:       "gpt-4o",
			initialProvider: "original",
			initialModel:    "old-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create a sidebar model with initial agent details
			sess := session.New()
			sessionState := service.NewSessionState(sess)
			m := New(sessionState).(*model)

			// Set up initial availableAgents
			m.availableAgents = []runtime.AgentDetails{
				{
					Name:        tt.agentName,
					Provider:    tt.initialProvider,
					Model:       tt.initialModel,
					Description: "Test agent",
				},
			}

			// Call SetAgentInfo with the new model ID
			m.SetAgentInfo(tt.agentName, tt.modelID, "Updated description")

			// Find the agent and verify
			require.Len(t, m.availableAgents, 1, "should have one agent")
			agent := m.availableAgents[0]

			assert.Equal(t, tt.wantProvider, agent.Provider, "provider should match")
			assert.Equal(t, tt.wantModel, agent.Model, "model should match")
		})
	}
}

func TestSetAgentInfo_OnlyUpdatesMatchingAgent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Set up multiple agents
	m.availableAgents = []runtime.AgentDetails{
		{Name: "agent1", Provider: "openai", Model: "gpt-4o"},
		{Name: "agent2", Provider: "anthropic", Model: "claude-sonnet-4-0"},
		{Name: "agent3", Provider: "google", Model: "gemini-2.0-flash"},
	}

	// Update only agent2
	m.SetAgentInfo("agent2", "openai/gpt-5", "New description")

	// Verify only agent2 was updated
	assert.Equal(t, "openai", m.availableAgents[0].Provider, "agent1 provider should be unchanged")
	assert.Equal(t, "gpt-4o", m.availableAgents[0].Model, "agent1 model should be unchanged")

	assert.Equal(t, "openai", m.availableAgents[1].Provider, "agent2 provider should be updated")
	assert.Equal(t, "gpt-5", m.availableAgents[1].Model, "agent2 model should be updated")

	assert.Equal(t, "google", m.availableAgents[2].Provider, "agent3 provider should be unchanged")
	assert.Equal(t, "gemini-2.0-flash", m.availableAgents[2].Model, "agent3 model should be unchanged")
}

func TestSetAgentInfo_EmptyModelIDNoUpdate(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Set up initial agent
	m.availableAgents = []runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4o"},
	}

	// Call with empty model ID
	m.SetAgentInfo("root", "", "description")

	// Verify no change to provider/model
	assert.Equal(t, "openai", m.availableAgents[0].Provider, "provider should be unchanged")
	assert.Equal(t, "gpt-4o", m.availableAgents[0].Model, "model should be unchanged")
}

func TestSetAgentInfo_NonexistentAgent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Set up agents without the target
	m.availableAgents = []runtime.AgentDetails{
		{Name: "agent1", Provider: "openai", Model: "gpt-4o"},
	}

	// Call with non-existent agent name - should not panic
	m.SetAgentInfo("nonexistent", "anthropic/claude-sonnet-4-0", "description")

	// Verify original agent unchanged
	assert.Equal(t, "openai", m.availableAgents[0].Provider)
	assert.Equal(t, "gpt-4o", m.availableAgents[0].Model)
}
