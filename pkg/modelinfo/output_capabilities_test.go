package modelinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestResolveToolCallSupport(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"tool-model":    {ToolCall: true},
			"no-tool-model": {ToolCall: false},
		}},
	}})

	tests := []struct {
		name  string
		store *modelsdev.Store
		model string
		want  ToolCallSupport
	}{
		{name: "catalogue true", store: store, model: "tool-model", want: ToolCallSupported},
		{name: "catalogue false", store: store, model: "no-tool-model", want: ToolCallUnsupported},
		{name: "missing model", store: store, model: "missing", want: ToolCallSupportUnknown},
		{name: "nil store", model: "tool-model", want: ToolCallSupportUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ResolveToolCallSupport(t.Context(), tt.store, modelsdev.NewID("google", tt.model))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveOutputImage(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"image-model": {Modalities: modelsdev.Modalities{Output: []string{"text", "IMAGE"}}},
			"text-model":  {Modalities: modelsdev.Modalities{Output: []string{"text"}}},
		}},
	}})

	tests := []struct {
		name     string
		store    *modelsdev.Store
		model    string
		override *bool
		want     bool
	}{
		{name: "explicit true overrides catalogue", store: store, model: "text-model", override: new(true), want: true},
		{name: "explicit false overrides catalogue", store: store, model: "image-model", override: new(false), want: false},
		{name: "catalogue image case insensitive", store: store, model: "image-model", want: true},
		{name: "catalogue text only", store: store, model: "text-model", want: false},
		{name: "missing model", store: store, model: "missing", want: false},
		{name: "nil store", model: "image-model", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ResolveOutputImage(t.Context(), tt.store, modelsdev.NewID("google", tt.model), tt.override)
			assert.Equal(t, tt.want, got)
		})
	}
}
