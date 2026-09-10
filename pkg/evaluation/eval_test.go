package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/session"
)

func TestToolCallF1Score(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expected []string
		actual   []string
		want     float64
	}{
		{
			name:     "empty tool calls",
			expected: []string{},
			actual:   []string{},
			want:     1.0,
		},
		{
			name:     "perfect match single tool call",
			expected: []string{"search"},
			actual:   []string{"search"},
			want:     1.0,
		},
		{
			name:     "different tool names",
			expected: []string{"search"},
			actual:   []string{"read_file"},
			want:     0.0,
		},
		{
			name:     "multiple tool calls all match",
			expected: []string{"search", "read_file"},
			actual:   []string{"search", "read_file"},
			want:     1.0,
		},
		{
			name:     "multiple tool calls 1 out of 2 match",
			expected: []string{"search", "read_file"},
			actual:   []string{"search", "write_file"},
			want:     0.5,
		},
		{
			name:     "more expected than actual",
			expected: []string{"search", "read_file"},
			actual:   []string{"search"},
			want:     0.6666666666666666,
		},
		{
			name:     "more actual than expected",
			expected: []string{"search"},
			actual:   []string{"search", "read_file"},
			want:     0.6666666666666666,
		},
		{
			name:     "order does not matter for F1",
			expected: []string{"search", "read_file"},
			actual:   []string{"read_file", "search"},
			want:     1.0,
		},
		{
			name:     "expected has no tool calls",
			expected: []string{},
			actual:   []string{"search"},
			want:     0.0,
		},
		{
			name:     "actual has no tool calls",
			expected: []string{"search"},
			actual:   []string{},
			want:     0.0,
		},
		{
			name:     "duplicate tool calls handled",
			expected: []string{"search", "search", "read_file"},
			actual:   []string{"search", "read_file", "read_file"},
			want:     0.6666666666666666,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toolCallF1Score(tt.expected, tt.actual)
			assert.InDelta(t, tt.want, got, 0.0001)
		})
	}
}

func TestGetResponseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response string
		want     string
	}{
		{"empty response is S", "", "S"},
		{"short response is S", "Hello, world!", "S"},
		{"medium response is M", string(make([]byte, 600)), "M"},
		{"long response is L", string(make([]byte, 2000)), "L"},
		{"extra long response is XL", string(make([]byte, 6000)), "XL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := getResponseSize(tt.response)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseJudgeResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		text       string
		wantPassed bool
		wantReason string
		wantErr    bool
	}{
		{"simple pass", `{"result": "pass", "reason": "good"}`, true, "good", false},
		{"simple fail", `{"result": "fail", "reason": "bad"}`, false, "bad", false},
		{"pass uppercase", `{"result": "PASS", "reason": "good"}`, true, "good", false},
		{"fail uppercase", `{"result": "FAIL", "reason": "bad"}`, false, "bad", false},
		{"pass mixed case", `{"result": "Pass", "reason": "good"}`, true, "good", false},
		{"invalid json returns error", `not json at all`, false, "", true},
		{"empty result returns false", `{"result": "", "reason": "empty"}`, false, "empty", false},
		{"missing result field", `{"reason": "no result field"}`, false, "no result field", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			passed, reason, err := parseJudgeResponse(tt.text)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantPassed, passed)
				assert.Equal(t, tt.wantReason, reason)
			}
		})
	}
}

func TestResultCheckResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		result       Result
		wantSuccess  []string
		wantFailures []string
	}{
		{
			name:         "error takes precedence",
			result:       Result{Error: "failed to run"},
			wantSuccess:  nil,
			wantFailures: []string{"failed to run"},
		},
		{
			name:         "all checks pass",
			result:       Result{SizeExpected: "M", Size: "M", ToolCallsExpected: 1, ToolCallsScore: 1.0, RelevanceExpected: 2, RelevancePassed: 2},
			wantSuccess:  []string{"size M", "tool calls", "relevance 2/2"},
			wantFailures: nil,
		},
		{
			name:         "size mismatch",
			result:       Result{SizeExpected: "M", Size: "S"},
			wantSuccess:  nil,
			wantFailures: []string{"size expected M, got S"},
		},
		{
			name:         "tool calls failed",
			result:       Result{ToolCallsExpected: 1, ToolCallsScore: 0.5},
			wantSuccess:  nil,
			wantFailures: []string{"tool calls score 0.50"},
		},
		{
			name:         "relevance failures listed",
			result:       Result{RelevanceExpected: 2, RelevancePassed: 0, RelevanceResults: []RelevanceResult{{Criterion: "check A", Passed: false, Reason: "reason A"}, {Criterion: "check B", Passed: false, Reason: "reason B"}}},
			wantSuccess:  nil,
			wantFailures: []string{"relevance: check A (reason: reason A)", "relevance: check B (reason: reason B)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			successes, failures := tt.result.checkResults()
			assert.Equal(t, tt.wantSuccess, successes)
			assert.Equal(t, tt.wantFailures, failures)
		})
	}
}

func TestComputeSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		results            []Result
		wantTotalCost      float64
		wantTotalEvals     int
		wantSizesPassed    int
		wantSizesTotal     int
		wantRelevance      float64
		wantRelevanceTotal float64
	}{
		{
			name:            "no results",
			results:         []Result{},
			wantTotalCost:   0,
			wantTotalEvals:  0,
			wantSizesPassed: 0,
			wantSizesTotal:  0,
		},
		{
			name: "all passed",
			results: []Result{
				{
					Title:        "session1",
					Cost:         0.01,
					SizeExpected: "M",
					Size:         "M",
				},
			},
			wantTotalCost:   0.01,
			wantTotalEvals:  1,
			wantSizesPassed: 1,
			wantSizesTotal:  1,
		},
		{
			name: "size mismatch",
			results: []Result{
				{
					Title:        "session1",
					SizeExpected: "M",
					Size:         "S",
				},
			},
			wantTotalEvals:  1,
			wantSizesPassed: 0,
			wantSizesTotal:  1,
		},
		{
			name: "multiple sessions",
			results: []Result{
				{Title: "session1", Cost: 0.01, SizeExpected: "M", Size: "M"},
				{Title: "session2", Cost: 0.02, SizeExpected: "L", Size: "S"},
				{Title: "session3", Cost: 0.03},
			},
			wantTotalCost:   0.06,
			wantTotalEvals:  3,
			wantSizesPassed: 1,
			wantSizesTotal:  2,
		},
		{
			name: "errored results excluded from totals",
			results: []Result{
				{Title: "session1", Cost: 0.01, SizeExpected: "M", Size: "M", RelevanceExpected: 2, RelevancePassed: 2},
				{Title: "session2", Cost: 0.02, Error: "docker build failed", SizeExpected: "L", RelevanceExpected: 2},
				{Title: "session3", Cost: 0.00, Error: "timeout", RelevanceExpected: 3},
			},
			wantTotalCost:      0.03, // cost is still counted
			wantTotalEvals:     3,
			wantSizesPassed:    1,
			wantSizesTotal:     1, // only non-errored results count
			wantRelevance:      2,
			wantRelevanceTotal: 2, // only non-errored results count
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			summary := computeSummary(tt.results)
			assert.Equal(t, tt.wantTotalEvals, summary.TotalEvals)
			assert.InDelta(t, tt.wantTotalCost, summary.TotalCost, 0.0001)
			assert.Equal(t, tt.wantSizesPassed, summary.SizesPassed)
			assert.Equal(t, tt.wantSizesTotal, summary.SizesTotal)
			assert.InDelta(t, tt.wantRelevance, summary.RelevancePassed, 0.0001)
			assert.InDelta(t, tt.wantRelevanceTotal, summary.RelevanceTotal, 0.0001)
		})
	}
}

func TestGenerateRunName(t *testing.T) {
	t.Parallel()

	// Pattern: adjective-noun-number (e.g., swift-falcon-042)
	pattern := regexp.MustCompile(`^[a-z]+-[a-z]+-\d{3}$`)

	// Generate multiple names and verify format
	names := make(map[string]bool)
	for range 100 {
		name := GenerateRunName()
		assert.Regexp(t, pattern, name, "run name should match pattern adjective-noun-NNN")
		names[name] = true
	}

	// Should generate unique names (with high probability)
	assert.Greater(t, len(names), 90, "should generate mostly unique names")
}

func TestSaveRunJSON(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()

	run := &EvalRun{
		Name:      "test-run-001",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Duration:  5 * time.Minute,
		Results: []Result{
			{Title: "test1", Cost: 0.01},
			{Title: "test2", Cost: 0.02, Error: "failed"},
		},
		Summary: Summary{
			TotalEvals: 2,
			TotalCost:  0.03,
		},
	}

	// Save the run
	resultsPath, err := SaveRunJSON(run, outputDir)
	require.NoError(t, err)

	// Verify file path
	assert.Equal(t, filepath.Join(outputDir, "test-run-001.json"), resultsPath)

	// Verify file exists and contains valid JSON
	data, err := os.ReadFile(resultsPath)
	require.NoError(t, err)

	var loaded EvalRun
	err = json.Unmarshal(data, &loaded)
	require.NoError(t, err)

	// Verify content
	assert.Equal(t, run.Name, loaded.Name)
	assert.Len(t, loaded.Results, len(run.Results))
	assert.Equal(t, run.Summary.TotalEvals, loaded.Summary.TotalEvals)
	assert.InDelta(t, run.Summary.TotalCost, loaded.Summary.TotalCost, 0.0001)
}

func TestSaveRunJSONCreatesDirectory(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	nestedDir := filepath.Join(baseDir, "nested", "results")

	run := &EvalRun{
		Name:    "test-run-002",
		Results: []Result{},
	}

	resultsPath, err := SaveRunJSON(run, nestedDir)
	require.NoError(t, err)
	assert.FileExists(t, resultsPath)
}

func TestParseContainerEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		events           []map[string]any
		wantResponse     string
		wantCost         float64
		wantOutputTokens int64
		wantToolCalls    []string
	}{
		{
			name:             "empty events",
			events:           []map[string]any{},
			wantResponse:     "",
			wantCost:         0,
			wantOutputTokens: 0,
			wantToolCalls:    nil,
		},
		{
			name: "agent choice events",
			events: []map[string]any{
				{"type": "agent_choice", "content": "Hello "},
				{"type": "agent_choice", "content": "world!"},
			},
			wantResponse:     "Hello world!",
			wantCost:         0,
			wantOutputTokens: 0,
			wantToolCalls:    nil,
		},
		{
			name: "tool call events",
			events: []map[string]any{
				{
					"type": "tool_call",
					"tool_call": map[string]any{
						"function": map[string]any{
							"name": "read_file",
						},
					},
				},
				{
					"type": "tool_call",
					"tool_call": map[string]any{
						"function": map[string]any{
							"name": "transfer_task",
						},
					},
				},
			},
			wantResponse:     "",
			wantCost:         0,
			wantOutputTokens: 0,
			wantToolCalls:    []string{"read_file", "transfer_task"},
		},
		{
			name: "token usage events",
			events: []map[string]any{
				{
					"type": "token_usage",
					"usage": map[string]any{
						"cost":          0.005,
						"output_tokens": float64(100),
					},
				},
				{
					"type": "token_usage",
					"usage": map[string]any{
						"cost":          0.008,
						"output_tokens": float64(50),
					},
				},
			},
			wantResponse:     "",
			wantCost:         0.008,
			wantOutputTokens: 150,
			wantToolCalls:    nil,
		},
		{
			name: "mixed events",
			events: []map[string]any{
				{"type": "agent_choice", "content": "Let me help."},
				{
					"type": "tool_call",
					"tool_call": map[string]any{
						"function": map[string]any{"name": "search"},
					},
				},
				{
					"type": "token_usage",
					"usage": map[string]any{
						"cost":          0.01,
						"output_tokens": float64(200),
					},
				},
			},
			wantResponse:     "Let me help.",
			wantCost:         0.01,
			wantOutputTokens: 200,
			wantToolCalls:    []string{"search"},
		},
		{
			name: "unknown event types ignored",
			events: []map[string]any{
				{"type": "unknown", "data": "ignored"},
				{"type": "agent_choice", "content": "Valid"},
			},
			wantResponse:     "Valid",
			wantCost:         0,
			wantOutputTokens: 0,
			wantToolCalls:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			response, cost, outputTokens, toolCalls := parseContainerEvents(tt.events)
			assert.Equal(t, tt.wantResponse, response)
			assert.InDelta(t, tt.wantCost, cost, 0.0001)
			assert.Equal(t, tt.wantOutputTokens, outputTokens)
			assert.Equal(t, tt.wantToolCalls, toolCalls)
		})
	}
}

func TestPrintSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		summary        Summary
		duration       time.Duration
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "all evaluations failed with errors",
			summary: Summary{
				TotalEvals:  10,
				FailedEvals: 10,
			},
			duration: 30 * time.Second,
			wantContains: []string{
				"Errors: 10/10 evaluations failed",
				"Total Cost: $0.000000",
				"Total Time: 30s",
			},
		},
		{
			name: "some evaluations failed",
			summary: Summary{
				TotalEvals:      10,
				FailedEvals:     5,
				TotalCost:       0.05,
				RelevancePassed: 8,
				RelevanceTotal:  10,
			},
			duration: 2 * time.Minute,
			wantContains: []string{
				"Errors: 5/10 evaluations failed",
				"Relevance: 8/10 passed",
				"Total Cost: $0.050000",
				"Total Time: 2m0s",
			},
		},
		{
			name: "all evaluations successful",
			summary: Summary{
				TotalEvals:      5,
				TotalCost:       0.1,
				SizesPassed:     4,
				SizesTotal:      5,
				RelevancePassed: 10,
				RelevanceTotal:  10,
			},
			duration: 1 * time.Minute,
			wantContains: []string{
				"Sizes: 4/5 passed",
				"Relevance: 10/10 passed",
				"Total Cost: $0.100000",
			},
			wantNotContain: []string{
				"Errors:",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			printSummary(&buf, tt.summary, tt.duration)
			output := buf.String()

			for _, want := range tt.wantContains {
				assert.Contains(t, output, want)
			}
			for _, notWant := range tt.wantNotContain {
				assert.NotContains(t, output, notWant)
			}
		})
	}
}

func TestProgressBarColors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		isTTY     bool
		wantGreen string
		wantRed   string
	}{
		{
			name:      "TTY output has color codes",
			isTTY:     true,
			wantGreen: "\x1b[32mtest\x1b[0m",
			wantRed:   "\x1b[31mtest\x1b[0m",
		},
		{
			name:      "non-TTY output has no color codes",
			isTTY:     false,
			wantGreen: "test",
			wantRed:   "test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			p := newProgressBar(&buf, &buf, 0, 10, tt.isTTY)

			assert.Equal(t, tt.wantGreen, p.green("test"))
			assert.Equal(t, tt.wantRed, p.red("test"))
		})
	}
}

func TestProgressBarPrintResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		result       Result
		wantContains []string
	}{
		{
			name: "successful result",
			result: Result{
				Title: "test-session",
				Cost:  0.005,
			},
			wantContains: []string{
				"✓ test-session",
				"$0.005000",
			},
		},
		{
			name: "failed result with error",
			result: Result{
				Title: "failed-session",
				Cost:  0,
				Error: "container failed",
			},
			wantContains: []string{
				"✗ failed-session",
				"✗ container failed",
			},
		},
		{
			name: "result with mixed successes and failures",
			result: Result{
				Title:             "mixed-session",
				Cost:              0.01,
				SizeExpected:      "M",
				Size:              "S",
				RelevanceExpected: 2,
				RelevancePassed:   1,
				RelevanceResults:  []RelevanceResult{{Criterion: "check failed", Passed: false, Reason: "did not meet criteria"}},
			},
			wantContains: []string{
				"✗ mixed-session", // overall failed
				"✗ size expected M, got S",
				"✗ relevance: check failed (reason: did not meet criteria)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			p := newProgressBar(&buf, &buf, 0, 10, false) // non-TTY for simpler output
			p.printResult(tt.result)
			output := buf.String()

			for _, want := range tt.wantContains {
				assert.Contains(t, output, want)
			}
		})
	}
}

func TestProgressBarCompleteCountsBasedOnCheckResults(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressBar(&buf, &buf, 0, 10, false)

	// Complete with a result that has no error but failed checks
	p.complete("test1", false) // failed checks
	p.complete("test2", true)  // passed checks
	p.complete("test3", false) // failed checks

	assert.Equal(t, int32(3), p.completed.Load())
	assert.Equal(t, int32(1), p.passed.Load())
	assert.Equal(t, int32(2), p.failed.Load())
}

func TestStatusIcon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ratio float64
		want  string
	}{
		{1.0, "✅"},
		{0.8, "✅"},
		{0.76, "✅"},
		{0.75, "⚠️"},
		{0.6, "⚠️"},
		{0.51, "⚠️"},
		{0.50, "❌"},
		{0.25, "❌"},
		{0.0, "❌"},
	}

	for _, tt := range tests {
		t.Run(strings.ReplaceAll(string(rune(int(tt.ratio*100))), "%", "pct"), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, statusIcon(tt.ratio))
		})
	}
}

func TestBuildTranscript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		events       []map[string]any
		wantContains []string
		wantOrder    []string // substrings that must appear in this order
	}{
		{
			name:         "empty events",
			events:       []map[string]any{},
			wantContains: nil,
		},
		{
			name: "text before tool call",
			events: []map[string]any{
				{"type": "agent_choice", "content": "I'll search for that.", "agent_name": "root"},
				{
					"type":       "tool_call",
					"agent_name": "root",
					"tool_call": map[string]any{
						"function": map[string]any{
							"name":      "search",
							"arguments": `{"query": "test"}`,
						},
					},
				},
			},
			wantContains: []string{
				"[Agent root says]",
				"I'll search for that.",
				`[Agent root calls tool "search" with arguments:`,
			},
			wantOrder: []string{"I'll search for that.", "calls tool"},
		},
		{
			name: "tool call before text (wrong order)",
			events: []map[string]any{
				{
					"type":       "tool_call",
					"agent_name": "Coding agent",
					"tool_call": map[string]any{
						"function": map[string]any{
							"name":      "shell",
							"arguments": `{"cmd": "ls"}`,
						},
					},
				},
				{"type": "agent_choice", "content": "I ran the command.", "agent_name": "Coding agent"},
			},
			wantContains: []string{
				`[Agent Coding agent calls tool "shell" with arguments:`,
				"[Agent Coding agent says]",
				"I ran the command.",
			},
			wantOrder: []string{"calls tool", "I ran the command."},
		},
		{
			name: "tool call response included",
			events: []map[string]any{
				{
					"type": "tool_call",
					"tool_call": map[string]any{
						"function": map[string]any{
							"name":      "read_file",
							"arguments": `{"path": "test.txt"}`,
						},
					},
				},
				{
					"type":         "tool_call_response",
					"response":     "file contents here",
					"tool_call_id": "call_123",
					"tool_definition": map[string]any{
						"name": "read_file",
					},
				},
			},
			wantContains: []string{
				`calls tool "read_file" with arguments:`,
				`[Tool "read_file" returns: file contents here]`,
			},
		},
		{
			name: "long tool response truncated",
			events: []map[string]any{
				{
					"type":         "tool_call_response",
					"response":     strings.Repeat("x", 600),
					"tool_call_id": "call_789",
					"tool_definition": map[string]any{
						"name": "shell",
					},
				},
			},
			wantContains: []string{
				"...(truncated)",
			},
		},
		{
			name: "agent switch flushes text",
			events: []map[string]any{
				{"type": "agent_choice", "content": "Handing off.", "agent_name": "root"},
				{
					"type":       "tool_call",
					"agent_name": "root",
					"tool_call": map[string]any{
						"function": map[string]any{
							"name":      "handoff",
							"arguments": `{}`,
						},
					},
				},
				{"type": "agent_choice", "content": "I'll help.", "agent_name": "Coding agent"},
			},
			wantContains: []string{
				"[Agent root says]",
				"Handing off.",
				"[Agent Coding agent says]",
				"I'll help.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transcript := buildTranscript(tt.events)

			for _, want := range tt.wantContains {
				assert.Contains(t, transcript, want)
			}

			// Verify ordering
			if len(tt.wantOrder) > 1 {
				lastIdx := -1
				for _, substr := range tt.wantOrder {
					idx := strings.Index(transcript, substr)
					assert.Greater(t, idx, lastIdx, "expected %q to appear after previous substring in transcript", substr)
					lastIdx = idx
				}
			}
		})
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileName string
		patterns []string
		want     bool
	}{
		{
			name:     "empty patterns matches nothing",
			fileName: "test-session.json",
			patterns: []string{},
			want:     false,
		},
		{
			name:     "exact match",
			fileName: "test-session.json",
			patterns: []string{"test-session.json"},
			want:     true,
		},
		{
			name:     "substring match",
			fileName: "my-test-session-1.json",
			patterns: []string{"test"},
			want:     true,
		},
		{
			name:     "case insensitive match",
			fileName: "MyTestSession.json",
			patterns: []string{"mytestsession"},
			want:     true,
		},
		{
			name:     "case insensitive pattern",
			fileName: "test-session.json",
			patterns: []string{"TEST"},
			want:     true,
		},
		{
			name:     "no match",
			fileName: "test-session.json",
			patterns: []string{"other"},
			want:     false,
		},
		{
			name:     "multiple patterns first matches",
			fileName: "test-session.json",
			patterns: []string{"test", "other"},
			want:     true,
		},
		{
			name:     "multiple patterns second matches",
			fileName: "test-session.json",
			patterns: []string{"other", "session"},
			want:     true,
		},
		{
			name:     "multiple patterns none match",
			fileName: "test-session.json",
			patterns: []string{"foo", "bar"},
			want:     false,
		},
		{
			name:     "match without extension",
			fileName: "test-session.json",
			patterns: []string{"test-session"},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := matchesAnyPattern(tt.fileName, tt.patterns)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRunDockerAgentInContainerCancelInterruptsDocker verifies that
// canceling the context interrupts the docker CLI with SIGINT and returns
// promptly, instead of killing it with SIGKILL (which docker never proxies
// to the container) or waiting on it forever.
//
// No Docker daemon is involved: a fake docker executable on PATH re-execs
// this test binary as a helper process (see
// TestRunDockerAgentInContainerHelperProcess) that waits for os.Interrupt.
func TestRunDockerAgentInContainerCancelInterruptsDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker executable is a POSIX shell script")
	}

	testBin, err := os.Executable()
	require.NoError(t, err)

	tmpDir := t.TempDir()
	startedMarker := filepath.Join(tmpDir, "started")
	interruptMarker := filepath.Join(tmpDir, "interrupted")

	// The wrapper script execs the test binary so that the SIGINT sent by
	// cmd.Cancel reaches the helper process directly.
	binDir := filepath.Join(tmpDir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))
	script := "#!/bin/sh\nexec \"" + testBin + "\" -test.run='^TestRunDockerAgentInContainerHelperProcess$'\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755))

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("EVAL_HELPER_STARTED_MARKER", startedMarker)
	t.Setenv("EVAL_HELPER_INTERRUPT_MARKER", interruptMarker)

	runner := newRunner(
		config.NewFileSource(filepath.Join(tmpDir, "agent.yaml")),
		&config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()},
		nil,
		Config{},
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := runner.runDockerAgentInContainer(ctx, "image-id", []string{"question"}, "")
		errCh <- err
	}()

	// Wait until the helper reports that its os.Interrupt handler is
	// installed; interrupting it earlier would kill it via the default
	// signal disposition.
	require.Eventually(t, func() bool {
		_, err := os.Stat(startedMarker)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "fake docker process never started")

	cancel()

	// The call must return promptly, well before cmd.WaitDelay expires.
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("runDockerAgentInContainer did not return after context cancellation")
	}

	assert.FileExists(t, interruptMarker, "fake docker process was not interrupted")
}

// TestRunDockerAgentInContainerHelperProcess is not a real test. It is
// re-executed by the fake docker script above and stands in for the docker
// CLI: it installs an os.Interrupt handler, reports readiness through the
// started marker, and records the received interrupt through the interrupt
// marker.
func TestRunDockerAgentInContainerHelperProcess(*testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	if err := os.WriteFile(os.Getenv("EVAL_HELPER_STARTED_MARKER"), nil, 0o600); err != nil {
		os.Exit(1)
	}

	select {
	case <-interrupt:
		if err := os.WriteFile(os.Getenv("EVAL_HELPER_INTERRUPT_MARKER"), nil, 0o600); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case <-time.After(30 * time.Second):
		// Bail out so a regression cannot hang the test binary.
		os.Exit(1)
	}
}

func TestContainerRuntimeOrDefault(t *testing.T) {
	t.Parallel()

	empty := Config{}
	assert.Equal(t, "docker", empty.containerRuntimeOrDefault(), "empty config must fall back to docker")

	custom := Config{ContainerRuntime: "podman"}
	assert.Equal(t, "podman", custom.containerRuntimeOrDefault())
}

// writeFakeContainerRuntime writes a POSIX shell script standing in for a
// Docker-compatible container runtime CLI: it records its arguments to
// argsFile and prints output on stdout. No daemon is involved.
func writeFakeContainerRuntime(t *testing.T, path, argsFile, output string) {
	t.Helper()
	script := "#!/bin/sh\necho \"$@\" > \"" + argsFile + "\"\necho '" + output + "'\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
}

// TestRunDockerAgentInContainerUsesConfiguredRuntime proves that container
// runs are executed with the configured runtime executable instead of the
// docker CLI.
func TestRunDockerAgentInContainerUsesConfiguredRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake container runtime executable is a POSIX shell script")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args")
	fakeRuntime := filepath.Join(tmpDir, "fake-podman")
	writeFakeContainerRuntime(t, fakeRuntime, argsFile, `{"type":"agent_choice","content":"ok"}`)

	runner := newRunner(
		config.NewFileSource(filepath.Join(tmpDir, "agent.yaml")),
		&config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()},
		nil,
		Config{ContainerRuntime: fakeRuntime},
	)

	events, err := runner.runDockerAgentInContainer(t.Context(), "image-id", []string{"question"}, "")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "agent_choice", events[0]["type"])

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	got := string(args)
	assert.True(t, strings.HasPrefix(got, "run "), "fake runtime must receive the run subcommand, got: %s", got)
	assert.Contains(t, got, "image-id")
}

// TestBuildEvalImageUsesConfiguredRuntime proves that image builds shell out
// to the configured runtime executable instead of the docker CLI.
func TestBuildEvalImageUsesConfiguredRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake container runtime executable is a POSIX shell script")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args")
	fakeRuntime := filepath.Join(tmpDir, "fake-podman")
	writeFakeContainerRuntime(t, fakeRuntime, argsFile, "sha256:fake-image-id")

	evalsDir := filepath.Join(tmpDir, "evals")
	require.NoError(t, os.Mkdir(evalsDir, 0o755))

	runner := newRunner(
		config.NewFileSource(filepath.Join(tmpDir, "agent.yaml")),
		&config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()},
		nil,
		Config{EvalsDir: evalsDir, ContainerRuntime: fakeRuntime},
	)

	imageID, err := runner.buildEvalImage(t.Context(), &session.EvalCriteria{})
	require.NoError(t, err)
	assert.Equal(t, "sha256:fake-image-id", imageID)

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "build -q -f- .", strings.TrimSpace(string(args)))
}

func TestNeedsJudge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		evals []InputSession
		want  bool
	}{
		{
			name:  "no evals",
			evals: nil,
			want:  false,
		},
		{
			name: "evals without relevance criteria",
			evals: []InputSession{
				{Session: &session.Session{Evals: &session.EvalCriteria{Size: "M"}}},
				{Session: &session.Session{Evals: &session.EvalCriteria{}}},
			},
			want: false,
		},
		{
			name: "evals with nil Evals field",
			evals: []InputSession{
				{Session: &session.Session{}},
			},
			want: false,
		},
		{
			name: "some evals with relevance criteria",
			evals: []InputSession{
				{Session: &session.Session{Evals: &session.EvalCriteria{}}},
				{Session: &session.Session{Evals: &session.EvalCriteria{Relevance: []string{"criterion1"}}}},
			},
			want: true,
		},
		{
			name: "all evals with relevance criteria",
			evals: []InputSession{
				{Session: &session.Session{Evals: &session.EvalCriteria{Relevance: []string{"a", "b"}}}},
				{Session: &session.Session{Evals: &session.EvalCriteria{Relevance: []string{"c"}}}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := needsJudge(tt.evals)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSanitizeEventText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		maxRunes int
		want     string
	}{
		{"plain text unchanged", "used $0.03 of $0.03", 256, "used $0.03 of $0.03"},
		{"surrounding whitespace trimmed", "  budget.max_cost \n", 256, "budget.max_cost"},
		{"control characters removed", "bud\x00get\x1b[31m", 256, "budget[31m"},
		{"newline and tab kept", "line one\n\tline two", 256, "line one\n\tline two"},
		{"carriage return removed", "line one\r\nline two", 256, "line one\nline two"},
		{"invalid utf-8 dropped", "bud\xffget", 256, "budget"},
		{"rune bound applied", strings.Repeat("é", 10), 4, "éééé"},
		{"trailing space after cut trimmed", "abc def", 4, "abc"},
		{"empty stays empty", "", 256, ""},
		{"whitespace only becomes empty", " \t\n ", 256, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeEventText(tt.input, tt.maxRunes)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, got, sanitizeEventText(got, tt.maxRunes), "sanitization must be idempotent")
			assert.LessOrEqual(t, utf8.RuneCountInString(got), tt.maxRunes)
		})
	}
}

func TestTerminationFromEvent(t *testing.T) {
	t.Parallel()

	t.Run("native fields copied under the fixed reason", func(t *testing.T) {
		t.Parallel()
		term := terminationFromEvent(budgetExceededTestEvent())
		assert.Equal(t, session.TerminationReasonBudgetExceeded, term.Reason)
		assert.Equal(t, "run", term.Budget)
		assert.Equal(t, "max_cost", term.Limit)
		assert.Equal(t, "$0.03", term.Used)
		assert.Equal(t, "$0.03", term.Max)
		assert.Equal(t, "budget.max_cost", term.ConfigPath)
		assert.Equal(t, budgetStopText, term.Message)
	})

	t.Run("missing and wrong-typed fields omitted", func(t *testing.T) {
		t.Parallel()
		term := terminationFromEvent(map[string]any{
			"type":    "budget_exceeded",
			"budget":  42,             // not a string
			"limit":   true,           // not a string
			"used":    []any{"$0.03"}, // not a string
			"message": " \x00 ",       // empty after sanitization
			// max and config_path absent
		})
		assert.Equal(t, session.TerminationReasonBudgetExceeded, term.Reason)
		assert.Empty(t, term.Budget)
		assert.Empty(t, term.Limit)
		assert.Empty(t, term.Used)
		assert.Empty(t, term.Max)
		assert.Empty(t, term.ConfigPath)
		assert.Empty(t, term.Message)
	})

	t.Run("only allow-listed keys are serialized", func(t *testing.T) {
		t.Parallel()
		event := budgetExceededTestEvent()
		event["prompt"] = "secret instructions"
		event["tool"] = "shell"

		data, err := json.Marshal(terminationFromEvent(event))
		require.NoError(t, err)

		var keys map[string]any
		require.NoError(t, json.Unmarshal(data, &keys))
		allowed := []string{"reason", "budget", "limit", "used", "max", "config_path", "message"}
		for key := range keys {
			assert.Contains(t, allowed, key)
		}
		assert.NotContains(t, string(data), "sess-1", "session_id must never be copied")
		assert.NotContains(t, string(data), "secret instructions")
		assert.NotContains(t, string(data), "shell")
	})

	t.Run("oversized fields bounded deterministically", func(t *testing.T) {
		t.Parallel()
		event := map[string]any{
			"type":    "budget_exceeded",
			"budget":  strings.Repeat("b", 10_000),
			"message": strings.Repeat("m", 10_000),
		}
		first := terminationFromEvent(event)
		second := terminationFromEvent(event)
		assert.Equal(t, first, second)
		assert.Equal(t, maxTerminationFieldRunes, utf8.RuneCountInString(first.Budget))
		assert.Equal(t, maxTerminationMessageRunes, utf8.RuneCountInString(first.Message))
	})
}

func TestBuildTranscriptBudgetTermination(t *testing.T) {
	t.Parallel()

	t.Run("marker then one assistant stop in chronological order", func(t *testing.T) {
		t.Parallel()
		transcript := buildTranscript([]map[string]any{
			{"type": "agent_choice", "content": "Working on it.", "agent_name": "root"},
			{
				"type":       "tool_call",
				"agent_name": "root",
				"tool_call": map[string]any{
					"function": map[string]any{"name": "shell", "arguments": `{"cmd": "ls"}`},
				},
			},
			budgetExceededTestEvent(),
			messageAddedTestEvent("sess-1", "root", budgetStopText),
			{"type": "stream_stopped"},
		})

		marker := "[Run terminated: budget_exceeded at budget.max_cost (used $0.03 of $0.03)]"
		assert.Equal(t, 1, strings.Count(transcript, marker))
		assert.Equal(t, 1, strings.Count(transcript, budgetStopText))

		wantOrder := []string{"Working on it.", `calls tool "shell"`, marker, "[Agent root says]:\n" + budgetStopText}
		lastIdx := -1
		for _, substr := range wantOrder {
			idx := strings.Index(transcript, substr)
			assert.Greater(t, idx, lastIdx, "expected %q to appear after previous substring", substr)
			lastIdx = idx
		}
	})

	t.Run("repeated events do not duplicate marker or stop", func(t *testing.T) {
		t.Parallel()
		transcript := buildTranscript([]map[string]any{
			budgetExceededTestEvent(),
			budgetExceededTestEvent(),
			{"type": "stream_stopped"},
		})

		assert.Equal(t, 1, strings.Count(transcript, "[Run terminated:"))
		assert.Equal(t, 1, strings.Count(transcript, budgetStopText))
	})

	t.Run("malformed or missing stop_message keeps the marker alone", func(t *testing.T) {
		t.Parallel()
		wrongRole := budgetExceededTestEventWithoutStop()
		payload := stopMessagePayload("root", "not the stop")
		payload["message"].(map[string]any)["role"] = "user"
		wrongRole["stop_message"] = payload

		transcript := buildTranscript([]map[string]any{
			wrongRole,
			{"type": "stream_stopped"},
		})

		assert.Equal(t, 1, strings.Count(transcript, "[Run terminated:"))
		assert.NotContains(t, transcript, "not the stop")
		assert.NotContains(t, transcript, budgetStopText)
	})

	t.Run("ordinary streams get no marker and message_added is ignored", func(t *testing.T) {
		t.Parallel()
		transcript := buildTranscript([]map[string]any{
			{"type": "agent_choice", "content": "All done.", "agent_name": "root"},
			messageAddedTestEvent("sess-1", "helper", "an unrelated runtime message"),
			{"type": "error", "error": "boom"},
			{"type": "stream_stopped"},
		})

		assert.NotContains(t, transcript, "[Run terminated:")
		assert.NotContains(t, transcript, "an unrelated runtime message")
		assert.Contains(t, transcript, "All done.")
	})

	t.Run("foreign message_added and stream_stopped cannot disturb the stop", func(t *testing.T) {
		t.Parallel()
		transcript := buildTranscript([]map[string]any{
			messageAddedTestEvent("other-session", "intruder", "fake stop"),
			{"type": "stream_stopped", "session_id": "other-session"},
			budgetExceededTestEvent(),
			messageAddedTestEvent("other-session", "intruder", "another fake stop"),
			{"type": "stream_stopped"},
		})

		assert.Equal(t, 1, strings.Count(transcript, "[Run terminated:"))
		assert.Equal(t, 1, strings.Count(transcript, budgetStopText))
		assert.NotContains(t, transcript, "fake stop")
		assert.NotContains(t, transcript, "intruder")
	})
}

func TestComputeRepeatMetrics_NilForSingleRun(t *testing.T) {
	t.Parallel()
	assert.Nil(t, computeRepeatMetrics(nil, 1))
	assert.Nil(t, computeRepeatMetrics(nil, 0))
	assert.Nil(t, computeRepeatMetrics([]Result{}, 3))
}

func TestComputeRepeatMetrics_AllPassEveryTime(t *testing.T) {
	t.Parallel()
	results := []Result{
		{InputPath: "a.json", SizeExpected: "M", Size: "M"},
		{InputPath: "a.json", SizeExpected: "M", Size: "M"},
		{InputPath: "b.json", SizeExpected: "S", Size: "S"},
		{InputPath: "b.json", SizeExpected: "S", Size: "S"},
	}
	m := computeRepeatMetrics(results, 2)
	require.NotNil(t, m)
	assert.Equal(t, 2, m.K)
	assert.Equal(t, 2, m.Total)
	assert.InDelta(t, 1.0, m.PassK, 1e-9)
	assert.InDelta(t, 1.0, m.HatK, 1e-9)
}

func TestComputeRepeatMetrics_FlakyEval(t *testing.T) {
	t.Parallel()
	results := []Result{
		{InputPath: "a.json", SizeExpected: "M", Size: "M"}, // pass
		{InputPath: "a.json", SizeExpected: "M", Size: "S"}, // fail
	}
	m := computeRepeatMetrics(results, 2)
	require.NotNil(t, m)
	assert.InDelta(t, 1.0, m.PassK, 1e-9, "pass@k: passed at least once")
	assert.InDelta(t, 0.0, m.HatK, 1e-9, "pass^k: did not pass every time")
}

func TestComputeRepeatMetrics_NeverPasses(t *testing.T) {
	t.Parallel()
	results := []Result{
		{InputPath: "a.json", SizeExpected: "M", Size: "S"},
		{InputPath: "a.json", SizeExpected: "M", Size: "S"},
	}
	m := computeRepeatMetrics(results, 2)
	require.NotNil(t, m)
	assert.InDelta(t, 0.0, m.PassK, 1e-9)
	assert.InDelta(t, 0.0, m.HatK, 1e-9)
}

func TestComputeRepeatMetrics_MixedEvals(t *testing.T) {
	t.Parallel()
	// 3 unique evals repeated 2 times:
	// a: pass, pass  → anyPass=true, allPass=true
	// b: pass, fail  → anyPass=true, allPass=false
	// c: fail, fail  → anyPass=false, allPass=false
	results := []Result{
		{InputPath: "a.json"},
		{InputPath: "a.json"},
		{InputPath: "b.json", SizeExpected: "M", Size: "M"},
		{InputPath: "b.json", SizeExpected: "M", Size: "S"},
		{InputPath: "c.json", Error: "boom"},
		{InputPath: "c.json", Error: "boom"},
	}
	m := computeRepeatMetrics(results, 2)
	require.NotNil(t, m)
	assert.Equal(t, 3, m.Total)
	assert.InDelta(t, 2.0/3.0, m.PassK, 1e-9, "2 of 3 evals passed at least once")
	assert.InDelta(t, 1.0/3.0, m.HatK, 1e-9, "1 of 3 evals passed every time")
}

func TestPrintSummary_WithRepeatMetrics(t *testing.T) {
	t.Parallel()
	summary := Summary{
		TotalEvals: 6,
		RepeatMetrics: &RepeatMetrics{
			K:     3,
			PassK: 1.0,
			HatK:  0.5,
			Total: 2,
		},
	}
	var buf bytes.Buffer
	printSummary(&buf, summary, time.Minute)
	output := buf.String()
	assert.Contains(t, output, "pass@3")
	assert.Contains(t, output, "pass^3")
	assert.Contains(t, output, "100.0%")
	assert.Contains(t, output, "50.0%")
}

func TestPrintSummary_NoRepeatMetricsWhenNil(t *testing.T) {
	t.Parallel()
	summary := Summary{TotalEvals: 2}
	var buf bytes.Buffer
	printSummary(&buf, summary, time.Minute)
	output := buf.String()
	assert.NotContains(t, output, "pass@")
	assert.NotContains(t, output, "Repeat")
}

func TestResultCheckResults_AssertionsAllPass(t *testing.T) {
	t.Parallel()
	r := Result{
		AssertionsTotal:  2,
		AssertionsPassed: 2,
		AssertionResults: []AssertionResult{
			{Name: "a", Passed: true},
			{Name: "b", Passed: true},
		},
	}
	successes, failures := r.checkResults()
	assert.Contains(t, successes, "assertions 2/2")
	assert.Empty(t, failures)
}

func TestResultCheckResults_AssertionsPartialFail(t *testing.T) {
	t.Parallel()
	r := Result{
		AssertionsTotal:  2,
		AssertionsPassed: 1,
		AssertionResults: []AssertionResult{
			{Name: "has greeting", Type: "contains", Passed: true},
			{Name: "no error", Type: "not_contains", Passed: false, Reason: `response contains "error"`},
		},
	}
	successes, failures := r.checkResults()
	assert.Empty(t, successes)
	assert.Len(t, failures, 1)
	assert.Contains(t, failures[0], "no error")
	assert.Contains(t, failures[0], `response contains "error"`)
}

func TestResultCheckResults_AssertionsDoNotFireWhenZero(t *testing.T) {
	t.Parallel()
	r := Result{AssertionsTotal: 0}
	successes, failures := r.checkResults()
	assert.Empty(t, successes)
	assert.Empty(t, failures)
}

func TestComputeSummary_Assertions(t *testing.T) {
	t.Parallel()

	results := []Result{
		{Title: "a", AssertionsTotal: 3, AssertionsPassed: 2},
		{Title: "b", AssertionsTotal: 2, AssertionsPassed: 2},
		{Title: "c", Error: "boom", AssertionsTotal: 1},
	}
	s := computeSummary(results)
	assert.Equal(t, 5, s.AssertionsTotal)
	assert.Equal(t, 4, s.AssertionsPassed)
}
