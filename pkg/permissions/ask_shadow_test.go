package permissions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestShadowedAskPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		allow []string
		ask   []string
		want  []Shadow
	}{
		{
			name:  "unconditional allow covers a literal ask",
			allow: []string{"*"},
			ask:   []string{"shell"},
			want:  []Shadow{{Ask: "shell", Allow: "*"}},
		},
		{
			name:  "identical allow and ask shadow",
			allow: []string{"shell"},
			ask:   []string{"shell"},
			want:  []Shadow{{Ask: "shell", Allow: "shell"}},
		},
		{
			name:  "tool glob allow shadows a literal ask",
			allow: []string{"read_*"},
			ask:   []string{"read_file"},
			want:  []Shadow{{Ask: "read_file", Allow: "read_*"}},
		},
		{
			name:  "unrelated allow does not shadow",
			allow: []string{"read_*"},
			ask:   []string{"shell"},
		},
		{
			name:  "argument-free allow covers an argument-scoped ask",
			allow: []string{"shell"},
			ask:   []string{"shell:cmd=ls*"},
			want:  []Shadow{{Ask: "shell:cmd=ls*", Allow: "shell"}},
		},
		{
			name:  "argument-scoped allows are not provable",
			allow: []string{"shell:cmd=ls*"},
			ask:   []string{"shell:cmd=ls*"},
		},
		{
			name:  "glob ask is not provable against a tool glob",
			allow: []string{"read_*"},
			ask:   []string{"read_*"},
		},
		{
			name:  "unconditional allow covers a glob ask",
			allow: []string{"*"},
			ask:   []string{"read_*"},
			want:  []Shadow{{Ask: "read_*", Allow: "*"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ShadowedAskPatterns(&latest.PermissionsConfig{Allow: tt.allow, Ask: tt.ask})
			if len(tt.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

// TestShadowedAskPatternsMatchesChecker pins the diagnostic against the
// checker's actual decision. For command tools, a literal trailing-star allow
// pattern only matches at a command-word boundary (commandAllowMatches), while
// the ask matcher is a plain glob: with "shell:cmd=ls*" in both lists, the
// checker still returns ForceAsk for lsof, so no shadow may be reported.
func TestShadowedAskPatternsMatchesChecker(t *testing.T) {
	t.Parallel()

	cfg := &latest.PermissionsConfig{
		Allow: []string{"shell:cmd=ls*"},
		Ask:   []string{"shell:cmd=ls*"},
	}
	checker := NewChecker(cfg)

	require.Equal(t, Allow, checker.CheckWithArgs("shell", map[string]any{"cmd": "ls -la"}))
	require.Equal(t, ForceAsk, checker.CheckWithArgs("shell", map[string]any{"cmd": "lsof"}))

	assert.Empty(t, ShadowedAskPatterns(cfg),
		"the ask still prompts for lsof, so no shadow may be reported")
}

func TestShadowedAskPatternsNilAndEmpty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, ShadowedAskPatterns(nil))
	assert.Empty(t, ShadowedAskPatterns(&latest.PermissionsConfig{}))
	assert.Empty(t, ShadowedAskPatterns(&latest.PermissionsConfig{Ask: []string{"shell"}}))
	assert.Empty(t, ShadowedAskPatterns(&latest.PermissionsConfig{Allow: []string{"*"}}))
}
