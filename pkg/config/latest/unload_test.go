package latest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModelConfigUnloadAPI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *ModelConfig
		want string
	}{
		{name: "no provider opts", cfg: &ModelConfig{}, want: ""},
		{name: "key absent", cfg: &ModelConfig{ProviderOpts: map[string]any{"other": "/foo"}}},
		{name: "valid path", cfg: &ModelConfig{ProviderOpts: map[string]any{"unload_api": "/api/unload"}}, want: "/api/unload"},
		{name: "non-string ignored", cfg: &ModelConfig{ProviderOpts: map[string]any{"unload_api": 42}}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.cfg.UnloadAPI())
		})
	}
}
