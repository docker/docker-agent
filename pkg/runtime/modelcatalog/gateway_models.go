package modelcatalog

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsgateway"
)

// gatewayModelsTTL is how long a gateway /v1/models response (or failure)
// is reused before the gateway is queried again. It keeps the model picker
// snappy on repeated opens while still picking up gateway changes
// eventually.
const gatewayModelsTTL = 5 * time.Minute

func (r *Catalog) listGatewayModels(ctx context.Context) ([]string, error) {
	start := time.Now()
	models, err := r.gatewayModels.get(ctx, r.clock(), gatewayModelsTTL, func(ctx context.Context) ([]string, error) {
		return modelsgateway.ListModels(ctx, r.Config.ModelsGateway, r.Config.EnvProvider)
	})
	slog.DebugContext(ctx, "Gateway model discovery completed", "duration", time.Since(start), "models", len(models), "error", err)
	return models, err
}

// buildGatewayChoices builds ModelChoice entries from the models served by
// the configured gateway, deduplicated against the explicitly configured
// models. The second return value reports whether discovery succeeded;
// when false (gateway unreachable, /v1/models unsupported, or an empty
// list that gives no usable signal) callers should fall back to the
// models.dev catalog.
func (r *Catalog) buildGatewayChoices(ctx context.Context) ([]ModelChoice, bool) {
	ids, err := r.listGatewayModels(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Gateway model discovery failed, falling back to catalog", "error", err)
		return nil, false
	}
	if len(ids) == 0 {
		slog.DebugContext(ctx, "Gateway returned no models, falling back to catalog")
		return nil, false
	}

	existingRefs := make(map[string]bool, len(r.Config.Models)*2)
	for name, cfg := range r.Config.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			existingRefs[cfg.Provider+"/"+cfg.Model] = true
		}
	}

	// The metadata callback memoizes full catalog entries so the display
	// name/pricing enrichment below reuses the lookup NormalizeIDs already
	// performed for its embedding/modality filters.
	metas := make(map[string]*modelsdev.Model)
	lookup := func(ctx context.Context, prov, model string) (modelsgateway.Metadata, bool) {
		if r.Store == nil {
			return modelsgateway.Metadata{}, false
		}
		m, err := r.Store.GetModel(ctx, modelsdev.NewID(prov, model))
		if err != nil {
			return modelsgateway.Metadata{}, false
		}
		metas[prov+"/"+model] = m
		return modelsgateway.Metadata{Family: m.Family, OutputModalities: m.Modalities.Output}, true
	}

	refs := modelsgateway.NormalizeIDs(ctx, ids, lookup)

	choices := make([]ModelChoice, 0, len(refs))
	for _, mr := range refs {
		ref := mr.String()
		if existingRefs[ref] {
			continue
		}
		existingRefs[ref] = true

		choice := ModelChoice{
			Name:      mr.Model,
			Ref:       ref,
			Provider:  mr.Provider,
			Model:     mr.Model,
			IsCatalog: true,
		}
		if meta := metas[ref]; meta != nil {
			if meta.Name != "" {
				choice.Name = meta.Name
			}
			applyCatalogMetadata(&choice, meta)
		}
		choices = append(choices, choice)
	}

	slog.DebugContext(ctx, "Built gateway model choices", "count", len(choices))
	return choices, true
}
