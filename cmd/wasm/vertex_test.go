//go:build js && wasm

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestWantsVertexAI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		opts map[string]any
		want bool
	}{
		{name: "unset"},
		{name: "empty", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": ""}},
		{name: "false", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}},
		{name: "uppercase false", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "FALSE"}},
		{name: "zero", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "0"}},
		{name: "invalid", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "invalid"}},
		{name: "true", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}, want: true},
		{name: "uppercase true", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "TRUE"}, want: true},
		{name: "one", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1"}, want: true},
		{name: "explicit project", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}, opts: map[string]any{"project": "test-project"}, want: true},
		{name: "explicit location", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}, opts: map[string]any{"location": "us-central1"}, want: true},
		{name: "model garden publisher", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}, opts: map[string]any{"publisher": "anthropic"}, want: true},
		{name: "google publisher", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}, opts: map[string]any{"publisher": "google"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{ProviderOpts: tt.opts}
			assert.Equal(t, tt.want, wantsVertexAI(t.Context(), cfg, environment.NewMapEnvProvider(tt.env)))
		})
	}
}
