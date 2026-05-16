package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSidebar_HandleClickType_Agent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sessionState.SetCurrentAgentName("agent1")
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "agent1"
	m.availableAgents = []runtime.AgentDetails{
		{Name: "agent1", Provider: "openai", Model: "gpt-4", Description: "First agent"},
		{Name: "agent2", Provider: "anthropic", Model: "claude", Description: "Second agent"},
	}
	m.width = 40
	m.height = 50

	// Force a render to populate agentClickZones
	_ = sb.View()

	paddingLeft := m.layoutCfg.PaddingLeft

	// Verify clicking on agent1 lines returns ClickAgent with "agent1"
	foundAgent1 := false
	foundAgent2 := false
	for y := range len(m.cachedLines) {
		result, agentName := sb.HandleClickType(paddingLeft+2, y)
		if result == ClickAgent {
			if agentName == "agent1" {
				foundAgent1 = true
			}
			if agentName == "agent2" {
				foundAgent2 = true
			}
		}
	}
	assert.True(t, foundAgent1, "should be able to click on agent1")
	assert.True(t, foundAgent2, "should be able to click on agent2")
}
