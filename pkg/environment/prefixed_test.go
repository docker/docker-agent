package environment

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type recordingProvider struct {
	Provider

	looked []string
}

func (p *recordingProvider) Get(ctx context.Context, name string) (string, bool) {
	p.looked = append(p.looked, name)
	return p.Provider.Get(ctx, name)
}

func TestPrefixedProvider_Get(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stored     map[string]string
		lookup     string
		wantValue  string
		wantFound  bool
		wantLooked []string
	}{
		{
			name:       "plain name only",
			stored:     map[string]string{"API_KEY": "plain"},
			lookup:     "API_KEY",
			wantValue:  "plain",
			wantFound:  true,
			wantLooked: []string{"DOCKER_AGENT_ENV_API_KEY", "API_KEY"},
		},
		{
			name:       "override only",
			stored:     map[string]string{"DOCKER_AGENT_ENV_API_KEY": "override"},
			lookup:     "API_KEY",
			wantValue:  "override",
			wantFound:  true,
			wantLooked: []string{"DOCKER_AGENT_ENV_API_KEY"},
		},
		{
			name:       "override wins over plain name",
			stored:     map[string]string{"API_KEY": "plain", "DOCKER_AGENT_ENV_API_KEY": "override"},
			lookup:     "API_KEY",
			wantValue:  "override",
			wantFound:  true,
			wantLooked: []string{"DOCKER_AGENT_ENV_API_KEY"},
		},
		{
			name:       "empty override falls back to plain name",
			stored:     map[string]string{"API_KEY": "plain", "DOCKER_AGENT_ENV_API_KEY": ""},
			lookup:     "API_KEY",
			wantValue:  "plain",
			wantFound:  true,
			wantLooked: []string{"DOCKER_AGENT_ENV_API_KEY", "API_KEY"},
		},
		{
			name:       "docker-agent settings are never overridden",
			stored:     map[string]string{"DOCKER_AGENT_NO_TOKEN_EXCHANGE": "1"},
			lookup:     "DOCKER_AGENT_NO_TOKEN_EXCHANGE",
			wantValue:  "1",
			wantFound:  true,
			wantLooked: []string{"DOCKER_AGENT_NO_TOKEN_EXCHANGE"},
		},
		{
			name:       "settings namespace does not leak into plain names",
			stored:     map[string]string{"DOCKER_AGENT_REMOTE_AUTH_TOKEN": "internal"},
			lookup:     "REMOTE_AUTH_TOKEN",
			wantFound:  false,
			wantLooked: []string{"DOCKER_AGENT_ENV_REMOTE_AUTH_TOKEN", "REMOTE_AUTH_TOKEN"},
		},
		{
			name:       "neither name set",
			stored:     map[string]string{},
			lookup:     "API_KEY",
			wantFound:  false,
			wantLooked: []string{"DOCKER_AGENT_ENV_API_KEY", "API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inner := &recordingProvider{Provider: NewMapEnvProvider(tt.stored)}
			value, found := NewPrefixedProvider(inner).Get(t.Context(), tt.lookup)

			assert.Equal(t, tt.wantValue, value)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantLooked, inner.looked)
		})
	}
}

func TestPrefixedProvider_EmptyOverrideDoesNotHideLaterSources(t *testing.T) {
	t.Parallel()

	shell := NewMapEnvProvider(map[string]string{"DOCKER_AGENT_ENV_API_KEY": ""})
	envFile := NewMapEnvProvider(map[string]string{"DOCKER_AGENT_ENV_API_KEY": "override"})

	value, found := NewPrefixedProvider(shell, envFile).Get(t.Context(), "API_KEY")
	assert.True(t, found)
	assert.Equal(t, "override", value)
}
