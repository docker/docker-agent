package evaluation

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// SaveRunSessions saves all eval sessions to a SQLite database file.
// The database follows the same schema as the main session store,
// allowing the sessions to be loaded and inspected using standard session tools.
func SaveRunSessions(ctx context.Context, run *EvalRun, outputDir string) (string, error) {
	dbPath := filepath.Join(outputDir, run.Name+".db")

	// Create output directory if needed
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return "", fmt.Errorf("creating output directory: %w", err)
	}

	// Create a new SQLite session store for this eval run
	store, err := session.NewSQLiteSessionStore(dbPath)
	if err != nil {
		return "", fmt.Errorf("creating session store: %w", err)
	}
	defer func() {
		if closer, ok := store.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	// Save each result's session to the database
	for i := range run.Results {
		result := &run.Results[i]
		if result.Session == nil {
			continue
		}

		if err := store.AddSession(ctx, result.Session); err != nil {
			return "", fmt.Errorf("saving session for %q: %w", result.Title, err)
		}
	}

	return dbPath, nil
}

// SessionFromEvents reconstructs a session from raw container output events.
// This parses the JSON events emitted by docker agent run --exec --json and builds a session
// with the conversation history.
func SessionFromEvents(events []map[string]any, title string, questions []string) *session.Session {
	sess := session.New(
		session.WithTitle(title),
		session.WithToolsApproved(true),
		session.WithNonInteractive(true),
	)

	// Add user questions as initial messages.
	// For multi-turn evals, these are interleaved with agent responses
	// as they appear in the event stream. User messages are added when
	// a "user_message" event is encountered (which carries the correct
	// timestamp), or when a "stream_stopped" event indicates the agent
	// finished processing the previous turn in a multi-turn eval.
	// If no "user_message" event is found before the first agent response,
	// the question is added with the timestamp of that first response.
	questionIdx := 0
	userMessageAdded := false
	addNextQuestion := func(timestamp string) {
		if questionIdx < len(questions) {
			msg := &session.Message{
				Message: chat.Message{
					Role:      chat.MessageRoleUser,
					Content:   questions[questionIdx],
					CreatedAt: timestamp,
				},
			}
			sess.AddMessage(msg)
			questionIdx++
			userMessageAdded = true
		}
	}

	// Track current assistant message being built
	var currentContent strings.Builder
	var currentReasoningContent strings.Builder
	var currentToolCalls []tools.ToolCall
	var currentToolDefinitions []tools.Tool
	var currentAgentName string
	var currentModel string
	var currentUsage *chat.Usage
	var currentCost float64
	var currentTimestamp string

	// Helper to flush current assistant message
	flushAssistantMessage := func() {
		if currentContent.Len() > 0 || currentReasoningContent.Len() > 0 || len(currentToolCalls) > 0 {
			msg := &session.Message{
				AgentName: currentAgentName,
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					Content:          currentContent.String(),
					ReasoningContent: currentReasoningContent.String(),
					ToolCalls:        currentToolCalls,
					ToolDefinitions:  currentToolDefinitions,
					CreatedAt:        currentTimestamp,
					Model:            currentModel,
					Usage:            currentUsage,
					Cost:             currentCost,
				},
			}
			sess.AddMessage(msg)
			currentContent.Reset()
			currentReasoningContent.Reset()
			currentToolCalls = nil
			currentToolDefinitions = nil
			currentModel = ""
			currentUsage = nil
			currentCost = 0
			currentTimestamp = ""
		}
	}

	for _, event := range events {
		eventType, _ := event["type"].(string)
		eventTimestamp := parseEventTimestamp(event)

		switch eventType {
		case "user_message":
			// Use the event timestamp for the user message instead of time.Now()
			if !userMessageAdded {
				addNextQuestion(eventTimestamp)
			}

		case "agent_choice":
			// Ensure a user message has been added before the first agent response.
			// This handles event streams that lack a "user_message" event.
			if !userMessageAdded {
				addNextQuestion(eventTimestamp)
			}

			// Accumulate agent response content
			if content, ok := event["content"].(string); ok {
				currentContent.WriteString(content)
			}
			if agentName, ok := event["agent_name"].(string); ok && agentName != "" {
				currentAgentName = agentName
			}
			if eventTimestamp != "" {
				currentTimestamp = eventTimestamp
			}

		case "agent_choice_reasoning":
			// Accumulate reasoning content (for models like DeepSeek, Claude with extended thinking)
			if content, ok := event["content"].(string); ok {
				currentReasoningContent.WriteString(content)
			}
			if agentName, ok := event["agent_name"].(string); ok && agentName != "" {
				currentAgentName = agentName
			}
			if eventTimestamp != "" {
				currentTimestamp = eventTimestamp
			}

		case "tool_call":
			// Parse tool call and add to current message
			if tc, ok := event["tool_call"].(map[string]any); ok {
				toolCall := parseToolCall(tc)
				currentToolCalls = append(currentToolCalls, toolCall)
			}
			// Parse tool definition if present
			if td, ok := event["tool_definition"].(map[string]any); ok {
				toolDef := parseToolDefinition(td)
				currentToolDefinitions = append(currentToolDefinitions, toolDef)
			} else {
				// Add empty tool definition to maintain index alignment with tool calls
				currentToolDefinitions = append(currentToolDefinitions, tools.Tool{})
			}
			if agentName, ok := event["agent_name"].(string); ok && agentName != "" {
				currentAgentName = agentName
			}
			if eventTimestamp != "" {
				currentTimestamp = eventTimestamp
			}

		case "tool_call_response":
			// Flush any pending assistant message before adding tool response
			flushAssistantMessage()

			// The ToolCallResponseEvent serializes tool_call_id as a top-level string field,
			// not nested under a "tool_call" map.
			toolCallID, _ := event["tool_call_id"].(string)
			response, _ := event["response"].(string)

			msg := &session.Message{
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					Content:    response,
					ToolCallID: toolCallID,
					CreatedAt:  eventTimestamp,
				},
			}
			sess.AddMessage(msg)

		case "token_usage":
			// Update session token usage
			if usage, ok := event["usage"].(map[string]any); ok {
				if inputTokens, ok := usage["input_tokens"].(float64); ok {
					sess.InputTokens = int64(inputTokens)
				}
				if outputTokens, ok := usage["output_tokens"].(float64); ok {
					sess.OutputTokens = int64(outputTokens)
				}
				if cost, ok := usage["cost"].(float64); ok {
					sess.Cost = cost
				}
				// Extract per-message usage if available
				if lastMsg, ok := usage["last_message"].(map[string]any); ok {
					currentUsage = parseMessageUsage(lastMsg)
					if model, ok := lastMsg["Model"].(string); ok {
						currentModel = model
					}
					if msgCost, ok := lastMsg["Cost"].(float64); ok {
						currentCost = msgCost
					}
				}
			}

		case "error":
			// Flush any pending assistant message before adding error
			flushAssistantMessage()

			// Add error as a system message so it's visible in the session
			if errorMsg, ok := event["error"].(string); ok && errorMsg != "" {
				msg := &session.Message{
					Message: chat.Message{
						Role:      chat.MessageRoleSystem,
						Content:   "Error: " + errorMsg,
						CreatedAt: eventTimestamp,
					},
				}
				sess.AddMessage(msg)
			}

		case "session_title":
			// Update session title if provided (may override the one from eval config)
			if eventTitle, ok := event["title"].(string); ok && eventTitle != "" {
				sess.Title = eventTitle
			}

		case "stream_stopped":
			// Flush final assistant message
			flushAssistantMessage()

			// In multi-turn evals, add the next user question after each turn.
			// Reset the flag so the next user_message event (or agent_choice
			// fallback) will add the question for the next turn.
			userMessageAdded = false
		}
	}

	// Flush any remaining content
	flushAssistantMessage()

	// Add any remaining questions that weren't added via user_message or
	// agent_choice events (e.g. when the event stream is empty).
	for questionIdx < len(questions) {
		addNextQuestion(time.Now().Format(time.RFC3339))
	}

	return sess
}

// parseToolCall converts a map representation of a tool call to tools.ToolCall
func parseToolCall(tc map[string]any) tools.ToolCall {
	toolCall := tools.ToolCall{}

	if id, ok := tc["id"].(string); ok {
		toolCall.ID = id
	}
	if typ, ok := tc["type"].(string); ok {
		toolCall.Type = tools.ToolType(typ)
	}

	if fn, ok := tc["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			toolCall.Function.Name = name
		}
		if args, ok := fn["arguments"].(string); ok {
			toolCall.Function.Arguments = args
		}
	}

	return toolCall
}

// parseToolDefinition converts a map representation of a tool definition to tools.Tool
func parseToolDefinition(td map[string]any) tools.Tool {
	toolDef := tools.Tool{}

	if name, ok := td["name"].(string); ok {
		toolDef.Name = name
	}
	if category, ok := td["category"].(string); ok {
		toolDef.Category = category
	}
	if description, ok := td["description"].(string); ok {
		toolDef.Description = description
	}
	if parameters, ok := td["parameters"]; ok {
		toolDef.Parameters = parameters
	}

	return toolDef
}

// parseMessageUsage converts a map representation of message usage to chat.Usage
// Note: The embedded chat.Usage fields use snake_case JSON tags (input_tokens, etc.)
// while Cost and Model don't have JSON tags and serialize with capitalized names.
func parseMessageUsage(m map[string]any) *chat.Usage {
	usage := &chat.Usage{}

	// Try snake_case first (from JSON serialization), then capitalized (fallback)
	if v, ok := m["input_tokens"].(float64); ok {
		usage.InputTokens = int64(v)
	} else if v, ok := m["InputTokens"].(float64); ok {
		usage.InputTokens = int64(v)
	}
	if v, ok := m["output_tokens"].(float64); ok {
		usage.OutputTokens = int64(v)
	} else if v, ok := m["OutputTokens"].(float64); ok {
		usage.OutputTokens = int64(v)
	}
	if v, ok := m["cached_input_tokens"].(float64); ok {
		usage.CachedInputTokens = int64(v)
	} else if v, ok := m["CachedInputTokens"].(float64); ok {
		usage.CachedInputTokens = int64(v)
	}
	if v, ok := m["cached_write_tokens"].(float64); ok {
		usage.CacheWriteTokens = int64(v)
	} else if v, ok := m["CacheWriteTokens"].(float64); ok {
		usage.CacheWriteTokens = int64(v)
	}
	if v, ok := m["reasoning_tokens"].(float64); ok {
		usage.ReasoningTokens = int64(v)
	} else if v, ok := m["ReasoningTokens"].(float64); ok {
		usage.ReasoningTokens = int64(v)
	}

	return usage
}

// parseEventTimestamp extracts the timestamp from an event map.
// Returns the timestamp string, falling back to current time if not present or invalid.
func parseEventTimestamp(event map[string]any) string {
	if ts, ok := event["timestamp"].(string); ok && ts != "" {
		// Validate RFC3339 format
		if _, err := time.Parse(time.RFC3339, ts); err == nil {
			return ts
		}
		// Invalid timestamp format - fall back to current time
	}
	return time.Now().Format(time.RFC3339)
}

// SaveRunJSON saves the eval run results to a JSON file.
// This is kept for backward compatibility and debugging purposes.
func SaveRunJSON(run *EvalRun, outputDir string) (string, error) {
	return saveJSON(run, filepath.Join(outputDir, run.Name+".json"))
}

// SaveRunSessionsJSON saves the full evaluation run output to a JSON file.
// The output includes run metadata (config, summary) and all sessions with
// their eval criteria and scoring results (pass/fail, judge reasoning, errors).
func SaveRunSessionsJSON(run *EvalRun, outputDir string) (string, error) {
	// Populate eval results on each session
	for i := range run.Results {
		populateEvalResult(&run.Results[i])
	}

	// Collect all sessions from results
	var sessions []*session.Session
	for i := range run.Results {
		if run.Results[i].Session != nil {
			sessions = append(sessions, run.Results[i].Session)
		}
	}

	output := RunOutput{
		Name:      run.Name,
		Timestamp: run.Timestamp,
		Duration:  run.Duration.Round(time.Millisecond).String(),
		Config: RunOutputConfig{
			Agent:       run.Config.AgentFilename,
			JudgeModel:  run.Config.JudgeModel,
			Concurrency: run.Config.Concurrency,
			EvalsDir:    run.Config.EvalsDir,
			BaseImage:   run.Config.BaseImage,
		},
		Summary:  run.Summary,
		Sessions: sessions,
	}

	outputPath := filepath.Join(outputDir, run.Name+".json")
	return saveJSON(output, outputPath)
}

// populateEvalResult copies scoring data from a Result to its Session's EvalResult field.
func populateEvalResult(result *Result) {
	if result.Session == nil {
		return
	}

	successes, failures := result.checkResults()

	evalResult := &session.EvalResult{
		Passed:       len(failures) == 0,
		Successes:    successes,
		Failures:     failures,
		Error:        result.Error,
		Cost:         result.Cost,
		OutputTokens: result.OutputTokens,
	}

	// Populate size check if size was evaluated
	if result.SizeExpected != "" {
		evalResult.Checks.Size = &session.SizeCheck{
			Passed:   result.Size == result.SizeExpected,
			Actual:   result.Size,
			Expected: result.SizeExpected,
		}
	}

	// Populate tool calls check if tool calls were evaluated
	if result.ToolCallsExpected > 0 {
		evalResult.Checks.ToolCalls = &session.ToolCallsCheck{
			Passed: result.ToolCallsScore >= 1.0,
			Score:  result.ToolCallsScore,
		}
	}

	// Populate relevance check if relevance was evaluated
	if result.RelevanceExpected > 0 {
		results := make([]session.RelevanceCriterionResult, 0, len(result.RelevanceResults))
		for _, rr := range result.RelevanceResults {
			results = append(results, session.RelevanceCriterionResult{
				Criterion: rr.Criterion,
				Passed:    rr.Passed,
				Reason:    rr.Reason,
			})
		}

		evalResult.Checks.Relevance = &session.RelevanceCheck{
			Passed:      result.RelevancePassed >= result.RelevanceExpected,
			PassedCount: result.RelevancePassed,
			Total:       result.RelevanceExpected,
			Results:     results,
		}
	}

	result.Session.EvalResult = evalResult
}

func Save(sess *session.Session, filename string) (string, error) {
	baseName := cmp.Or(filename, sess.ID)

	evalFile := filepath.Join("evals", baseName+".json")
	for number := 1; ; number++ {
		if _, err := os.Stat(evalFile); err != nil {
			break
		}

		evalFile = filepath.Join("evals", fmt.Sprintf("%s_%d.json", baseName, number))
	}

	// Ensure session has empty eval criteria for easier discovery
	if sess.Evals == nil {
		sess.Evals = &session.EvalCriteria{Relevance: []string{}}
	}

	return saveJSON(sess, evalFile)
}

func saveJSON(value any, outputPath string) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return "", err
	}

	if err := os.WriteFile(outputPath, data, 0o600); err != nil {
		return "", err
	}

	return outputPath, nil
}
