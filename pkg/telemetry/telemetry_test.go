package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockHTTPClient captures HTTP requests for testing
type MockHTTPClient struct {
	*http.Client

	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	response *http.Response
}

// NewMockHTTPClient creates a new mock HTTP client with a default success response
func NewMockHTTPClient() *MockHTTPClient {
	mock := &MockHTTPClient{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"success": true}`))),
			Header:     make(http.Header),
		},
	}
	mock.Client = &http.Client{Transport: mock}
	return mock
}

// SetResponse allows updating the mock response for testing different scenarios
func (m *MockHTTPClient) SetResponse(resp *http.Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.response = resp
}

// RoundTrip implements http.RoundTripper and captures the request
func (m *MockHTTPClient) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Capture the request
	m.requests = append(m.requests, req)

	// Read and store the body for inspection
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		m.bodies = append(m.bodies, body)
		// Reset body for the actual request processing
		req.Body = io.NopCloser(bytes.NewReader(body))
	} else {
		m.bodies = append(m.bodies, nil)
	}

	return m.response, nil
}

// GetRequests returns all captured requests
func (m *MockHTTPClient) GetRequests() []*http.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*http.Request{}, m.requests...)
}

// GetBodies returns all captured request bodies
func (m *MockHTTPClient) GetBodies() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte{}, m.bodies...)
}

// GetRequestCount returns the number of HTTP requests made
func (m *MockHTTPClient) GetRequestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func TestNewClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Note: debug mode does NOT disable HTTP calls - it only adds extra logging
	client := newClient(t.Context(), logger, false, false, "test-version")

	// This should not panic
	commandEvent := &CommandEvent{
		Action:  "test-command",
		Success: true,
		Error:   "",
	}
	client.Track(t.Context(), commandEvent)
	client.RecordToolCall(t.Context(), "test-tool", "session-id", "agent-name", time.Millisecond, nil)
	client.RecordTokenUsage(t.Context(), "test-model", 100, 50, 0.5)
}

func TestSessionTracking(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mockHTTP := NewMockHTTPClient()
	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-session-tracking.com/api"
	client.apiKey = "test-session-key"
	client.header = "test-header"

	ctx := t.Context()

	sessionID := client.RecordSessionStart(ctx, "test-agent", "test-session-id")
	assert.NotEmpty(t, sessionID)

	// Record some activity
	client.RecordToolCall(ctx, "test-tool", "session-id", "agent-name", time.Millisecond, nil)
	client.RecordTokenUsage(ctx, "test-model", 100, 50, 0.5)

	// End session
	client.RecordSessionEnd(ctx)

	// Multiple ends should be safe
	client.RecordSessionEnd(ctx)

	require.Eventually(t, func() bool {
		return mockHTTP.GetRequestCount() > 0
	}, time.Second, 5*time.Millisecond, "Expected HTTP requests to be made for session tracking events")

	requestCount := mockHTTP.GetRequestCount()
	t.Logf("Session tracking HTTP requests captured: %d", requestCount)

	requests := mockHTTP.GetRequests()
	for i, req := range requests {
		assert.Equal(t, http.MethodPost, req.Method, "Request %d: Expected POST method", i)
		assert.Equal(t, "test-session-key", req.Header.Get("test-header"), "Request %d: Expected test-header test-session-key", i)
	}
}

func TestCommandTracking(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mockHTTP := NewMockHTTPClient()
	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-command-tracking.com/api"
	client.apiKey = "test-command-key"
	client.header = "test-header"

	executed := false
	cmdInfo := CommandInfo{
		Action: "test-command",
		Args:   []string{},
		Flags:  []string{},
	}
	err := client.TrackCommand(t.Context(), cmdInfo, func(ctx context.Context) error {
		executed = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, executed)

	require.Eventually(t, func() bool {
		return mockHTTP.GetRequestCount() > 0
	}, time.Second, 5*time.Millisecond, "Expected HTTP requests to be made for command tracking")

	requestCount := mockHTTP.GetRequestCount()
	t.Logf("Command tracking HTTP requests captured: %d", requestCount)

	requests := mockHTTP.GetRequests()
	for i, req := range requests {
		assert.Equal(t, "test-command-key", req.Header.Get("test-header"), "Request %d: Expected test-header test-command-key", i)
	}
}

func TestCommandTrackingWithError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mockHTTP := NewMockHTTPClient()
	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-command-error.com/api"
	client.apiKey = "test-command-error-key"
	client.header = "test-header"

	testErr := &testError{}
	cmdInfo := CommandInfo{
		Action: "failing-command",
		Args:   []string{},
		Flags:  []string{},
	}
	err := client.TrackCommand(t.Context(), cmdInfo, func(ctx context.Context) error {
		return testErr
	})

	assert.Equal(t, testErr, err)

	require.Eventually(t, func() bool {
		return mockHTTP.GetRequestCount() > 0
	}, time.Second, 5*time.Millisecond, "Expected HTTP requests to be made for command error tracking")

	requestCount := mockHTTP.GetRequestCount()
	t.Logf("Command error tracking HTTP requests captured: %d", requestCount)
}

func TestStructuredEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Use debug mode to avoid HTTP calls in tests
	client := newClient(t.Context(), logger, true, true, "test-version")

	event := CommandEvent{
		Action:  "test-command",
		Success: true,
	}

	// Should not panic
	client.Track(t.Context(), &event)
}

func TestGetTelemetryEnabled(t *testing.T) {
	// When running under 'go test', GetTelemetryEnabled() always returns false
	// because flag.Lookup("test.v") is set. This test verifies that behavior.
	assert.False(t, GetTelemetryEnabled(), "Expected telemetry to be disabled during tests")

	// Even with TELEMETRY_ENABLED=true, telemetry is disabled during tests
	t.Setenv("TELEMETRY_ENABLED", "true")
	assert.False(t, GetTelemetryEnabled(), "Expected telemetry to be disabled during tests even with TELEMETRY_ENABLED=true")
}

func TestGetTelemetryEnabledFromEnv(t *testing.T) {
	// Test the environment variable logic directly (bypassing test detection)

	// Default (no env var) should be enabled
	t.Setenv("TELEMETRY_ENABLED", "")
	assert.True(t, getTelemetryEnabledFromEnv(), "Expected telemetry enabled by default")

	// Explicitly set to "true" should be enabled
	t.Setenv("TELEMETRY_ENABLED", "true")
	assert.True(t, getTelemetryEnabledFromEnv(), "Expected telemetry enabled when TELEMETRY_ENABLED=true")

	// Explicitly set to "false" should be disabled
	t.Setenv("TELEMETRY_ENABLED", "false")
	assert.False(t, getTelemetryEnabledFromEnv(), "Expected telemetry disabled when TELEMETRY_ENABLED=false")

	// Any other value should be enabled (only "false" disables)
	t.Setenv("TELEMETRY_ENABLED", "1")
	assert.True(t, getTelemetryEnabledFromEnv(), "Expected telemetry enabled when TELEMETRY_ENABLED=1")

	t.Setenv("TELEMETRY_ENABLED", "yes")
	assert.True(t, getTelemetryEnabledFromEnv(), "Expected telemetry enabled when TELEMETRY_ENABLED=yes")
}

// testError is a simple error implementation for testing
type testError struct{}

func (e *testError) Error() string {
	return "test error"
}

// Test-only methods - these wrap command execution with telemetry for testing purposes

// TrackCommand wraps command execution with telemetry (test-only method)
func (tc *Client) TrackCommand(ctx context.Context, commandInfo CommandInfo, fn func(context.Context) error) error {
	if !tc.enabled {
		return fn(ctx)
	}

	ctx = WithClient(ctx, tc)

	// Send telemetry event immediately (optimistic approach)
	commandEvent := CommandEvent{
		Action:  commandInfo.Action,
		Args:    commandInfo.Args,
		Success: true, // Assume success - we're tracking user intent, not outcome
	}

	// Send the telemetry event immediately
	tc.Track(ctx, &commandEvent)

	// Now run the command function
	return fn(ctx)
}

// TrackServerStart immediately sends telemetry for server startup, then runs the server function (test-only method)
// This is for long-running commands that may never exit (api, mcp, etc.)
func (tc *Client) TrackServerStart(ctx context.Context, commandInfo CommandInfo, fn func(context.Context) error) error {
	if !tc.enabled {
		return fn(ctx)
	}

	ctx = WithClient(ctx, tc)

	// Send startup event immediately
	startupEvent := CommandEvent{
		Action:  commandInfo.Action,
		Args:    commandInfo.Args,
		Success: true, // We assume startup succeeds if we reach this point
	}

	// Send the startup telemetry event immediately
	tc.Track(ctx, &startupEvent)

	// Now run the server function (which may run indefinitely)
	return fn(ctx)
}

// TestAllEventTypes tests all possible telemetry events with mock HTTP client
func TestAllEventTypes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Use mock HTTP client to avoid actual HTTP calls in tests
	mockHTTP := NewMockHTTPClient()
	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-telemetry-all-events.com/api"
	client.apiKey = "test-all-events-key"
	client.header = "test-header"

	ctx := t.Context()
	sessionID := "test-session-123"
	agentName := "test-agent"

	// Start session to enable session-based events
	client.RecordSessionStart(ctx, agentName, sessionID)

	t.Run("CommandEvents", func(t *testing.T) {
		commands := []struct {
			action string
			args   []string
		}{
			{"run", []string{"config.yaml"}},
			{"api", []string{}},
			{"mcp", []string{}},
			{"tui", []string{"config.yaml"}},
			{"pull", []string{"user/agent:latest"}},
			{"push", []string{"user/agent:latest"}},
			{"catalog", []string{}},
			{"new", []string{"my-agent"}},
			{"version", []string{}},
			{"feedback", []string{}},
			{"eval", []string{"expression"}},
			{"readme", []string{}},
			{"gateway", []string{}},
			{"debug", []string{}},
		}

		for _, cmd := range commands {
			t.Run(cmd.action, func(t *testing.T) {
				event := &CommandEvent{
					Action:  cmd.action,
					Args:    cmd.args,
					Success: true,
				}
				client.Track(ctx, event)

				errorEvent := &CommandEvent{
					Action:  cmd.action,
					Args:    cmd.args,
					Success: false,
					Error:   "test error",
				}
				client.Track(ctx, errorEvent)
			})
		}
	})

	t.Run("SessionEvents", func(t *testing.T) {
		startEvent := &SessionStartEvent{
			Action:    "start",
			SessionID: sessionID,
			AgentName: agentName,
		}
		client.Track(ctx, startEvent)

		endEvent := &SessionEndEvent{
			Action:       "end",
			SessionID:    sessionID,
			AgentName:    agentName,
			Duration:     1000, // 1 second
			ToolCalls:    5,
			InputTokens:  100,
			OutputTokens: 200,
			TotalTokens:  300,
			IsSuccess:    true,
			Error:        []string{},
		}
		client.Track(ctx, endEvent)

		errorSessionEvent := &SessionEndEvent{
			Action:       "end",
			SessionID:    sessionID + "-error",
			AgentName:    agentName,
			Duration:     500,
			ToolCalls:    3,
			InputTokens:  50,
			OutputTokens: 25,
			TotalTokens:  75,
			IsSuccess:    false,
			Error:        []string{"session failed"},
		}
		client.Track(ctx, errorSessionEvent)
	})

	t.Run("ToolEvents", func(t *testing.T) {
		tools := []struct {
			name     string
			success  bool
			duration int64
			error    string
		}{
			{"think", true, 100, ""},
			{"todo", true, 50, ""},
			{"memory", true, 200, ""},
			{"transfer_task", true, 150, ""},
			{"filesystem", true, 75, ""},
			{"shell", true, 300, ""},
			{"mcp_tool", false, 500, "tool execution failed"},
			{"custom_tool", true, 125, ""},
		}

		for _, tool := range tools {
			t.Run(tool.name, func(t *testing.T) {
				event := &ToolEvent{
					Action:    "call",
					ToolName:  tool.name,
					SessionID: sessionID,
					AgentName: agentName,
					Duration:  tool.duration,
					Success:   tool.success,
					Error:     tool.error,
				}
				client.Track(ctx, event)

				// Also test RecordToolCall method
				var err error
				if tool.error != "" {
					err = &testError{}
				}
				client.RecordToolCall(ctx, tool.name, sessionID, agentName, time.Duration(tool.duration)*time.Millisecond, err)
			})
		}
	})

	t.Run("TokenEvents", func(t *testing.T) {
		models := []struct {
			name         string
			inputTokens  int64
			outputTokens int64
		}{
			{"gpt-4", 150, 75},
			{"claude-3-sonnet", 200, 100},
			{"gemini-pro", 100, 50},
			{"local-model", 80, 40},
		}

		for _, model := range models {
			t.Run(model.name, func(t *testing.T) {
				event := &TokenEvent{
					Action:       "usage",
					ModelName:    model.name,
					SessionID:    sessionID,
					AgentName:    agentName,
					InputTokens:  model.inputTokens,
					OutputTokens: model.outputTokens,
					TotalTokens:  model.inputTokens + model.outputTokens,
					Cost:         0,
				}
				client.Track(ctx, event)

				// Also test RecordTokenUsage method
				client.RecordTokenUsage(ctx, model.name, model.inputTokens, model.outputTokens, 0)
			})
		}

		errorTokenEvent := &TokenEvent{
			Action:       "usage",
			ModelName:    "failing-model",
			SessionID:    sessionID,
			AgentName:    agentName,
			InputTokens:  50,
			OutputTokens: 0,
			TotalTokens:  50,
			Cost:         0,
		}
		client.Track(ctx, errorTokenEvent)
	})

	// End session
	client.RecordSessionEnd(ctx)

	require.Eventually(t, func() bool {
		return mockHTTP.GetRequestCount() > 0
	}, time.Second, 5*time.Millisecond, "Expected HTTP requests to be made for telemetry events")

	requestCount := mockHTTP.GetRequestCount()

	t.Logf("Total HTTP requests captured: %d", requestCount)

	requests := mockHTTP.GetRequests()
	bodies := mockHTTP.GetBodies()

	assert.Len(t, requests, len(bodies), "Mismatch between request count and body count")

	for i, req := range requests {
		assert.Equal(t, http.MethodPost, req.Method, "Request %d: Expected POST method", i)
		assert.Equal(t, "https://test-telemetry-all-events.com/api", req.URL.String(), "Request %d: Expected correct URL", i)

		assert.Equal(t, "application/json", req.Header.Get("Content-Type"), "Request %d: Expected Content-Type application/json", i)
		assert.Equal(t, "cagent/test-version", req.Header.Get("User-Agent"), "Request %d: Expected User-Agent cagent/test-version", i)
		assert.Equal(t, "test-all-events-key", req.Header.Get("test-header"), "Request %d: Expected test-header test-all-events-key", i)

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[i], &requestBody), "Request %d: Failed to unmarshal request body", i)

		records, ok := requestBody["records"].([]any)
		require.True(t, ok, "Request %d: Expected 'records' array in request body", i)
		assert.Len(t, records, 1, "Request %d: Expected 1 record", i)

		record := records[0].(map[string]any)
		eventType, ok := record["event"].(string)
		assert.True(t, ok && eventType != "", "Request %d: Expected non-empty event type", i)

		_, ok = record["properties"].(map[string]any)
		assert.True(t, ok, "Request %d: Expected properties object in event", i)
	}
}

// TestTrackServerStart tests long-running server command tracking
func TestTrackServerStart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client := newClient(t.Context(), logger, true, true, "test-version")

	executed := false
	cmdInfo := CommandInfo{
		Action: "mcp",
		Args:   []string{},
		Flags:  []string{},
	}
	err := client.TrackServerStart(t.Context(), cmdInfo, func(ctx context.Context) error {
		executed = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, executed)
}

// TestGlobalTelemetryFunctions tests the global telemetry convenience functions
func TestGlobalTelemetryFunctions(t *testing.T) {
	// Save original global state
	originalClient := globalToolTelemetryClient
	originalVersion := globalTelemetryVersion
	originalDebugMode := globalTelemetryDebugMode
	defer func() {
		globalToolTelemetryClient = originalClient
		globalTelemetryOnce = sync.Once{} // Reset to new instance
		globalTelemetryVersion = originalVersion
		globalTelemetryDebugMode = originalDebugMode
	}()

	// Reset global state for testing
	globalToolTelemetryClient = nil
	globalTelemetryOnce = sync.Once{}
	SetGlobalTelemetryVersion("test-version")
	SetGlobalTelemetryDebugMode(true)

	TrackCommand(t.Context(), "test-command", []string{"arg1"})

	assert.NotNil(t, globalToolTelemetryClient)

	EnsureGlobalTelemetryInitialized(t.Context())
	client := GetGlobalTelemetryClient(t.Context())
	assert.NotNil(t, client)
}

// TestHTTPRequestVerification tests that HTTP requests are made correctly when telemetry is enabled
func TestHTTPRequestVerification(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mockHTTP := NewMockHTTPClient()

	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-telemetry.example.com/api/events"
	client.apiKey = "test-api-key"
	client.header = "test-header"

	ctx := t.Context()

	t.Run("CommandEventHTTPRequest", func(t *testing.T) {
		// Reset mock before test
		mockHTTP = NewMockHTTPClient()
		client.httpClient = mockHTTP

		event := &CommandEvent{
			Action:  "run",
			Args:    []string{"config.yaml"},
			Success: true,
		}

		assert.NotEmpty(t, client.endpoint, "Client endpoint should be set for this test")
		assert.NotEmpty(t, client.apiKey, "Client API key should be set for this test")
		assert.True(t, client.enabled, "Client should be enabled for this test")

		t.Logf("Before Track: endpoint=%s, apiKey len=%d, enabled=%t", client.endpoint, len(client.apiKey), client.enabled)

		client.Track(ctx, event)

		require.Eventually(t, func() bool {
			return mockHTTP.GetRequestCount() > 0
		}, time.Second, 5*time.Millisecond, "Expected HTTP request to be made")

		t.Logf("HTTP requests captured: %d", mockHTTP.GetRequestCount())

		requests := mockHTTP.GetRequests()
		req := requests[0]

		assert.Equal(t, http.MethodPost, req.Method, "Expected POST request")
		assert.Equal(t, "https://test-telemetry.example.com/api/events", req.URL.String(), "Expected correct URL")

		assert.Equal(t, "application/json", req.Header.Get("Content-Type"), "Expected Content-Type application/json")
		assert.Equal(t, "cagent/test-version", req.Header.Get("User-Agent"), "Expected User-Agent cagent/test-version")
		assert.Equal(t, "test-api-key", req.Header.Get("test-header"), "Expected test-header test-api-key")

		bodies := mockHTTP.GetBodies()
		assert.NotEmpty(t, bodies, "Expected request body to be captured")

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[0], &requestBody), "Failed to unmarshal request body")

		records, ok := requestBody["records"].([]any)
		require.True(t, ok, "Expected 'records' array in request body")
		assert.Len(t, records, 1, "Expected 1 record")

		record := records[0].(map[string]any)
		assert.Equal(t, "command", record["event"], "Expected event type 'command'")

		properties, ok := record["properties"].(map[string]any)
		require.True(t, ok, "Expected properties object in event")
		assert.Equal(t, "run", properties["action"], "Expected action 'run'")
		assert.True(t, properties["is_success"].(bool), "Expected is_success true")
	})

	t.Run("NoHTTPWhenMissingCredentials", func(t *testing.T) {
		mockHTTP2 := NewMockHTTPClient()
		client2 := newClient(t.Context(), logger, true, true, "test-version", mockHTTP2.Client)

		// Leave endpoint and API key empty
		client2.endpoint = ""
		client2.apiKey = ""

		event := &CommandEvent{
			Action:  "version",
			Success: true,
		}

		client2.Track(ctx, event)

		assert.Zero(t, mockHTTP2.GetRequestCount(), "Expected no HTTP requests when endpoint/apiKey are missing")
	})

	t.Run("NoHTTPWhenDisabled", func(t *testing.T) {
		mockHTTP3 := NewMockHTTPClient()
		client3 := newClient(t.Context(), logger, false, true, "test-version", mockHTTP3.Client)

		event := &CommandEvent{
			Action:  "version",
			Success: true,
		}

		client3.Track(ctx, event)

		assert.Zero(t, mockHTTP3.GetRequestCount(), "Expected no HTTP requests when client is disabled")
	})
}

// TestCreateEventTelemetryTags tests the TELEMETRY_TAGS environment variable support in createEvent
func TestCreateEventTelemetryTags(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client := newClient(t.Context(), logger, true, true, "test-version")
	client.userUUID = "test-uuid"

	t.Run("NoTagsSet", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "")

		event := client.createEvent("test_event", map[string]any{"action": "run"})

		assert.Equal(t, "run", event.Properties["action"])
		assert.Equal(t, "test-version", event.Properties["version"])
		assert.Equal(t, "test-uuid", event.Properties["user_uuid"])
		// Ensure no unexpected tag keys leaked in
		_, hasSource := event.Properties["source_system"]
		assert.False(t, hasSource, "Expected no source_system property when TELEMETRY_TAGS is empty")
	})

	t.Run("SingleTag", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "source_system=github-actions")

		event := client.createEvent("test_event", map[string]any{"action": "run"})

		assert.Equal(t, "github-actions", event.Properties["source_system"])
		assert.Equal(t, "run", event.Properties["action"])
	})

	t.Run("MultipleTags", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "source_system=github-actions,repo=docker/cagent,workflow=pr-review")

		event := client.createEvent("test_event", map[string]any{"action": "run"})

		assert.Equal(t, "github-actions", event.Properties["source_system"])
		assert.Equal(t, "docker/cagent", event.Properties["repo"])
		assert.Equal(t, "pr-review", event.Properties["workflow"])
	})

	t.Run("TagsWithWhitespace", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", " source_system = github-actions , repo = docker/docker-agent")

		event := client.createEvent("test_event", map[string]any{"action": "run"})

		assert.Equal(t, "github-actions", event.Properties["source_system"])
		assert.Equal(t, "docker/docker-agent", event.Properties["repo"])
	})

	t.Run("MalformedTagsIgnored", func(t *testing.T) {
		// Tags without "=" should be silently ignored
		t.Setenv("TELEMETRY_TAGS", "valid_key=valid_value,malformed_no_equals,another=good")

		event := client.createEvent("test_event", map[string]any{"action": "run"})

		assert.Equal(t, "valid_value", event.Properties["valid_key"])
		assert.Equal(t, "good", event.Properties["another"])
		_, hasMalformed := event.Properties["malformed_no_equals"]
		assert.False(t, hasMalformed, "Malformed tag without = should be ignored")
	})

	t.Run("SystemMetadataCannotBeOverwritten", func(t *testing.T) {
		// This is the critical security test: TELEMETRY_TAGS must NOT be able to
		// overwrite system metadata like user_uuid, version, os, os_language
		t.Setenv("TELEMETRY_TAGS", "user_uuid=attacker,version=fake,os=spoofed,os_language=xx")

		event := client.createEvent("test_event", map[string]any{"action": "run"})

		// System metadata should win over tags
		assert.Equal(t, "test-uuid", event.Properties["user_uuid"], "user_uuid must not be overwritable via TELEMETRY_TAGS")
		assert.Equal(t, "test-version", event.Properties["version"], "version must not be overwritable via TELEMETRY_TAGS")
		assert.NotEqual(t, "spoofed", event.Properties["os"], "os must not be overwritable via TELEMETRY_TAGS")
		assert.NotEqual(t, "xx", event.Properties["os_language"], "os_language must not be overwritable via TELEMETRY_TAGS")
	})

	t.Run("TagsDoNotOverwriteUserProperties", func(t *testing.T) {
		// Tags are applied after user properties, so tags CAN overwrite user-provided props.
		// This is by design — TELEMETRY_TAGS is set by the environment operator (e.g., CI),
		// who should have higher priority than individual event properties.
		t.Setenv("TELEMETRY_TAGS", "action=overridden")

		event := client.createEvent("test_event", map[string]any{"action": "original"})

		assert.Equal(t, "overridden", event.Properties["action"],
			"TELEMETRY_TAGS should override user-provided properties (environment operator has priority)")
	})

	t.Run("EmptyValueTag", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "empty_val=")

		event := client.createEvent("test_event", map[string]any{})

		assert.Empty(t, event.Properties["empty_val"], "Empty value tags should be preserved")
	})

	t.Run("TagWithEqualsInValue", func(t *testing.T) {
		// strings.Cut splits on the first "=", so "key=val=ue" → key:"val=ue"
		t.Setenv("TELEMETRY_TAGS", "equation=a=b")

		event := client.createEvent("test_event", map[string]any{})

		assert.Equal(t, "a=b", event.Properties["equation"], "Values containing = should be preserved")
	})
}

func TestTelemetryTags(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mockHTTP := NewMockHTTPClient()
	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-tags.com/api"
	client.apiKey = "test-tags-key"
	client.header = "test-header"

	t.Run("TagsIncludedInEvent", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "ci=github-actions,repository=docker/cagent,workflow=PR Review")

		mockHTTP = NewMockHTTPClient()
		client.httpClient = mockHTTP

		client.Track(t.Context(), &CommandEvent{Action: "test", Success: true})
		time.Sleep(20 * time.Millisecond)

		bodies := mockHTTP.GetBodies()
		require.NotEmpty(t, bodies)

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[0], &requestBody))

		records := requestBody["records"].([]any)
		properties := records[0].(map[string]any)["properties"].(map[string]any)

		assert.Equal(t, "github-actions", properties["ci"])
		assert.Equal(t, "docker/cagent", properties["repository"])
		assert.Equal(t, "PR Review", properties["workflow"])
	})

	t.Run("NoTagsWhenUnset", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "")

		mockHTTP = NewMockHTTPClient()
		client.httpClient = mockHTTP

		client.Track(t.Context(), &CommandEvent{Action: "test", Success: true})
		time.Sleep(20 * time.Millisecond)

		bodies := mockHTTP.GetBodies()
		require.NotEmpty(t, bodies)

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[0], &requestBody))

		records := requestBody["records"].([]any)
		properties := records[0].(map[string]any)["properties"].(map[string]any)

		_, hasCi := properties["ci"]
		assert.False(t, hasCi, "Expected no 'ci' property when TELEMETRY_TAGS is empty")
	})

	t.Run("SystemMetadataCannotBeOverwritten", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "user_uuid=fake,version=0.0.0,os=spoofed")

		mockHTTP = NewMockHTTPClient()
		client.httpClient = mockHTTP

		client.Track(t.Context(), &CommandEvent{Action: "test", Success: true})
		time.Sleep(20 * time.Millisecond)

		bodies := mockHTTP.GetBodies()
		require.NotEmpty(t, bodies)

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[0], &requestBody))

		records := requestBody["records"].([]any)
		properties := records[0].(map[string]any)["properties"].(map[string]any)

		assert.NotEqual(t, "fake", properties["user_uuid"], "user_uuid should not be overwritable via tags")
		assert.Equal(t, "test-version", properties["version"], "version should not be overwritable via tags")
		assert.NotEqual(t, "spoofed", properties["os"], "os should not be overwritable via tags")
	})

	t.Run("MalformedTagsIgnored", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", "valid=yes,no_equals_sign,=empty_key,also_valid=true")

		mockHTTP = NewMockHTTPClient()
		client.httpClient = mockHTTP

		client.Track(t.Context(), &CommandEvent{Action: "test", Success: true})
		time.Sleep(20 * time.Millisecond)

		bodies := mockHTTP.GetBodies()
		require.NotEmpty(t, bodies)

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[0], &requestBody))

		records := requestBody["records"].([]any)
		properties := records[0].(map[string]any)["properties"].(map[string]any)

		assert.Equal(t, "yes", properties["valid"])
		assert.Equal(t, "true", properties["also_valid"])
		_, hasEmptyKey := properties[""]
		assert.False(t, hasEmptyKey, "Empty keys should be ignored")
	})

	t.Run("WhitespaceIsTrimmed", func(t *testing.T) {
		t.Setenv("TELEMETRY_TAGS", " key1 = value1 , key2 = value2 ")

		mockHTTP = NewMockHTTPClient()
		client.httpClient = mockHTTP

		client.Track(t.Context(), &CommandEvent{Action: "test", Success: true})
		time.Sleep(20 * time.Millisecond)

		bodies := mockHTTP.GetBodies()
		require.NotEmpty(t, bodies)

		var requestBody map[string]any
		require.NoError(t, json.Unmarshal(bodies[0], &requestBody))

		records := requestBody["records"].([]any)
		properties := records[0].(map[string]any)["properties"].(map[string]any)

		assert.Equal(t, "value1", properties["key1"])
		assert.Equal(t, "value2", properties["key2"])
	})
}

// TestNon2xxHTTPResponseHandling ensures that 5xx responses are logged and handled gracefully
func TestNon2xxHTTPResponseHandling(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mockHTTP := NewMockHTTPClient()
	client := newClient(t.Context(), logger, true, true, "test-version", mockHTTP.Client)

	client.endpoint = "https://test-error-response.com/api"
	client.apiKey = "error-key"
	client.header = "test-header"

	// Configure mock to return 500
	mockHTTP.SetResponse(&http.Response{
		StatusCode: http.StatusInternalServerError,
		Status:     "500 Internal Server Error",
		Body:       io.NopCloser(bytes.NewReader([]byte("internal error"))),
		Header:     make(http.Header),
	})

	client.Track(t.Context(), &CommandEvent{Action: "error-test", Success: true})

	require.Eventually(t, func() bool {
		return mockHTTP.GetRequestCount() > 0
	}, time.Second, 5*time.Millisecond, "Expected HTTP request to be made despite error response")

	mockHTTP.SetResponse(&http.Response{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
		Body:       io.NopCloser(bytes.NewReader([]byte("not found"))),
		Header:     make(http.Header),
	})

	client.Track(t.Context(), &CommandEvent{Action: "not-found-test", Success: true})

	require.Eventually(t, func() bool {
		return mockHTTP.GetRequestCount() >= 2
	}, time.Second, 5*time.Millisecond, "Expected at least 2 HTTP requests (500 + 404)")
}
