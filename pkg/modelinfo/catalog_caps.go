package modelinfo

import (
	"context"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ModelLookup is the catalogue lookup needed to resolve input capabilities.
type ModelLookup interface {
	GetModel(ctx context.Context, id modelsdev.ID) (*modelsdev.Model, error)
}

// AliasedCatalogCaps resolves image input support for ChatGPT from the matching
// OpenAI entry after a direct catalogue miss. Other capabilities, limits, and
// pricing are not inherited.
func AliasedCatalogCaps(ctx context.Context, store ModelLookup, id modelsdev.ID) (ModelCapabilities, bool) {
	if store == nil || !id.IsValid() || id.Provider != "chatgpt" || ctx.Err() != nil {
		return ModelCapabilities{}, false
	}
	model, err := store.GetModel(ctx, modelsdev.NewID("openai", id.Model))
	if err != nil || model == nil || ctx.Err() != nil {
		return ModelCapabilities{}, false
	}
	caps := capsFromModalities(model.Modalities.Input)
	return CapsWith(caps.SupportsImage(), false, false, false), true
}
