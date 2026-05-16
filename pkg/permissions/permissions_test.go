package permissions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestNewChecker(t *testing.T) {
	t.Parallel()

	t.Run("nil config", func(t *testing.T) {
		t.Parallel()
		checker := NewChecker(nil)
		require.NotNil(t, checker)
		assert.True(t, checker.IsEmpty())
	})

	t.Run("empty config", func(t *testing.T) {
		t.Parallel()
		checker := NewChecker(&latest.PermissionsConfig{})
		require.NotNil(t, checker)
		assert.True(t, checker.IsEmpty())
	})

	t.Run("with patterns", func(t *testing.T) {
		t.Parallel()
		checker := NewChecker(&latest.PermissionsConfig{
			Allow: []string{"read_*"},
			Deny:  []string{"shell"},
		})
		require.NotNil(t, checker)
		assert.False(t, checker.IsEmpty())
	})

	t.Run("with only ask patterns", func(t *testing.T) {
		t.Parallel()
		checker := NewChecker(&latest.PermissionsConfig{
			Ask: []string{"fetch"},
		})
		require.NotNil(t, checker)
		assert.False(t, checker.IsEmpty())
	})
}

func TestChecker_Check(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		allow    []string
		ask      []string
		deny     []string
		toolName string
		want     Decision
	}{
		{
			name:     "no patterns returns Ask",
			toolName: "shell",
			want:     Ask,
		},
		{
			name:     "exact allow match",
			allow:    []string{"shell"},
			toolName: "shell",
			want:     Allow,
		},
		{
			name:     "exact deny match",
			deny:     []string{"shell"},
			toolName: "shell",
			want:     Deny,
		},
		{
			name:     "deny takes priority over allow",
			allow:    []string{"shell"},
			deny:     []string{"shell"},
			toolName: "shell",
			want:     Deny,
		},
		{
			name:     "glob pattern allow",
			allow:    []string{"read_*"},
			toolName: "read_file",
			want:     Allow,
		},
		{
			name:     "glob pattern deny",
			deny:     []string{"*_file"},
			toolName: "write_file",
			want:     Deny,
		},
		{
			name:     "no match returns Ask",
			allow:    []string{"read_*"},
			deny:     []string{"shell"},
			toolName: "write_file",
			want:     Ask,
		},
		{
			name:     "case insensitive matching",
			allow:    []string{"Shell"},
			toolName: "shell",
			want:     Allow,
		},
		{
			name:     "case insensitive pattern",
			allow:    []string{"READ_*"},
			toolName: "read_file",
			want:     Allow,
		},
		{
			name:     "question mark wildcard",
			allow:    []string{"read_???e"},
			toolName: "read_file",
			want:     Allow,
		},
		{
			name:     "character class",
			allow:    []string{"[rw]*_file"},
			toolName: "read_file",
			want:     Allow,
		},
		{
			name:     "mcp tool pattern",
			allow:    []string{"mcp:github:*"},
			toolName: "mcp:github:create_issue",
			want:     Allow,
		},
		{
			name:     "mcp tool exact match",
			deny:     []string{"mcp:github:delete_repo"},
			toolName: "mcp:github:delete_repo",
			want:     Deny,
		},
		{
			name:     "wildcard all",
			allow:    []string{"*"},
			toolName: "anything",
			want:     Allow,
		},
		{
			name:     "multiple patterns first match wins",
			allow:    []string{"shell", "read_*"},
			toolName: "read_file",
			want:     Allow,
		},
		// Ask patterns
		{
			name:     "ask pattern returns ForceAsk",
			ask:      []string{"fetch"},
			toolName: "fetch",
			want:     ForceAsk,
		},
		{
			name:     "deny takes priority over ask",
			ask:      []string{"fetch"},
			deny:     []string{"fetch"},
			toolName: "fetch",
			want:     Deny,
		},
		{
			name:     "allow takes priority over ask",
			allow:    []string{"fetch"},
			ask:      []string{"fetch"},
			toolName: "fetch",
			want:     Allow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checker := NewChecker(&latest.PermissionsConfig{
				Allow: tt.allow,
				Ask:   tt.ask,
				Deny:  tt.deny,
			})
			got := checker.Check(tt.toolName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestChecker_CheckWithArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		allow    []string
		deny     []string
		toolName string
		args     map[string]any
		want     Decision
	}{
		// Basic tool name matching (no args in pattern)
		{
			name:     "tool name only match with args provided",
			allow:    []string{"shell"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Allow,
		},
		{
			name:     "tool name only match with nil args",
			allow:    []string{"shell"},
			toolName: "shell",
			args:     nil,
			want:     Allow,
		},

		// Argument matching - Allow patterns
		{
			name:     "allow shell with specific cmd pattern",
			allow:    []string{"shell:cmd=ls*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Allow,
		},
		{
			name:     "allow shell with exact cmd match",
			allow:    []string{"shell:cmd=ls"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls"},
			want:     Allow,
		},
		{
			name:     "allow shell cmd pattern no match",
			allow:    []string{"shell:cmd=ls*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "rm -rf /"},
			want:     Ask,
		},
		{
			name:     "allow pattern with args but no args provided",
			allow:    []string{"shell:cmd=ls*"},
			toolName: "shell",
			args:     nil,
			want:     Ask,
		},

		// Argument matching - Deny patterns
		{
			name:     "deny shell with rm command",
			deny:     []string{"shell:cmd=rm*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "rm -rf /"},
			want:     Deny,
		},
		{
			name:     "deny shell with sudo",
			deny:     []string{"shell:cmd=sudo*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "sudo rm -rf /"},
			want:     Deny,
		},
		{
			name:     "deny pattern not matching",
			deny:     []string{"shell:cmd=rm*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Ask,
		},

		// Multiple argument conditions
		{
			name:     "multiple args all match",
			allow:    []string{"shell:cmd=ls*:cwd=."},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la", "cwd": "."},
			want:     Allow,
		},
		{
			name:     "multiple args partial match fails",
			allow:    []string{"shell:cmd=ls*:cwd=/home/*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la", "cwd": "/tmp"},
			want:     Ask,
		},
		{
			name:     "multiple args missing one fails",
			allow:    []string{"shell:cmd=ls*:cwd=."},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Ask,
		},

		// Priority: Deny > Allow
		{
			name:     "deny takes priority over allow with args",
			allow:    []string{"shell:cmd=*"},
			deny:     []string{"shell:cmd=rm*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "rm -rf /"},
			want:     Deny,
		},
		{
			name:     "allow wins when deny pattern does not match args",
			allow:    []string{"shell:cmd=ls*"},
			deny:     []string{"shell:cmd=rm*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Allow,
		},

		// Case insensitivity
		{
			name:     "case insensitive arg value matching",
			allow:    []string{"shell:cmd=LS*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Allow,
		},
		{
			name:     "case insensitive arg name matching",
			allow:    []string{"shell:CMD=ls*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls -la"},
			want:     Ask, // arg names are case-sensitive in Go maps
		},

		// Different argument types
		{
			name:     "boolean arg true",
			allow:    []string{"tool:flag=true"},
			toolName: "tool",
			args:     map[string]any{"flag": true},
			want:     Allow,
		},
		{
			name:     "boolean arg false",
			allow:    []string{"tool:flag=false"},
			toolName: "tool",
			args:     map[string]any{"flag": false},
			want:     Allow,
		},
		{
			name:     "numeric arg",
			allow:    []string{"tool:count=42"},
			toolName: "tool",
			args:     map[string]any{"count": float64(42)}, // JSON numbers are float64
			want:     Allow,
		},

		// Wildcard patterns in args
		{
			name:     "wildcard in arg value",
			allow:    []string{"shell:cmd=*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "anything"},
			want:     Allow,
		},
		{
			name:     "question mark wildcard in arg",
			allow:    []string{"shell:cmd=l?"},
			toolName: "shell",
			args:     map[string]any{"cmd": "ls"},
			want:     Allow,
		},

		// Complex real-world examples
		{
			name:     "allow safe git commands",
			allow:    []string{"shell:cmd=git status*", "shell:cmd=git log*", "shell:cmd=git diff*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "git status"},
			want:     Allow,
		},
		{
			name:     "deny dangerous git commands",
			deny:     []string{"shell:cmd=git push*", "shell:cmd=git reset --hard*"},
			toolName: "shell",
			args:     map[string]any{"cmd": "git push origin main"},
			want:     Deny,
		},
		{
			name:     "allow read operations on specific paths",
			allow:    []string{"read_file:path=/home/user/safe/*"},
			toolName: "read_file",
			args:     map[string]any{"path": "/home/user/safe/config.txt"},
			want:     Allow,
		},
		{
			name:     "deny write to sensitive paths",
			deny:     []string{"write_file:path=/etc/*", "write_file:path=/root/*"},
			toolName: "write_file",
			args:     map[string]any{"path": "/etc/passwd"},
			want:     Deny,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checker := NewChecker(&latest.PermissionsConfig{
				Allow: tt.allow,
				Deny:  tt.deny,
			})
			got := checker.CheckWithArgs(tt.toolName, tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecision_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		decision Decision
		want     string
	}{
		{Ask, "ask"},
		{Allow, "allow"},
		{Deny, "deny"},
		{ForceAsk, "force_ask"},
		{Decision(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.decision.String())
		})
	}
}

func TestChecker_ForceAsk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		allow    []string
		ask      []string
		deny     []string
		toolName string
		want     Decision
	}{
		{name: "no patterns returns Ask", toolName: "fetch", want: Ask},
		{name: "ask pattern returns ForceAsk", ask: []string{"fetch"}, toolName: "fetch", want: ForceAsk},
		{name: "ask glob returns ForceAsk", ask: []string{"fetch*"}, toolName: "fetch_url", want: ForceAsk},
		{name: "ask pattern does not match other tool", ask: []string{"fetch"}, toolName: "shell", want: Ask},
		{name: "deny takes priority over ask", ask: []string{"fetch"}, deny: []string{"fetch"}, toolName: "fetch", want: Deny},
		{name: "allow takes priority over ask", ask: []string{"fetch"}, allow: []string{"fetch"}, toolName: "fetch", want: Allow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checker := NewChecker(&latest.PermissionsConfig{
				Allow: tt.allow,
				Ask:   tt.ask,
				Deny:  tt.deny,
			})
			assert.Equal(t, tt.want, checker.Check(tt.toolName))
		})
	}
}

func TestParsePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern        string
		wantTool       string
		wantArgPattern map[string]string
	}{
		{
			pattern:        "shell",
			wantTool:       "shell",
			wantArgPattern: map[string]string{},
		},
		{
			pattern:        "shell:cmd=ls*",
			wantTool:       "shell",
			wantArgPattern: map[string]string{"cmd": "ls*"},
		},
		{
			pattern:        "shell:cmd=ls*:cwd=/home/*",
			wantTool:       "shell",
			wantArgPattern: map[string]string{"cmd": "ls*", "cwd": "/home/*"},
		},
		{
			// MCP tools have colons in their names - should preserve them
			pattern:        "mcp:github:create_issue",
			wantTool:       "mcp:github:create_issue",
			wantArgPattern: map[string]string{},
		},
		{
			// MCP tool with argument pattern
			pattern:        "mcp:github:create_issue:repo=owner/*",
			wantTool:       "mcp:github:create_issue",
			wantArgPattern: map[string]string{"repo": "owner/*"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			t.Parallel()
			toolPattern, argPatterns := parsePattern(tt.pattern)
			assert.Equal(t, tt.wantTool, toolPattern)
			assert.Equal(t, tt.wantArgPattern, argPatterns)
		})
	}
}

func TestMatchGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		// Exact matches
		{"shell", "shell", true},
		{"shell", "bash", false},

		// Glob patterns
		{"read_*", "read_file", true},
		{"read_*", "read_directory", true},
		{"read_*", "write_file", false},
		{"*_file", "read_file", true},
		{"*_file", "write_file", true},
		{"*_file", "read_directory", false},

		// Wildcards
		{"*", "anything", true},
		{"???", "abc", true},
		{"???", "abcd", false},

		// Character classes
		{"[a-z]*", "shell", true},
		{"[0-9]*", "shell", false},

		// Case insensitivity
		{"SHELL", "shell", true},
		{"shell", "SHELL", true},
		{"Read_*", "read_file", true},

		// Qualified names (MCP tools)
		{"mcp:*", "mcp:github:create_issue", true}, // * matches everything including :
		{"mcp:github:*", "mcp:github:create_issue", true},
		{"mcp:github:create_*", "mcp:github:create_issue", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.value, func(t *testing.T) {
			t.Parallel()
			got := matchGlob(tt.pattern, tt.value)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestArgToString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input any
		want  string
	}{
		{"string", "hello", "hello"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"float64 integer", float64(42), "42"},
		{"float64 decimal", float64(3.14), "3.14"},
		{"int", 42, "42"},
		{"int64", int64(42), "42"},
		{"nil", nil, "<nil>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := argToString(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()

	t.Run("both nil", func(t *testing.T) {
		t.Parallel()
		merged := Merge(nil, nil)
		assert.True(t, merged.IsEmpty())
	})

	t.Run("one nil", func(t *testing.T) {
		t.Parallel()
		c := NewChecker(&latest.PermissionsConfig{Allow: []string{"tool_a"}})
		merged := Merge(c, nil)
		assert.Equal(t, []string{"tool_a"}, merged.AllowPatterns())
	})

	t.Run("combines patterns", func(t *testing.T) {
		t.Parallel()
		team := NewChecker(&latest.PermissionsConfig{
			Allow: []string{"team_tool"},
			Deny:  []string{"team_deny"},
		})
		global := NewChecker(&latest.PermissionsConfig{
			Allow: []string{"global_tool"},
			Ask:   []string{"global_ask"},
		})
		merged := Merge(team, global)
		assert.Equal(t, []string{"team_tool", "global_tool"}, merged.AllowPatterns())
		assert.Equal(t, []string{"team_deny"}, merged.DenyPatterns())
		assert.Equal(t, []string{"global_ask"}, merged.AskPatterns())
	})

	t.Run("deny from either source blocks", func(t *testing.T) {
		t.Parallel()
		team := NewChecker(&latest.PermissionsConfig{Allow: []string{"tool_a"}})
		global := NewChecker(&latest.PermissionsConfig{Deny: []string{"tool_a"}})
		merged := Merge(team, global)
		// Deny is checked first, so global deny overrides team allow
		assert.Equal(t, Deny, merged.Check("tool_a"))
	})

	t.Run("skips empty checkers", func(t *testing.T) {
		t.Parallel()
		empty := NewChecker(&latest.PermissionsConfig{})
		actual := NewChecker(&latest.PermissionsConfig{Deny: []string{"bad"}})
		merged := Merge(empty, nil, actual, empty)
		assert.Equal(t, []string{"bad"}, merged.DenyPatterns())
	})
}
