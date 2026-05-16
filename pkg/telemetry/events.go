package telemetry

import (
	"context"
	"time"
)

func (tc *Client) TrackSynchronous(_ context.Context, structuredEvent StructuredEvent) {
	tc.track(context.Background(), structuredEvent, false)
}

// Track records a structured telemetry event with type-safe properties (synchronous)
// This is the only method for telemetry tracking, all event-specific methods are wrappers around this one
func (tc *Client) Track(ctx context.Context, structuredEvent StructuredEvent) {
	tc.track(ctx, structuredEvent, true)
}

func (tc *Client) track(_ context.Context, structuredEvent StructuredEvent, async bool) {
	eventType := structuredEvent.GetEventType()
	structuredProps := structuredEvent.ToStructuredProperties()

	// Convert structured properties to map using JSON marshaling
	// This automatically handles all fields and respects json tags (including omitempty)
	properties, err := structToMap(structuredProps)
	if err != nil {
		tc.logger.Error("Failed to convert structured properties to map", "error", err, "event_type", eventType)
		return
	}

	// Debug logging to track event flow
	if tc.debugMode {
		tc.logger.Debug("Processing telemetry event synchronously", "event_type", eventType)
	}

	event := tc.createEvent(string(eventType), properties)

	if tc.debugMode {
		tc.printEvent(&event)
	}

	if !tc.enabled {
		return
	}

	if async {
		go tc.sendEvent(&event) //nolint:gosec // telemetry is fire-and-forget; sendEvent uses its own context internally
	} else {
		tc.sendEvent(&event)
	}
}

// RecordSessionStart initializes session tracking
func (tc *Client) RecordSessionStart(ctx context.Context, agentName, sessionID string) string {
	tc.mu.Lock()
	tc.session = SessionState{
		ID:           sessionID,
		AgentName:    agentName,
		StartTime:    time.Now(),
		ToolCalls:    0,
		TokenUsage:   SessionTokenUsage{},
		ErrorCount:   0,
		Error:        []string{},
		SessionEnded: false,
	}
	tc.mu.Unlock()

	if tc.enabled {
		sessionEvent := &SessionStartEvent{
			Action:    "start",
			SessionID: sessionID,
			AgentName: agentName,
		}
		tc.Track(ctx, sessionEvent)
	}

	return sessionID
}

// RecordError records a general session error
func (tc *Client) RecordError(_ context.Context, errorMsg string) {
	tc.mu.Lock()

	if tc.session.SessionEnded || tc.session.AgentName == "" || tc.session.ID == "" {
		tc.mu.Unlock()
		return
	}

	tc.session.ErrorCount++
	tc.session.Error = append(tc.session.Error, errorMsg)

	tc.mu.Unlock()
}

// RecordSessionEnd finalizes session tracking
func (tc *Client) RecordSessionEnd(ctx context.Context) {
	tc.mu.Lock()

	if tc.session.SessionEnded || tc.session.AgentName == "" || tc.session.ID == "" {
		tc.mu.Unlock()
		return
	}

	tc.session.SessionEnded = true

	// Capture session data while holding the lock
	sessionEvent := &SessionEndEvent{
		Action:       "end",
		SessionID:    tc.session.ID,
		AgentName:    tc.session.AgentName,
		Duration:     time.Since(tc.session.StartTime).Milliseconds(),
		ToolCalls:    tc.session.ToolCalls,
		InputTokens:  tc.session.TokenUsage.InputTokens,
		OutputTokens: tc.session.TokenUsage.OutputTokens,
		TotalTokens:  tc.session.TokenUsage.InputTokens + tc.session.TokenUsage.OutputTokens,
		IsSuccess:    tc.session.ErrorCount == 0,
		Error:        tc.session.Error,
	}

	tc.mu.Unlock()

	if tc.enabled {
		tc.Track(ctx, sessionEvent)
	}
}

// RecordToolCall records a tool call event
func (tc *Client) RecordToolCall(ctx context.Context, toolName, sessionID, agentName string, duration time.Duration, err error) {
	tc.mu.Lock()
	tc.session.ToolCalls++
	if err != nil {
		tc.session.ErrorCount++
	}
	tc.mu.Unlock()

	if tc.enabled {
		errorMsg := ""
		if err != nil {
			errorMsg = err.Error()
		}

		toolEvent := &ToolEvent{
			Action:    "call",
			ToolName:  toolName,
			SessionID: sessionID,
			AgentName: agentName,
			Duration:  duration.Milliseconds(),
			Success:   err == nil,
			Error:     errorMsg,
		}
		tc.Track(ctx, toolEvent)
	}
}

// RecordTokenUsage records token usage metrics
func (tc *Client) RecordTokenUsage(ctx context.Context, model string, inputTokens, outputTokens int64, cost float64) {
	tc.mu.Lock()
	tc.session.TokenUsage.InputTokens += inputTokens
	tc.session.TokenUsage.OutputTokens += outputTokens
	tc.session.TokenUsage.Cost += cost

	tokenEvent := &TokenEvent{
		Action:       "usage",
		ModelName:    model,
		SessionID:    tc.session.ID,
		AgentName:    tc.session.AgentName,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         cost,
	}

	tc.mu.Unlock()

	if tc.enabled {
		tc.Track(ctx, tokenEvent)
	}
}
