package effort

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		input string
		want  Level
		ok    bool
	}{
		{"none", None, true},
		{"minimal", Minimal, true},
		{"low", Low, true},
		{"medium", Medium, true},
		{"high", High, true},
		{"xhigh", XHigh, true},
		{"max", Max, true},
		{"HIGH", High, true},
		{"  Medium  ", Medium, true},
		{"adaptive", "", false},
		{"adaptive/high", "", false},
		{"unknown", "", false},
		{"", "", false},
	} {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, ok := Parse(tt.input)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsValid(t *testing.T) {
	t.Parallel()

	valid := []string{
		"none", "minimal", "low", "medium", "high", "xhigh", "max",
		"adaptive", "adaptive/low", "adaptive/medium", "adaptive/high", "adaptive/xhigh", "adaptive/max",
		"ADAPTIVE/HIGH", "  adaptive  ",
	}
	for _, s := range valid {
		t.Run("valid_"+s, func(t *testing.T) {
			t.Parallel()
			assert.True(t, IsValid(s), "expected %q to be valid", s)
		})
	}

	invalid := []string{
		"", "unknown", "adaptive/none", "adaptive/minimal",
		"adaptive/", "adaptive/foo",
	}
	for _, s := range invalid {
		t.Run("invalid_"+s, func(t *testing.T) {
			t.Parallel()
			assert.False(t, IsValid(s), "expected %q to be invalid", s)
		})
	}
}

func TestForOpenAI(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		level Level
		want  string
		ok    bool
	}{
		{Minimal, "minimal", true},
		{Low, "low", true},
		{Medium, "medium", true},
		{High, "high", true},
		{XHigh, "xhigh", true},
		{Max, "", false},
		{None, "", false},
	} {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			got, ok := ForOpenAI(tt.level)
			require.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestForAnthropic(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		level Level
		want  string
		ok    bool
	}{
		{Minimal, "low", true}, // minimal maps to low
		{Low, "low", true},
		{Medium, "medium", true},
		{High, "high", true},
		{XHigh, "xhigh", true},
		{Max, "max", true},
		{None, "", false},
	} {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			got, ok := ForAnthropic(tt.level)
			require.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBedrockTokens(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		level Level
		want  int
		ok    bool
	}{
		{Minimal, 1024, true},
		{Low, 2048, true},
		{Medium, 8192, true},
		{High, 16384, true},
		{XHigh, 32768, true},
		{Max, 32768, true},
		{None, 0, false},
	} {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			got, ok := BedrockTokens(tt.level)
			require.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestForGemini3(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		level Level
		want  string
		ok    bool
	}{
		{Minimal, "minimal", true},
		{Low, "low", true},
		{Medium, "medium", true},
		{High, "high", true},
		{XHigh, "", false},
		{Max, "", false},
		{None, "", false},
	} {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			got, ok := ForGemini3(tt.level)
			require.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsValidAdaptive(t *testing.T) {
	t.Parallel()

	valid := []string{"low", "medium", "high", "xhigh", "max", "HIGH", "  Medium  "}
	for _, s := range valid {
		t.Run("valid_"+s, func(t *testing.T) {
			t.Parallel()
			assert.True(t, IsValidAdaptive(s), "expected %q to be valid", s)
		})
	}

	invalid := []string{"", "none", "minimal", "unknown", "adaptive", "adaptive/high"}
	for _, s := range invalid {
		t.Run("invalid_"+s, func(t *testing.T) {
			t.Parallel()
			assert.False(t, IsValidAdaptive(s), "expected %q to be invalid", s)
		})
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level Level
		want  string
	}{
		{None, "none"},
		{Minimal, "minimal"},
		{Low, "low"},
		{Medium, "medium"},
		{High, "high"},
		{XHigh, "xhigh"},
		{Max, "max"},
	}

	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.level.String())
		})
	}
}
