package anthropic

import "testing"

func TestRejectsSampling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{model: "claude-haiku-4-5", want: false},
		{model: "claude-haiku-5", want: true},
		{model: "claude-haiku-5-5", want: true},
		{model: "claude-haiku-5-5-20251001", want: true},
		{model: "vendor/claude-haiku-5", want: true},
		{model: "us.anthropic.claude-haiku-5-5", want: true},
		{model: "claude-sonnet-4-5", want: false},
		{model: "claude-sonnet-5", want: true},
		{model: "claude-opus-4-6", want: false},
		{model: "claude-opus-4-7", want: true},
		{model: "claude-fable-5", want: true},
		{model: "claude-mythos-5", want: true},
		{model: "not-a-claude-model", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			if got := rejectsSampling(tc.model); got != tc.want {
				t.Fatalf("rejectsSampling(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestClaude5Predicates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model                                                       string
		defaultThinking, requiresThinking, checksPrefix, noSampling bool
	}{
		{"claude-haiku-5-5", true, false, true, true},
		{"claude-opus-5", true, false, false, true},
		{"claude-opus-5-5", true, true, true, true},
		{"claude-sonnet-5", true, false, false, true},
		{"claude-sonnet-5-5", true, true, true, true},
		{"claude-fable-5", true, true, false, true},
		{"claude-fable-5-1", true, true, true, true},
		{"claude-mythos-5", true, true, false, true},
		{"claude-mythos-5-1", true, true, false, true},
		{"claude-opus-5-5@20261001", true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			if got := usesDefaultThinking(tc.model); got != tc.defaultThinking {
				t.Errorf("usesDefaultThinking = %v, want %v", got, tc.defaultThinking)
			}
			if got := requiresThinking(tc.model); got != tc.requiresThinking {
				t.Errorf("requiresThinking = %v, want %v", got, tc.requiresThinking)
			}
			if got := checksThinkingPrefix(tc.model); got != tc.checksPrefix {
				t.Errorf("checksThinkingPrefix = %v, want %v", got, tc.checksPrefix)
			}
			if got := rejectsSampling(tc.model); got != tc.noSampling {
				t.Errorf("rejectsSampling = %v, want %v", got, tc.noSampling)
			}
		})
	}

	// IDs outside the table keep the name heuristics.
	if !requiresThinking("claude-mythos-5-preview") {
		t.Error("requiresThinking(claude-mythos-5-preview) should keep the heuristic answer")
	}
}
