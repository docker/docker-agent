package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
)

func TestBuildTaskSystemMessage(t *testing.T) {
	t.Run("with expected output", func(t *testing.T) {
		msg := buildTaskSystemMessage("do the thing", "a result", nil)
		assert.Contains(t, msg, "<task>\ndo the thing\n</task>")
		assert.Contains(t, msg, "<expected_output>\na result\n</expected_output>")
		assert.NotContains(t, msg, "<attached_files>")
	})

	t.Run("without expected output", func(t *testing.T) {
		msg := buildTaskSystemMessage("do the thing", "", nil)
		assert.Contains(t, msg, "<task>\ndo the thing\n</task>")
		assert.NotContains(t, msg, "expected_output")
		assert.NotContains(t, msg, "<attached_files>")
	})

	t.Run("with attached files", func(t *testing.T) {
		msg := buildTaskSystemMessage("do the thing", "", []string{"/abs/foo.go", "/abs/bar.go"})
		assert.Contains(t, msg, "<task>\ndo the thing\n</task>")
		assert.Contains(t, msg, "<attached_files>\n- /abs/foo.go\n- /abs/bar.go\n</attached_files>")
	})
}

func TestAgentNames(t *testing.T) {
	agents := []*agent.Agent{
		agent.New("alpha", ""),
		agent.New("beta", ""),
	}
	assert.Equal(t, []string{"alpha", "beta"}, agentNames(agents))
	assert.Empty(t, agentNames(nil))
}

func TestValidateAgentInList(t *testing.T) {
	agents := []*agent.Agent{
		agent.New("sub1", ""),
		agent.New("sub2", ""),
	}

	t.Run("valid agent returns nil", func(t *testing.T) {
		result := validateAgentInList("root", "sub1", "transfer to", "sub-agents", agents)
		assert.Nil(t, result)
	})

	t.Run("invalid agent with non-empty list", func(t *testing.T) {
		result := validateAgentInList("root", "missing", "transfer to", "sub-agents", agents)
		require.NotNil(t, result)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "sub1")
		assert.Contains(t, result.Output, "sub2")
	})

	t.Run("invalid agent with empty list", func(t *testing.T) {
		result := validateAgentInList("root", "missing", "transfer to", "sub-agents", nil)
		require.NotNil(t, result)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "No agents are configured")
	})
}

func TestNewSubSession(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "a worker agent",
		agent.WithMaxIterations(10),
	)

	t.Run("basic config", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:           "write tests",
			ExpectedOutput: "passing tests",
			AgentName:      "worker",
			Title:          "Test task",
			ToolsApproved:  true,
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.Equal(t, parent.ID, s.ParentID)
		assert.Equal(t, "Test task", s.Title)
		assert.True(t, s.ToolsApproved)
		assert.False(t, s.SendUserMessage)
		assert.Equal(t, 10, s.MaxIterations)
		// AgentName should NOT be set when PinAgent is false
		assert.Empty(t, s.AgentName)
	})

	t.Run("pin agent", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:      "background work",
			AgentName: "worker",
			Title:     "Background task",
			PinAgent:  true,
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.Equal(t, "worker", s.AgentName)
	})

	t.Run("custom implicit user message", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:                "bump deps",
			AgentName:           "worker",
			Title:               "Skill task",
			ImplicitUserMessage: "Update all Go dependencies",
		}

		s := newSubSession(parent, cfg, childAgent)

		// The implicit user message should be the custom one, not "Please proceed."
		assert.Equal(t, "Update all Go dependencies", s.GetLastUserMessageContent())
	})

	t.Run("default implicit user message", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:      "do work",
			AgentName: "worker",
			Title:     "Task",
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.Equal(t, "Please proceed.", s.GetLastUserMessageContent())
	})

	t.Run("custom system message", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:          "bump deps",
			SystemMessage: "You are a skill sub-agent. Follow these instructions.",
			AgentName:     "worker",
			Title:         "Skill task",
		}

		s := newSubSession(parent, cfg, childAgent)

		// When SystemMessage is set, the default task-based message should not be used.
		// We can verify the user message is still the default.
		assert.Equal(t, "Please proceed.", s.GetLastUserMessageContent())
	})
}

func TestSubSessionConfig_DefaultValues(t *testing.T) {
	// Verify zero-value SubSessionConfig produces a valid session
	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "")

	cfg := SubSessionConfig{
		Task:      "minimal task",
		AgentName: "worker",
		Title:     "Minimal",
	}

	s := newSubSession(parent, cfg, childAgent)

	assert.False(t, s.ToolsApproved)
	assert.False(t, s.SendUserMessage)
	assert.Empty(t, s.AgentName)
}

func TestSubSessionConfig_InheritsAgentLimits(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))

	t.Run("with custom limits", func(t *testing.T) {
		childAgent := agent.New("worker", "",
			agent.WithMaxIterations(42),
			agent.WithMaxConsecutiveToolCalls(7),
		)

		cfg := SubSessionConfig{
			Task:      "work",
			AgentName: "worker",
			Title:     "test",
		}

		s := newSubSession(parent, cfg, childAgent)
		assert.Equal(t, 42, s.MaxIterations)
		assert.Equal(t, 7, s.MaxConsecutiveToolCalls)
	})

	t.Run("with zero limits (defaults)", func(t *testing.T) {
		childAgent := agent.New("worker", "")

		cfg := SubSessionConfig{
			Task:      "work",
			AgentName: "worker",
			Title:     "test",
		}

		s := newSubSession(parent, cfg, childAgent)
		assert.Equal(t, 0, s.MaxIterations)
		assert.Equal(t, 0, s.MaxConsecutiveToolCalls)
	})
}

func TestSubSessionInheritsAttachedFiles(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))
	parent.AddAttachedFile("/abs/foo.go")
	parent.AddAttachedFile("/abs/bar.go")
	parent.AddAttachedFile("/abs/foo.go") // duplicate, should be ignored

	childAgent := agent.New("worker", "")
	cfg := SubSessionConfig{
		Task:      "refactor",
		AgentName: "worker",
		Title:     "Refactor",
	}

	s := newSubSession(parent, cfg, childAgent)

	// Child session inherits parent's attached files (deduplicated, ordered).
	assert.Equal(t, []string{"/abs/foo.go", "/abs/bar.go"}, s.AttachedFilesSnapshot())

	// The system message lists them so the sub-agent sees them up-front.
	sysMsg := s.GetMessages(childAgent)
	require.NotEmpty(t, sysMsg)
	var joined strings.Builder
	for _, m := range sysMsg {
		joined.WriteString(m.Content)
		joined.WriteString("\n")
	}
	assert.Contains(t, joined.String(), "<attached_files>\n- /abs/foo.go\n- /abs/bar.go\n</attached_files>")
}

func TestSubSessionWithoutAttachedFilesOmitsBlock(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "")
	cfg := SubSessionConfig{
		Task:      "refactor",
		AgentName: "worker",
		Title:     "Refactor",
	}

	s := newSubSession(parent, cfg, childAgent)
	assert.Empty(t, s.AttachedFilesSnapshot())

	msgs := s.GetMessages(childAgent)
	require.NotEmpty(t, msgs)
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "<attached_files>")
	}
}
