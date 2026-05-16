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
	store, err := session.NewSQLiteSessionStore(dbPath)
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
	store, err := session.NewSQLiteSessionStore(dbPath)
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
