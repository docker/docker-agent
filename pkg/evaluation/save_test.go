package evaluation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSaveWithCustomFilename(t *testing.T) {
	// Create a temporary directory and change to it
	t.Chdir(t.TempDir())

	// Create a test session
	sess := session.New()
	sess.ID = "test-session-id"

	// Test 1: Save with custom filename
	evalFile, err := Save(sess, "my-custom-eval")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("evals", "my-custom-eval.json"), evalFile)
	require.FileExists(t, evalFile)

	// Verify the saved file contains the evals field
	data, err := os.ReadFile(evalFile)
	require.NoError(t, err)
	var savedSession session.Session
	err = json.Unmarshal(data, &savedSession)
	require.NoError(t, err)
	assert.NotNil(t, savedSession.Evals)
	assert.Empty(t, savedSession.Evals.Relevance)

	// Test 2: Save without filename (should use session ID)
	evalFile2, err := Save(sess, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("evals", sess.ID+".json"), evalFile2)
	require.FileExists(t, evalFile2)

	// Test 3: Save with same filename (should add _1 suffix)
	evalFile3, err := Save(sess, "my-custom-eval")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("evals", "my-custom-eval_1.json"), evalFile3)
	require.FileExists(t, evalFile3)
}

func TestSaveRunSessions(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	outputDir := t.TempDir()

	// Create an eval run with sessions
	run := &EvalRun{
		Name:      "test-eval-001",
		Timestamp: time.Now(),
		Results: []Result{
			{
				Title:    "eval-test-1",
				Question: "What is the capital of France?",
				Response: "Paris is the capital of France.",
				Session: session.New(
					session.WithTitle("eval-test-1"),
					session.WithUserMessage("What is the capital of France?"),
				),
			},
			{
				Title:    "eval-test-2",
				Question: "What is 2+2?",
				Response: "4",
				Session: session.New(
					session.WithTitle("eval-test-2"),
					session.WithUserMessage("What is 2+2?"),
				),
			},
			{
				// Result without a session (error case)
				Title:   "eval-test-3",
				Error:   "container failed",
				Session: nil,
			},
		},
	}

	// Save sessions to database
	dbPath, err := SaveRunSessions(ctx, run, outputDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(outputDir, "test-eval-001.db"), dbPath)
	assert.FileExists(t, dbPath)

	// Verify we can read sessions back from the database
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer func() {
		if closer, ok := store.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	// Get all sessions
	sessions, err := store.GetSessions(ctx)
	require.NoError(t, err)
	assert.Len(t, sessions, 2, "should have 2 sessions (excluding the error case)")

	// Verify session content
	titles := make(map[string]bool)
	for _, sess := range sessions {
		titles[sess.Title] = true
	}
	assert.True(t, titles["eval-test-1"], "should have eval-test-1")
	assert.True(t, titles["eval-test-2"], "should have eval-test-2")
}

func TestSaveRunSessionsJSON(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()

	// Create sessions with different content
	sess1 := session.New(
		session.WithTitle("eval-json-1"),
		session.WithUserMessage("What is the capital of France?"),
	)
	sess1.InputTokens = 100
	sess1.OutputTokens = 50
	sess1.Cost = 0.01
	sess1.Evals = &session.EvalCriteria{
		Relevance: []string{"mentions Paris", "mentions France"},
	}

	sess2 := session.New(
		session.WithTitle("eval-json-2"),
		session.WithUserMessage("What is 2+2?"),
	)
	sess2.InputTokens = 80
	sess2.OutputTokens = 30
	sess2.Cost = 0.005
	sess2.Evals = &session.EvalCriteria{
		Relevance: []string{"gives the correct answer", "explains the math"},
	}

	// Create an eval run with sessions and eval criteria
	run := &EvalRun{
		Name:      "test-json-001",
		Timestamp: time.Now(),
		Duration:  42 * time.Second,
		Config: Config{
			AgentFilename: "./test-agent.yaml",
			JudgeModel:    "anthropic/claude-opus-4-5",
			Concurrency:   4,
			EvalsDir:      "./evals",
		},
		Summary: Summary{
			TotalEvals:  3,
			FailedEvals: 1,
			TotalCost:   0.015,
		},
		Results: []Result{
			{
				Title:             "eval-json-1",
				Question:          "What is the capital of France?",
				Response:          "Paris is the capital of France.",
				Cost:              0.01,
				OutputTokens:      50,
				RelevancePassed:   2,
				RelevanceExpected: 2,
				RelevanceResults: []RelevanceResult{
					{Criterion: "mentions Paris", Passed: true, Reason: "response includes Paris"},
					{Criterion: "mentions France", Passed: true, Reason: "response includes France"},
				},
				Session: sess1,
			},
			{
				Title:             "eval-json-2",
				Question:          "What is 2+2?",
				Response:          "4",
				Cost:              0.005,
				OutputTokens:      30,
				RelevancePassed:   1,
				RelevanceExpected: 2,
				RelevanceResults: []RelevanceResult{
					{Criterion: "gives the correct answer", Passed: true, Reason: "the response says 4"},
					{Criterion: "explains the math", Passed: false, Reason: "no explanation given"},
				},
				Session: sess2,
			},
			{
				// Result without a session (error case)
				Title:   "eval-json-3",
				Error:   "container failed",
				Session: nil,
			},
		},
	}

	// Save sessions to JSON
	sessionsPath, err := SaveRunSessionsJSON(run, outputDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(outputDir, "test-json-001.json"), sessionsPath)
	assert.FileExists(t, sessionsPath)

	// Read and parse the JSON file
	data, err := os.ReadFile(sessionsPath)
	require.NoError(t, err)

	var output RunOutput
	err = json.Unmarshal(data, &output)
	require.NoError(t, err)

	// Verify run-level metadata
	assert.Equal(t, "test-json-001", output.Name)
	assert.Equal(t, "42s", output.Duration)
	assert.Equal(t, "./test-agent.yaml", output.Config.Agent)
	assert.Equal(t, "anthropic/claude-opus-4-5", output.Config.JudgeModel)
	assert.Equal(t, 4, output.Config.Concurrency)
	assert.Equal(t, "./evals", output.Config.EvalsDir)

	// Verify summary
	assert.Equal(t, 3, output.Summary.TotalEvals)
	assert.Equal(t, 1, output.Summary.FailedEvals)
	assert.InDelta(t, 0.015, output.Summary.TotalCost, 0.0001)

	// Should have 2 sessions (excluding the error case)
	assert.Len(t, output.Sessions, 2)

	// Verify session content
	titles := make(map[string]*session.Session)
	for _, sess := range output.Sessions {
		titles[sess.Title] = sess
	}

	assert.Contains(t, titles, "eval-json-1")
	assert.Contains(t, titles, "eval-json-2")

	// Verify cost and token data is preserved
	sess1Loaded := titles["eval-json-1"]
	assert.Equal(t, int64(100), sess1Loaded.InputTokens)
	assert.Equal(t, int64(50), sess1Loaded.OutputTokens)
	assert.InDelta(t, 0.01, sess1Loaded.Cost, 0.0001)

	// Verify eval results are populated
	require.NotNil(t, sess1Loaded.EvalResult)
	assert.True(t, sess1Loaded.EvalResult.Passed)
	assert.NotEmpty(t, sess1Loaded.EvalResult.Successes)
	assert.Empty(t, sess1Loaded.EvalResult.Failures)
	assert.InDelta(t, 0.01, sess1Loaded.EvalResult.Cost, 0.0001)
	assert.Equal(t, int64(50), sess1Loaded.EvalResult.OutputTokens)

	// Verify structured relevance check
	require.NotNil(t, sess1Loaded.EvalResult.Checks.Relevance)
	assert.True(t, sess1Loaded.EvalResult.Checks.Relevance.Passed)
	assert.InDelta(t, 2, sess1Loaded.EvalResult.Checks.Relevance.PassedCount, 0.01)
	assert.InDelta(t, 2, sess1Loaded.EvalResult.Checks.Relevance.Total, 0.01)

	// No size or tool calls checks were configured
	assert.Nil(t, sess1Loaded.EvalResult.Checks.Size)
	assert.Nil(t, sess1Loaded.EvalResult.Checks.ToolCalls)

	sess2Loaded := titles["eval-json-2"]
	assert.Equal(t, int64(80), sess2Loaded.InputTokens)
	assert.Equal(t, int64(30), sess2Loaded.OutputTokens)
	assert.InDelta(t, 0.005, sess2Loaded.Cost, 0.0001)

	// Verify failed eval result
	require.NotNil(t, sess2Loaded.EvalResult)
	assert.False(t, sess2Loaded.EvalResult.Passed)
	assert.NotEmpty(t, sess2Loaded.EvalResult.Failures)

	// Verify structured relevance check with per-criterion results
	require.NotNil(t, sess2Loaded.EvalResult.Checks.Relevance)
	assert.False(t, sess2Loaded.EvalResult.Checks.Relevance.Passed)
	assert.InDelta(t, 1, sess2Loaded.EvalResult.Checks.Relevance.PassedCount, 0.01)
	assert.InDelta(t, 2, sess2Loaded.EvalResult.Checks.Relevance.Total, 0.01)
	require.Len(t, sess2Loaded.EvalResult.Checks.Relevance.Results, 2)

	// First criterion should be passed with reason
	assert.True(t, sess2Loaded.EvalResult.Checks.Relevance.Results[0].Passed)
	assert.Equal(t, "the response says 4", sess2Loaded.EvalResult.Checks.Relevance.Results[0].Reason)

	// Second criterion should be failed with reason
	assert.False(t, sess2Loaded.EvalResult.Checks.Relevance.Results[1].Passed)
	assert.Equal(t, "explains the math", sess2Loaded.EvalResult.Checks.Relevance.Results[1].Criterion)
	assert.Equal(t, "no explanation given", sess2Loaded.EvalResult.Checks.Relevance.Results[1].Reason)
}

func TestSaveRunSessionsJSONContainerRuntime(t *testing.T) {
	t.Parallel()

	save := func(t *testing.T, cfg Config) []byte {
		t.Helper()
		run := &EvalRun{
			Name:      "test-runtime-001",
			Timestamp: time.Now(),
			Config:    cfg,
		}

		sessionsPath, err := SaveRunSessionsJSON(run, t.TempDir())
		require.NoError(t, err)

		data, err := os.ReadFile(sessionsPath)
		require.NoError(t, err)
		return data
	}

	t.Run("recorded when configured", func(t *testing.T) {
		t.Parallel()

		data := save(t, Config{ContainerRuntime: "podman"})

		var output RunOutput
		require.NoError(t, json.Unmarshal(data, &output))
		assert.Equal(t, "podman", output.Config.ContainerRuntime)
	})

	t.Run("omitted when empty", func(t *testing.T) {
		t.Parallel()

		data := save(t, Config{})

		var raw struct {
			Config map[string]any `json:"config"`
		}
		require.NoError(t, json.Unmarshal(data, &raw))
		assert.NotContains(t, raw.Config, "container_runtime")
	})
}

// TestSaveRunSessionsJSONAgentImage covers the RunOutputConfig.AgentImage
// cases: it records the resolved image (not the raw Config.AgentImage), and
// unlike sibling fields it is never omitted, even when empty, so an explicit
// --agent-image none (skip injection) is distinguishable from a run
// predating this field.
func TestSaveRunSessionsJSONAgentImage(t *testing.T) {
	t.Parallel()

	withVersion(t, "v1.133.0")

	save := func(t *testing.T, cfg Config) []byte {
		t.Helper()
		run := &EvalRun{
			Name:      "test-agent-image-001",
			Timestamp: time.Now(),
			Config:    cfg,
		}

		sessionsPath, err := SaveRunSessionsJSON(run, t.TempDir())
		require.NoError(t, err)

		data, err := os.ReadFile(sessionsPath)
		require.NoError(t, err)
		return data
	}

	t.Run("records the version-derived default", func(t *testing.T) {
		t.Parallel()

		data := save(t, Config{})

		var output RunOutput
		require.NoError(t, json.Unmarshal(data, &output))
		assert.Equal(t, "docker/docker-agent:1.133.0", output.Config.AgentImage)
	})

	t.Run("records an explicit override", func(t *testing.T) {
		t.Parallel()

		data := save(t, Config{AgentImage: "docker/docker-agent:1.100.0"})

		var output RunOutput
		require.NoError(t, json.Unmarshal(data, &output))
		assert.Equal(t, "docker/docker-agent:1.100.0", output.Config.AgentImage)
	})

	t.Run("present but empty when injection is skipped", func(t *testing.T) {
		t.Parallel()

		data := save(t, Config{AgentImage: NoAgentImage})

		var raw struct {
			Config map[string]any `json:"config"`
		}
		require.NoError(t, json.Unmarshal(data, &raw))
		require.Contains(t, raw.Config, "agent_image")
		assert.Empty(t, raw.Config["agent_image"])
	})
}

func TestSaveRunSessionsWithCost(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	outputDir := t.TempDir()

	// Create a session with cost data
	sess := session.New(
		session.WithTitle("cost-test"),
		session.WithUserMessage("test question"),
	)
	sess.InputTokens = 500
	sess.OutputTokens = 200
	sess.Cost = 0.0125

	run := &EvalRun{
		Name:      "test-cost-001",
		Timestamp: time.Now(),
		Results: []Result{
			{
				Title:    "cost-test",
				Question: "test question",
				Response: "test response",
				Session:  sess,
			},
		},
	}

	// Save sessions to database
	dbPath, err := SaveRunSessions(ctx, run, outputDir)
	require.NoError(t, err)

	// Verify we can read sessions back with cost preserved
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer func() {
		if closer, ok := store.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	sessions, err := store.GetSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	loadedSess := sessions[0]
	assert.Equal(t, int64(500), loadedSess.InputTokens)
	assert.Equal(t, int64(200), loadedSess.OutputTokens)
	assert.InDelta(t, 0.0125, loadedSess.Cost, 0.0001, "cost should be preserved")
}

func TestSessionFromEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		events       []map[string]any
		title        string
		question     string
		wantMessages int
		wantContent  string
	}{
		{
			name:         "empty events",
			events:       []map[string]any{},
			title:        "test",
			question:     "hello",
			wantMessages: 1, // just the user message
			wantContent:  "",
		},
		{
			name: "agent choice events",
			events: []map[string]any{
				{"type": "agent_choice", "content": "Hello ", "agent_name": "root"},
				{"type": "agent_choice", "content": "world!"},
				{"type": "stream_stopped"},
			},
			title:        "test",
			question:     "greet me",
			wantMessages: 2, // user + assistant
			wantContent:  "Hello world!",
		},
		{
			name: "tool calls and responses",
			events: []map[string]any{
				{"type": "agent_choice", "content": "Let me help.", "agent_name": "root"},
				{
					"type": "tool_call",
					"tool_call": map[string]any{
						"id":   "call_123",
						"type": "function",
						"function": map[string]any{
							"name":      "read_file",
							"arguments": `{"path": "test.txt"}`,
						},
					},
				},
				{
					"type":         "tool_call_response",
					"tool_call_id": "call_123",
					"response":     "file content",
				},
				{"type": "agent_choice", "content": "Done!"},
				{"type": "stream_stopped"},
			},
			title:        "test",
			question:     "read file",
			wantMessages: 4, // user + assistant (with tool call) + tool response + assistant
			wantContent:  "Done!",
		},
		{
			name: "token usage updates session",
			events: []map[string]any{
				{"type": "agent_choice", "content": "Answer"},
				{
					"type": "token_usage",
					"usage": map[string]any{
						"input_tokens":  float64(100),
						"output_tokens": float64(50),
						"cost":          0.005,
					},
				},
				{"type": "stream_stopped"},
			},
			title:        "test",
			question:     "question",
			wantMessages: 2,
			wantContent:  "Answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sess := SessionFromEvents(tt.events, tt.title, []string{tt.question})

			assert.Equal(t, tt.title, sess.Title)
			assert.Len(t, sess.Messages, tt.wantMessages)

			// Check first message is user message
			if tt.question != "" {
				assert.Equal(t, chat.MessageRoleUser, sess.Messages[0].Message.Message.Role)
				assert.Equal(t, tt.question, sess.Messages[0].Message.Message.Content)
			}

			// Check last assistant message content if expected
			if tt.wantContent != "" {
				lastContent := sess.GetLastAssistantMessageContent()
				assert.Equal(t, tt.wantContent, lastContent)
			}
		})
	}
}

func TestSessionFromEventsTokenUsage(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "agent_choice", "content": "Answer"},
		{
			"type": "token_usage",
			"usage": map[string]any{
				"input_tokens":  float64(100),
				"output_tokens": float64(50),
				"cost":          0.005,
			},
		},
		{"type": "stream_stopped"},
	}

	sess := SessionFromEvents(events, "test", []string{"question"})

	assert.Equal(t, int64(100), sess.InputTokens)
	assert.Equal(t, int64(50), sess.OutputTokens)
	assert.InDelta(t, 0.005, sess.Cost, 0.0001)
}

// TestSessionFromEventsPartialTokenUsage pins the merge semantics of
// consecutive token_usage events: a later event that omits some usage
// fields must preserve the previously recorded values for the omitted
// fields instead of resetting them to zero.
func TestSessionFromEventsPartialTokenUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		second     map[string]any
		wantInput  int64
		wantOutput int64
		wantCost   float64
	}{
		{
			name:       "second event omits input_tokens and cost",
			second:     map[string]any{"output_tokens": float64(80)},
			wantInput:  100,
			wantOutput: 80,
			wantCost:   0.005,
		},
		{
			name:       "second event omits output_tokens",
			second:     map[string]any{"input_tokens": float64(200), "cost": 0.01},
			wantInput:  200,
			wantOutput: 50,
			wantCost:   0.01,
		},
		{
			name:       "second event omits every field",
			second:     map[string]any{},
			wantInput:  100,
			wantOutput: 50,
			wantCost:   0.005,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			events := []map[string]any{
				{"type": "agent_choice", "content": "Answer"},
				{
					"type": "token_usage",
					"usage": map[string]any{
						"input_tokens":  float64(100),
						"output_tokens": float64(50),
						"cost":          0.005,
					},
				},
				{"type": "token_usage", "usage": tt.second},
				{"type": "stream_stopped"},
			}

			sess := SessionFromEvents(events, "test", []string{"question"})

			gotInput, gotOutput, gotCost := sess.TokensAndCost()
			assert.Equal(t, tt.wantInput, gotInput)
			assert.Equal(t, tt.wantOutput, gotOutput)
			assert.InDelta(t, tt.wantCost, gotCost, 0.0001)
		})
	}
}

func TestParseToolCall(t *testing.T) {
	t.Parallel()

	tc := map[string]any{
		"id":   "call_abc",
		"type": "function",
		"function": map[string]any{
			"name":      "read_file",
			"arguments": `{"path": "foo.txt"}`,
		},
	}

	toolCall := parseToolCall(tc)

	assert.Equal(t, "call_abc", toolCall.ID)
	assert.Equal(t, tools.ToolType("function"), toolCall.Type)
	assert.Equal(t, "read_file", toolCall.Function.Name)
	assert.JSONEq(t, `{"path": "foo.txt"}`, toolCall.Function.Arguments)
}

func TestParseToolDefinition(t *testing.T) {
	t.Parallel()

	td := map[string]any{
		"name":        "read_file",
		"category":    "filesystem",
		"description": "Read the contents of a file",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file path to read",
				},
			},
		},
	}

	toolDef := parseToolDefinition(td)

	assert.Equal(t, "read_file", toolDef.Name)
	assert.Equal(t, "filesystem", toolDef.Category)
	assert.Equal(t, "Read the contents of a file", toolDef.Description)
	assert.NotNil(t, toolDef.Parameters)
}

func TestSessionFromEventsWithToolDefinitions(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "agent_choice", "content": "Let me read that file.", "agent_name": "root"},
		{
			"type": "tool_call",
			"tool_call": map[string]any{
				"id":   "call_123",
				"type": "function",
				"function": map[string]any{
					"name":      "read_file",
					"arguments": `{"path": "test.txt"}`,
				},
			},
			"tool_definition": map[string]any{
				"name":        "read_file",
				"category":    "filesystem",
				"description": "Read the contents of a file",
			},
		},
		{
			"type":         "tool_call_response",
			"tool_call_id": "call_123",
			"response":     "file content",
		},
		{"type": "stream_stopped"},
	}

	sess := SessionFromEvents(events, "test", []string{"read the file"})

	// Find the assistant message with tool calls
	var assistantMsg *session.Message
	for _, item := range sess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant && len(item.Message.Message.ToolCalls) > 0 {
			assistantMsg = item.Message
			break
		}
	}

	require.NotNil(t, assistantMsg, "should have assistant message with tool calls")
	assert.Len(t, assistantMsg.Message.ToolCalls, 1)
	assert.Len(t, assistantMsg.Message.ToolDefinitions, 1)

	// Verify tool call
	toolCall := assistantMsg.Message.ToolCalls[0]
	assert.Equal(t, "call_123", toolCall.ID)
	assert.Equal(t, "read_file", toolCall.Function.Name)

	// Verify tool definition
	toolDef := assistantMsg.Message.ToolDefinitions[0]
	assert.Equal(t, "read_file", toolDef.Name)
	assert.Equal(t, "filesystem", toolDef.Category)
	assert.Equal(t, "Read the contents of a file", toolDef.Description)
}

func TestSessionFromEventsWithReasoningContent(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "agent_choice_reasoning", "content": "Let me think about this...", "agent_name": "root"},
		{"type": "agent_choice_reasoning", "content": " I should analyze the question."},
		{"type": "agent_choice", "content": "Here is my answer."},
		{"type": "stream_stopped"},
	}

	sess := SessionFromEvents(events, "test", []string{"complex question"})

	// Find the assistant message
	var assistantMsg *session.Message
	for _, item := range sess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			assistantMsg = item.Message
			break
		}
	}

	require.NotNil(t, assistantMsg, "should have assistant message")
	assert.Equal(t, "Here is my answer.", assistantMsg.Message.Content)
	assert.Equal(t, "Let me think about this... I should analyze the question.", assistantMsg.Message.ReasoningContent)
}

func TestSessionFromEventsWithPerMessageUsage(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "agent_choice", "content": "Hello!", "agent_name": "root"},
		{
			"type": "token_usage",
			"usage": map[string]any{
				"input_tokens":  float64(100),
				"output_tokens": float64(50),
				"cost":          0.005,
				"last_message": map[string]any{
					"input_tokens":        float64(100),
					"output_tokens":       float64(50),
					"cached_input_tokens": float64(25),
					"Model":               "gpt-4o",
					"Cost":                0.005,
				},
			},
		},
		{"type": "stream_stopped"},
	}

	sess := SessionFromEvents(events, "test", []string{"hi"})

	// Check session-level usage
	assert.Equal(t, int64(100), sess.InputTokens)
	assert.Equal(t, int64(50), sess.OutputTokens)
	assert.InDelta(t, 0.005, sess.Cost, 0.0001)

	// Find the assistant message
	var assistantMsg *session.Message
	for _, item := range sess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			assistantMsg = item.Message
			break
		}
	}

	require.NotNil(t, assistantMsg, "should have assistant message")
	assert.Equal(t, "gpt-4o", assistantMsg.Message.Model)
	assert.InDelta(t, 0.005, assistantMsg.Message.Cost, 0.0001)
	require.NotNil(t, assistantMsg.Message.Usage)
	assert.Equal(t, int64(100), assistantMsg.Message.Usage.InputTokens)
	assert.Equal(t, int64(50), assistantMsg.Message.Usage.OutputTokens)
	assert.Equal(t, int64(25), assistantMsg.Message.Usage.CachedInputTokens)
}

func TestSessionFromEventsWithError(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "agent_choice", "content": "Let me try...", "agent_name": "root"},
		{"type": "error", "error": "API rate limit exceeded"},
		{"type": "stream_stopped"},
	}

	sess := SessionFromEvents(events, "test", []string{"do something"})

	// Should have: user message, assistant message, error message
	assert.Len(t, sess.Messages, 3)

	// Check the error message was captured
	errorMsg := sess.Messages[2].Message
	require.NotNil(t, errorMsg)
	assert.Equal(t, chat.MessageRoleSystem, errorMsg.Message.Role)
	assert.Contains(t, errorMsg.Message.Content, "API rate limit exceeded")
}

func TestSessionFromEventsWithSessionTitle(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "session_title", "title": "Auto-generated title"},
		{"type": "agent_choice", "content": "Hello!"},
		{"type": "stream_stopped"},
	}

	// Start with a default title
	sess := SessionFromEvents(events, "default-title", []string{"hi"})

	// Title should be updated from the event
	assert.Equal(t, "Auto-generated title", sess.Title)
}

func TestInputIDPassthrough(t *testing.T) {
	t.Parallel()

	const knownInputID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	sess := SessionFromEvents(nil, "title", []string{"q"})
	sess.InputID = knownInputID

	assert.Equal(t, knownInputID, sess.InputID)
	assert.NotEmpty(t, sess.ID)
	assert.NotEqual(t, knownInputID, sess.ID)
}

// budgetStopText is the assistant stop message the runtime emits when a
// budget ceiling stops a run, reused across the termination tests.
const budgetStopText = "Execution stopped after reaching the configured budget.max_cost limit (used $0.03 of $0.03)."

// budgetExceededTestEvent returns a fresh copy of the runtime's native
// budget_exceeded event as decoded from `run --exec --json` output,
// embedding the assistant stop message under stop_message.
func budgetExceededTestEvent() map[string]any {
	event := budgetExceededTestEventWithoutStop()
	event["stop_message"] = stopMessagePayload("root", budgetStopText)
	return event
}

// budgetExceededTestEventWithoutStop returns a budget_exceeded event that
// carries no embedded stop message: the marker must then stand alone.
func budgetExceededTestEventWithoutStop() map[string]any {
	return map[string]any{
		"type":        "budget_exceeded",
		"session_id":  "sess-1",
		"agent_name":  "root",
		"timestamp":   "2024-01-15T10:30:00Z",
		"budget":      "run",
		"limit":       "max_cost",
		"used":        "$0.03",
		"max":         "$0.03",
		"config_path": "budget.max_cost",
		"message":     budgetStopText,
	}
}

// stopMessagePayload mirrors the decoded wire shape of
// BudgetExceededEvent.StopMessage: the runtime's dedicated DTO carrying
// only the agent name and the assistant message's safe fields.
func stopMessagePayload(agentName, content string) map[string]any {
	return map[string]any{
		"agent_name": agentName,
		"message": map[string]any{
			"role":       "assistant",
			"content":    content,
			"created_at": "2024-01-15T10:30:01Z",
		},
	}
}

// messageAddedTestEvent returns a message_added event carrying a full
// message payload, as a hostile or foreign producer could serialize it.
// The pipeline must ignore every message_added event.
func messageAddedTestEvent(sessionID, agentName, content string) map[string]any {
	return map[string]any{
		"type":       "message_added",
		"session_id": sessionID,
		"agent_name": agentName,
		"timestamp":  "2024-01-15T10:30:01Z",
		"message":    stopMessagePayload(agentName, content),
	}
}

func TestSessionFromEventsBudgetTermination(t *testing.T) {
	t.Parallel()

	sess := SessionFromEvents([]map[string]any{
		{"type": "agent_choice", "content": "Working on it.", "agent_name": "root"},
		budgetExceededTestEvent(),
		// The runtime still emits message_added after budget_exceeded; it
		// must be ignored even when a payload is present.
		messageAddedTestEvent("sess-1", "root", budgetStopText),
		{"type": "stream_stopped"},
	}, "budget-case", []string{"do work"})

	// user question, buffered assistant content, termination marker,
	// assistant stop: in that chronological order.
	require.Len(t, sess.Messages, 4)
	assert.Equal(t, chat.MessageRoleUser, sess.Messages[0].Message.Message.Role)
	assert.Equal(t, "Working on it.", sess.Messages[1].Message.Message.Content)

	require.True(t, sess.Messages[2].IsTermination())
	term := sess.Messages[2].Termination
	assert.Equal(t, session.TerminationReasonBudgetExceeded, term.Reason)
	assert.Equal(t, "run", term.Budget)
	assert.Equal(t, "max_cost", term.Limit)
	assert.Equal(t, "budget.max_cost", term.ConfigPath)

	stop := sess.Messages[3].Message
	require.NotNil(t, stop)
	assert.Equal(t, chat.MessageRoleAssistant, stop.Message.Role)
	assert.Equal(t, budgetStopText, stop.Message.Content)
	assert.Equal(t, "root", stop.AgentName)
	assert.Equal(t, "2024-01-15T10:30:01Z", stop.Message.CreatedAt)
	// Only the safe representation is imported.
	assert.Empty(t, stop.Message.ToolCalls)
	assert.Empty(t, stop.Message.ReasoningContent)
	assert.Empty(t, stop.Message.Model)
	assert.Nil(t, stop.Message.Usage)

	// The helper surfaces the marker for populateEvalResult.
	require.NotNil(t, sess.Termination())
	assert.Equal(t, *term, *sess.Termination())
}

func TestSessionFromEventsBudgetTerminationRepeatedEvents(t *testing.T) {
	t.Parallel()

	sess := SessionFromEvents([]map[string]any{
		budgetExceededTestEvent(),
		messageAddedTestEvent("sess-1", "root", budgetStopText),
		budgetExceededTestEvent(),
		messageAddedTestEvent("sess-1", "root", budgetStopText),
		{"type": "stream_stopped"},
	}, "budget-case", []string{"do work"})

	var markers, stops int
	for _, item := range sess.Messages {
		if item.IsTermination() {
			markers++
		}
		if item.Message != nil && item.Message.Message.Content == budgetStopText {
			stops++
		}
	}
	assert.Equal(t, 1, markers, "repeated budget_exceeded events must not duplicate the marker")
	assert.Equal(t, 1, stops, "repeated budget_exceeded events must not duplicate the stop message")
}

func TestSessionFromEventsForeignMessageAddedIgnored(t *testing.T) {
	t.Parallel()

	sess := SessionFromEvents([]map[string]any{
		messageAddedTestEvent("sess-1", "root", "before any marker"),
		{"type": "agent_choice", "content": "All done.", "agent_name": "root"},
		messageAddedTestEvent("other-session", "intruder", "after the turn"),
		{"type": "stream_stopped"},
	}, "normal-case", []string{"do work"})

	// user question + assistant turn only; no termination, no imported
	// message_added content.
	require.Len(t, sess.Messages, 2)
	assert.Nil(t, sess.Termination())
	for _, item := range sess.Messages {
		assert.False(t, item.IsTermination())
		if item.Message != nil {
			assert.NotContains(t, item.Message.Message.Content, "before any marker")
			assert.NotContains(t, item.Message.Message.Content, "after the turn")
		}
	}
}

func TestSessionFromEventsBudgetTerminationMalformedFields(t *testing.T) {
	t.Parallel()

	event := budgetExceededTestEvent()
	event["budget"] = 42          // wrong type
	event["used"] = nil           // wrong type
	delete(event, "config_path")  // missing
	event["message"] = " \x01\t " // empty after sanitization

	sess := SessionFromEvents([]map[string]any{
		event,
		{"type": "stream_stopped"},
	}, "budget-case", []string{"do work"})

	term := sess.Termination()
	require.NotNil(t, term, "malformed optional fields must not drop the marker")
	assert.Equal(t, session.TerminationReasonBudgetExceeded, term.Reason)
	assert.Empty(t, term.Budget)
	assert.Empty(t, term.Used)
	assert.Empty(t, term.ConfigPath)
	assert.Empty(t, term.Message)
	assert.Equal(t, "max_cost", term.Limit)
}

func TestSessionFromEventsBudgetTerminationWithoutStopMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event map[string]any
	}{
		{"stop_message absent", budgetExceededTestEventWithoutStop()},
		{"stop_message wrong type", func() map[string]any {
			event := budgetExceededTestEventWithoutStop()
			event["stop_message"] = "not a map"
			return event
		}()},
		{"stop_message wrong role", func() map[string]any {
			event := budgetExceededTestEventWithoutStop()
			payload := stopMessagePayload("root", budgetStopText)
			payload["message"].(map[string]any)["role"] = "user"
			event["stop_message"] = payload
			return event
		}()},
		{"stop_message empty content", func() map[string]any {
			event := budgetExceededTestEventWithoutStop()
			event["stop_message"] = stopMessagePayload("root", " \x00 ")
			return event
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sess := SessionFromEvents([]map[string]any{
				{"type": "agent_choice", "content": "Working on it.", "agent_name": "root"},
				tt.event,
				{"type": "stream_stopped"},
			}, "budget-case", []string{"do work"})

			require.NotNil(t, sess.Termination(), "the marker stands alone")
			// No assistant message may be invented from termination.message.
			assert.Equal(t, "Working on it.", sess.GetLastAssistantMessageContent())
		})
	}
}

// TestSessionFromEventsForeignEventsCannotDisturbBudgetStop pins the
// atomicity of the association: the stop message travels inside the
// budget_exceeded event itself, so a message_added from another session
// and a foreign stream_stopped can neither be captured as the stop nor
// suppress the real one.
func TestSessionFromEventsForeignEventsCannotDisturbBudgetStop(t *testing.T) {
	t.Parallel()

	sess := SessionFromEvents([]map[string]any{
		{"type": "agent_choice", "content": "Working on it.", "agent_name": "root"},
		messageAddedTestEvent("other-session", "intruder", "fake stop"),
		{"type": "stream_stopped", "session_id": "other-session"},
		budgetExceededTestEvent(),
		messageAddedTestEvent("other-session", "intruder", "another fake stop"),
		{"type": "stream_stopped"},
	}, "budget-case", []string{"do work"})

	require.NotNil(t, sess.Termination())

	markerIdx, stopIdx := -1, -1
	for i, item := range sess.Messages {
		if item.IsTermination() {
			markerIdx = i
		}
		if item.Message != nil {
			assert.NotEqual(t, "intruder", item.Message.AgentName)
			assert.NotContains(t, item.Message.Message.Content, "fake stop")
			if item.Message.Message.Content == budgetStopText {
				stopIdx = i
			}
		}
	}
	require.GreaterOrEqual(t, markerIdx, 0, "the real marker must be recorded")
	assert.Equal(t, markerIdx+1, stopIdx, "the real stop must directly follow its marker")
}

func TestSaveRunSessionsBudgetTerminationRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	outputDir := t.TempDir()

	budgetSess := SessionFromEvents([]map[string]any{
		{"type": "agent_choice", "content": "Working on it.", "agent_name": "root"},
		budgetExceededTestEvent(),
		messageAddedTestEvent("sess-1", "root", budgetStopText),
		{"type": "stream_stopped"},
	}, "budget-case", []string{"do work"})

	normalSess := SessionFromEvents([]map[string]any{
		{"type": "agent_choice", "content": "All done.", "agent_name": "root"},
		{"type": "stream_stopped"},
	}, "normal-case", []string{"do work"})

	run := &EvalRun{
		Name:      "test-termination-001",
		Timestamp: time.Now(),
		Results: []Result{
			{Title: "budget-case", Session: budgetSess},
			{Title: "normal-case", Session: normalSess},
		},
	}

	dbPath, err := SaveRunSessions(ctx, run, outputDir)
	require.NoError(t, err)

	store, err := sqlitestore.New(ctx, dbPath)
	require.NoError(t, err)
	defer func() {
		if closer, ok := store.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	sessions, err := store.GetSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 2)

	byTitle := make(map[string]*session.Session)
	for _, sess := range sessions {
		byTitle[sess.Title] = sess
	}

	loaded := byTitle["budget-case"]
	require.NotNil(t, loaded)
	require.Len(t, loaded.Messages, 4)
	require.True(t, loaded.Messages[2].IsTermination())
	assert.Equal(t, session.Termination{
		Reason:     session.TerminationReasonBudgetExceeded,
		Budget:     "run",
		Limit:      "max_cost",
		Used:       "$0.03",
		Max:        "$0.03",
		ConfigPath: "budget.max_cost",
		Message:    budgetStopText,
	}, *loaded.Messages[2].Termination)

	stop := loaded.Messages[3].Message
	require.NotNil(t, stop)
	assert.Equal(t, chat.MessageRoleAssistant, stop.Message.Role)
	assert.Equal(t, budgetStopText, stop.Message.Content)
	assert.Equal(t, "root", stop.AgentName)

	// The other case is isolated: no termination leaks across sessions.
	normal := byTitle["normal-case"]
	require.NotNil(t, normal)
	assert.Nil(t, normal.Termination())
	for _, item := range normal.Messages {
		assert.False(t, item.IsTermination())
	}
}

func TestSaveRunSessionsJSONBudgetTermination(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()

	budgetSess := SessionFromEvents([]map[string]any{
		{"type": "agent_choice", "content": "Working on it.", "agent_name": "root"},
		budgetExceededTestEvent(),
		messageAddedTestEvent("sess-1", "root", budgetStopText),
		{"type": "stream_stopped"},
	}, "budget-case", []string{"do work"})

	normalSess := SessionFromEvents([]map[string]any{
		{"type": "agent_choice", "content": "All done.", "agent_name": "root"},
		{"type": "stream_stopped"},
	}, "normal-case", []string{"do work"})

	errorSess := SessionFromEvents([]map[string]any{
		{"type": "error", "error": "boom"},
		{"type": "stream_stopped"},
	}, "error-case", []string{"do work"})

	run := &EvalRun{
		Name:      "test-termination-json-001",
		Timestamp: time.Now(),
		Results: []Result{
			{Title: "budget-case", Session: budgetSess},
			{Title: "normal-case", Session: normalSess},
			{Title: "error-case", Error: "container failed", Session: errorSess},
		},
	}

	sessionsPath, err := SaveRunSessionsJSON(run, outputDir)
	require.NoError(t, err)

	data, err := os.ReadFile(sessionsPath)
	require.NoError(t, err)

	var output RunOutput
	require.NoError(t, json.Unmarshal(data, &output))
	require.Len(t, output.Sessions, 3)

	byTitle := make(map[string]*session.Session)
	for _, sess := range output.Sessions {
		byTitle[sess.Title] = sess
	}

	// The budget session round-trips marker, stop message, and
	// eval_result.termination; scoring is untouched by the termination.
	budgetLoaded := byTitle["budget-case"]
	require.NotNil(t, budgetLoaded)
	require.True(t, budgetLoaded.Messages[2].IsTermination())
	assert.Equal(t, budgetStopText, budgetLoaded.Messages[3].Message.Message.Content)
	require.NotNil(t, budgetLoaded.EvalResult)
	require.NotNil(t, budgetLoaded.EvalResult.Termination)
	assert.Equal(t, session.TerminationReasonBudgetExceeded, budgetLoaded.EvalResult.Termination.Reason)
	assert.Equal(t, "budget.max_cost", budgetLoaded.EvalResult.Termination.ConfigPath)
	assert.True(t, budgetLoaded.EvalResult.Passed, "termination must not affect scoring")
	assert.Empty(t, budgetLoaded.EvalResult.Error)

	// Normal and errored runs expose no termination at all.
	normalLoaded := byTitle["normal-case"]
	require.NotNil(t, normalLoaded)
	require.NotNil(t, normalLoaded.EvalResult)
	assert.Nil(t, normalLoaded.EvalResult.Termination)
	assert.Nil(t, normalLoaded.Termination())

	errorLoaded := byTitle["error-case"]
	require.NotNil(t, errorLoaded)
	require.NotNil(t, errorLoaded.EvalResult)
	assert.Nil(t, errorLoaded.EvalResult.Termination)
	assert.Equal(t, "container failed", errorLoaded.EvalResult.Error)
	assert.False(t, errorLoaded.EvalResult.Passed)

	// Raw JSON contract: the serialized termination carries only
	// allow-listed keys, and sessions without one omit the field.
	var raw struct {
		Sessions []map[string]any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(data, &raw))
	allowed := []string{"reason", "budget", "limit", "used", "max", "config_path", "message"}
	for _, rawSess := range raw.Sessions {
		evalResult, ok := rawSess["eval_result"].(map[string]any)
		require.True(t, ok)
		rawTerm, hasTerm := evalResult["termination"]
		if rawSess["title"] != "budget-case" {
			assert.False(t, hasTerm, "session %v must not expose a termination", rawSess["title"])
			continue
		}
		termKeys, ok := rawTerm.(map[string]any)
		require.True(t, ok)
		for key := range termKeys {
			assert.Contains(t, allowed, key)
		}
	}
}

// TestSessionJSONWithoutTerminationLoads pins backward compatibility:
// sessions and eval results serialized before the termination field
// existed must load with no termination.
func TestSessionJSONWithoutTerminationLoads(t *testing.T) {
	t.Parallel()

	legacy := `{
		"id": "old-session",
		"title": "old-eval",
		"messages": [
			{"message": {"agent_name": "", "message": {"role": "user", "content": "hi", "created_at": "2024-01-15T10:30:00Z"}}},
			{"message": {"agent_name": "root", "message": {"role": "assistant", "content": "hello", "created_at": "2024-01-15T10:30:01Z"}}}
		],
		"eval_result": {"passed": true, "cost": 0, "output_tokens": 0, "checks": {}},
		"created_at": "2024-01-15T10:30:00Z",
		"tools_approved": false,
		"hide_tool_results": false,
		"max_iterations": 0,
		"starred": false,
		"input_tokens": 0,
		"output_tokens": 0,
		"cost": 0
	}`

	var sess session.Session
	require.NoError(t, json.Unmarshal([]byte(legacy), &sess))
	assert.Nil(t, sess.Termination())
	require.NotNil(t, sess.EvalResult)
	assert.Nil(t, sess.EvalResult.Termination)
	assert.Len(t, sess.Messages, 2)
}
