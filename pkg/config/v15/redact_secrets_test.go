package v15

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactSecretsEnabledDefault(t *testing.T) {
	t.Parallel()

	tru, fls := true, false

	tests := []struct {
		name string
		cfg  *AgentConfig
		want bool
	}{
		{name: "nil receiver defaults to on", cfg: nil, want: true},
		{name: "field omitted defaults to on", cfg: &AgentConfig{}, want: true},
		{name: "explicit true is on", cfg: &AgentConfig{RedactSecrets: &tru}, want: true},
		{name: "explicit false opts out", cfg: &AgentConfig{RedactSecrets: &fls}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.cfg.RedactSecretsEnabled())
		})
	}
}

func TestRedactSecretsYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantSet bool // whether the pointer should be non-nil
		wantVal bool // value to expect when set
		want    bool // expected effective value via RedactSecretsEnabled
	}{
		{name: "omitted", yaml: "model: openai/gpt-5", wantSet: false, want: true},
		{name: "explicit true", yaml: "model: openai/gpt-5\nredact_secrets: true", wantSet: true, wantVal: true, want: true},
		{name: "explicit false", yaml: "model: openai/gpt-5\nredact_secrets: false", wantSet: true, wantVal: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg AgentConfig
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &cfg))

			if tt.wantSet {
				require.NotNil(t, cfg.RedactSecrets, "field should be set")
				assert.Equal(t, tt.wantVal, *cfg.RedactSecrets)
			} else {
				assert.Nil(t, cfg.RedactSecrets, "field should be nil when omitted")
			}
			assert.Equal(t, tt.want, cfg.RedactSecretsEnabled())
		})
	}
}
