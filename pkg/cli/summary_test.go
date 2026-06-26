package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
)

func renderSummary(stats session.Stats) string {
	var buf bytes.Buffer
	NewPrinter(&buf).PrintSessionSummary(stats)
	return buf.String()
}

func TestPrintSessionSummary_NoActivityIsNoOp(t *testing.T) {
	t.Parallel()

	assert.Empty(t, renderSummary(session.Stats{ID: "abc"}))
}

func TestPrintSessionSummary_FullRecap(t *testing.T) {
	t.Parallel()

	out := renderSummary(session.Stats{
		ID:           "abc-123",
		Duration:     90 * time.Second,
		Requests:     4,
		ToolCalls:    12,
		ToolErrors:   2,
		InputTokens:  1000,
		CachedInput:  146112,
		OutputTokens: 2059,
		Cost:         0.1234,
		Models: []session.ModelStats{
			{Model: "gemini-2.5-pro", Requests: 12, InputTokens: 22264, CachedInput: 106844, OutputTokens: 237},
		},
	})

	// Interaction summary.
	assert.Contains(t, out, "Session summary")
	assert.Contains(t, out, "abc-123")
	assert.Contains(t, out, "1m 30s")
	assert.Contains(t, out, "12 (10 ok, 2 failed)")
	assert.Contains(t, out, "83.3%")

	// Model usage table.
	assert.Contains(t, out, "Model usage")
	assert.Contains(t, out, "gemini-2.5-pro")
	assert.Contains(t, out, "22,264")
	assert.Contains(t, out, "106,844")

	// Totals and cache savings.
	assert.Contains(t, out, "147,112 in / 2,059 out")
	assert.Contains(t, out, "$0.12")
	assert.Contains(t, out, "146,112 input tokens from cache")
}

func TestPrintSessionSummary_NoToolCallsOmitsToolLines(t *testing.T) {
	t.Parallel()

	out := renderSummary(session.Stats{
		ID:           "no-tools",
		Requests:     1,
		InputTokens:  500,
		OutputTokens: 100,
		Cost:         0.002,
		Models:       []session.ModelStats{{Model: "m", Requests: 1, InputTokens: 500, OutputTokens: 100}},
	})

	assert.NotContains(t, out, "Tool calls")
	assert.NotContains(t, out, "Success rate")
	assert.Contains(t, out, "500 in / 100 out")
}

func TestGroupDigits(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0:       "0",
		5:       "5",
		999:     "999",
		1000:    "1,000",
		70637:   "70,637",
		1234567: "1,234,567",
		-1500:   "-1,500",
	}
	for in, want := range cases {
		assert.Equalf(t, want, groupDigits(in), "groupDigits(%d)", in)
	}
}

func TestFormatCost(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "$0.00", formatCost(0))
	assert.Equal(t, "$0.00", formatCost(0.00005))
	assert.Equal(t, "$0.0050", formatCost(0.005))
	assert.Equal(t, "$0.12", formatCost(0.1234))
	assert.Equal(t, "$3.00", formatCost(3))
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0s", formatDuration(0))
	assert.Equal(t, "45s", formatDuration(45*time.Second))
	assert.Equal(t, "1m", formatDuration(time.Minute))
	assert.Equal(t, "1m 30s", formatDuration(90*time.Second))
	assert.Equal(t, "1h", formatDuration(time.Hour))
	assert.Equal(t, "1h 1m", formatDuration(time.Hour+time.Minute))
}
