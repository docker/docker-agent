package modelinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

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
