package latest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateInjectMemories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     AgentConfig
		wantErr string
	}{
		{
			name: "disabled with no companion fields is valid",
			cfg:  AgentConfig{Name: "agent"},
		},
		{
			name: "disabled with strategy set is valid",
			cfg:  AgentConfig{Name: "agent", InjectMemoriesStrategy: InjectMemoriesStrategyLocal},
		},
		{
			name: "disabled with max set is valid",
			cfg:  AgentConfig{Name: "agent", MaxInjectMemories: 5},
		},
		{
			name: "enabled with empty strategy is valid (defaults to local)",
			cfg:  AgentConfig{Name: "agent", InjectMemories: true},
		},
		{
			name: "enabled with local strategy is valid",
			cfg:  AgentConfig{Name: "agent", InjectMemories: true, InjectMemoriesStrategy: InjectMemoriesStrategyLocal},
		},
		{
			name: "enabled with max_inject_memories zero is valid",
			cfg:  AgentConfig{Name: "agent", InjectMemories: true, MaxInjectMemories: 0},
		},
		{
			name: "enabled with positive max is valid",
			cfg:  AgentConfig{Name: "agent", InjectMemories: true, MaxInjectMemories: 20},
		},
		{
			name:    "enabled with invalid strategy is rejected",
			cfg:     AgentConfig{Name: "myagent", InjectMemories: true, InjectMemoriesStrategy: "bogus"},
			wantErr: `agent "myagent": inject_memories_strategy "bogus" is invalid`,
		},
		{
			name:    "enabled with negative max is rejected",
			cfg:     AgentConfig{Name: "myagent", InjectMemories: true, MaxInjectMemories: -1},
			wantErr: `agent "myagent": max_inject_memories must be >= 0 (got -1)`,
		},
		{
			name:    "enabled with llm strategy is rejected (not yet shipped)",
			cfg:     AgentConfig{Name: "myagent", InjectMemories: true, InjectMemoriesStrategy: "llm"},
			wantErr: `agent "myagent": inject_memories_strategy "llm" is invalid`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.validateInjectMemories()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
