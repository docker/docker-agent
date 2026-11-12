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
		{None, "none", true},
		{Minimal, "minimal", true},
		{Low, "low", true},
		{Medium, "medium", true},
		{High, "high", true},
		{XHigh, "xhigh", true},
		{Max, "max", true},
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

func TestSupportedLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		reasoning bool
		levelMap  LevelMap
		want      []Level
	}{
		{
			name:      "non-reasoning model only supports none",
			reasoning: false,
			levelMap:  LevelMap{XHigh: true},
			want:      []Level{None},
		},
		{
			name:      "nil map yields defaults without top tiers",
			reasoning: true,
			levelMap:  nil,
			want:      []Level{None, Minimal, Low, Medium, High},
		},
		{
			name:      "false entry excludes level",
			reasoning: true,
			levelMap:  LevelMap{Minimal: false},
			want:      []Level{None, Low, Medium, High},
		},
		{
			name:      "xhigh requires explicit support",
			reasoning: true,
			levelMap:  LevelMap{XHigh: true},
			want:      []Level{None, Minimal, Low, Medium, High, XHigh},
		},
		{
			name:      "max requires explicit support",
			reasoning: true,
			levelMap:  LevelMap{Max: true},
			want:      []Level{None, Minimal, Low, Medium, High, Max},
		},
		{
			name:      "explicit false top tier stays excluded",
			reasoning: true,
			levelMap:  LevelMap{XHigh: false, Max: false},
			want:      []Level{None, Minimal, Low, Medium, High},
		},
		{
			name:      "anthropic-style map without minimal",
			reasoning: true,
			levelMap:  LevelMap{Minimal: false, XHigh: true, Max: true},
			want:      []Level{None, Low, Medium, High, XHigh, Max},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, SupportedLevels(tt.reasoning, tt.levelMap))
		})
	}
}

func TestClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		supported []Level
		requested Level
		want      Level
	}{
		{"supported level returned unchanged", []Level{None, Low, Medium, High}, Medium, Medium},
		{"clamps upward first", []Level{None, Low, High}, Medium, High},
		{"clamps downward when nothing above", []Level{None, Low, Medium, High}, Max, High},
		{"minimal clamps up to low", []Level{None, Low, Medium}, Minimal, Low},
		{"unknown level falls back to first", []Level{None, Low}, Level("bogus"), None},
		{"empty supported falls back to none", nil, High, None},
		{"none clamps up when unsupported", []Level{Low, Medium}, None, Low},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, Clamp(tt.supported, tt.requested))
		})
	}
}

func TestNextSupportedLevel(t *testing.T) {
	t.Parallel()

	supported := []Level{None, Low, Medium, High}

	tests := []struct {
		name    string
		levels  []Level
		current Level
		want    Level
	}{
		{"advances to next", supported, Low, Medium},
		{"wraps to first", supported, High, None},
		{"unknown current resets to first", supported, XHigh, None},
		{"empty supported returns none", nil, High, None},
		{"nonnil empty supported returns none", []Level{}, High, None},
		{"single level cycles onto itself", []Level{None}, None, None},
		{"duplicates advance from first match", []Level{Low, Medium, Low, High}, Low, Medium},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, NextSupportedLevel(tt.levels, tt.current))
		})
	}
}

// TestNextSupportedLevel_FullCycle advances through a capability-derived
// level list end-to-end and asserts it returns to the starting level.
func TestNextSupportedLevel_FullCycle(t *testing.T) {
	t.Parallel()

	supported := SupportedLevels(true, LevelMap{Minimal: false, XHigh: true, Max: true})
	cur := supported[0]
	for range supported {
		cur = NextSupportedLevel(supported, cur)
	}
	assert.Equal(t, supported[0], cur, "advancing len(supported) times returns to start")
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
