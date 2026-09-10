package evaluation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
)

func TestRunAssertions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		assertions []session.Assertion
		response   string
		cost       float64
		toolCalls  []string
		wantPass   []bool
	}{
		{
			name: "contains pass",
			assertions: []session.Assertion{
				{Name: "has hello", Type: "contains", Value: "hello"},
			},
			response: "hello world",
			wantPass: []bool{true},
		},
		{
			name: "contains fail",
			assertions: []session.Assertion{
				{Name: "has goodbye", Type: "contains", Value: "goodbye"},
			},
			response: "hello world",
			wantPass: []bool{false},
		},
		{
			name: "not_contains pass",
			assertions: []session.Assertion{
				{Name: "no error", Type: "not_contains", Value: "error"},
			},
			response: "all good",
			wantPass: []bool{true},
		},
		{
			name: "not_contains fail",
			assertions: []session.Assertion{
				{Name: "no error", Type: "not_contains", Value: "error"},
			},
			response: "there was an error",
			wantPass: []bool{false},
		},
		{
			name: "equals pass with whitespace trimming",
			assertions: []session.Assertion{
				{Name: "exact", Type: "equals", Value: "hello"},
			},
			response: "  hello  ",
			wantPass: []bool{true},
		},
		{
			name: "equals fail",
			assertions: []session.Assertion{
				{Name: "exact", Type: "equals", Value: "hello"},
			},
			response: "hello world",
			wantPass: []bool{false},
		},
		{
			name: "starts_with pass",
			assertions: []session.Assertion{
				{Name: "prefix", Type: "starts_with", Value: "hello"},
			},
			response: "hello world",
			wantPass: []bool{true},
		},
		{
			name: "starts_with fail",
			assertions: []session.Assertion{
				{Name: "prefix", Type: "starts_with", Value: "world"},
			},
			response: "hello world",
			wantPass: []bool{false},
		},
		{
			name: "ends_with pass",
			assertions: []session.Assertion{
				{Name: "suffix", Type: "ends_with", Value: "world"},
			},
			response: "hello world",
			wantPass: []bool{true},
		},
		{
			name: "ends_with fail",
			assertions: []session.Assertion{
				{Name: "suffix", Type: "ends_with", Value: "hello"},
			},
			response: "hello world",
			wantPass: []bool{false},
		},
		{
			name: "regex pass",
			assertions: []session.Assertion{
				{Name: "pattern", Type: "regex", Value: `\d+ files`},
			},
			response: "found 42 files",
			wantPass: []bool{true},
		},
		{
			name: "regex fail",
			assertions: []session.Assertion{
				{Name: "pattern", Type: "regex", Value: `\d+ files`},
			},
			response: "no files found",
			wantPass: []bool{false},
		},
		{
			name: "regex invalid pattern",
			assertions: []session.Assertion{
				{Name: "bad regex", Type: "regex", Value: `[invalid`},
			},
			response: "anything",
			wantPass: []bool{false},
		},
		{
			name: "cost_threshold pass",
			assertions: []session.Assertion{
				{Name: "cheap", Type: "cost_threshold", Value: "0.10"},
			},
			cost:     0.05,
			wantPass: []bool{true},
		},
		{
			name: "cost_threshold fail",
			assertions: []session.Assertion{
				{Name: "cheap", Type: "cost_threshold", Value: "0.01"},
			},
			cost:     0.05,
			wantPass: []bool{false},
		},
		{
			name: "cost_threshold invalid value",
			assertions: []session.Assertion{
				{Name: "bad cost", Type: "cost_threshold", Value: "not-a-number"},
			},
			cost:     0.05,
			wantPass: []bool{false},
		},
		{
			name: "tool_called pass",
			assertions: []session.Assertion{
				{Name: "used search", Type: "tool_called", Value: "search"},
			},
			toolCalls: []string{"read_file", "search", "write_file"},
			wantPass:  []bool{true},
		},
		{
			name: "tool_called fail",
			assertions: []session.Assertion{
				{Name: "used search", Type: "tool_called", Value: "search"},
			},
			toolCalls: []string{"read_file", "write_file"},
			wantPass:  []bool{false},
		},
		{
			name: "unknown type fails",
			assertions: []session.Assertion{
				{Name: "unknown", Type: "magic", Value: "xyz"},
			},
			wantPass: []bool{false},
		},
		{
			name: "multiple assertions mixed results",
			assertions: []session.Assertion{
				{Name: "has hello", Type: "contains", Value: "hello"},
				{Name: "no error", Type: "not_contains", Value: "error"},
				{Name: "used search", Type: "tool_called", Value: "search"},
			},
			response:  "hello world",
			toolCalls: []string{"read_file"},
			wantPass:  []bool{true, true, false},
		},
		{
			name:       "empty assertions",
			assertions: nil,
			wantPass:   []bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			results := runAssertions(tt.assertions, tt.response, tt.cost, tt.toolCalls)
			assert.Len(t, results, len(tt.wantPass))
			for i, want := range tt.wantPass {
				assert.Equal(t, want, results[i].Passed,
					"assertion %d (%s): expected pass=%v, got pass=%v, reason=%s",
					i, results[i].Name, want, results[i].Passed, results[i].Reason)
			}
		})
	}
}
