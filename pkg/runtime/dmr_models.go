package runtime

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// dmrModelsTTL is how long a Docker Model Runner /models response (or failure)
// is reused before DMR is queried again. It keeps the model picker snappy on
// repeated opens while still picking up newly-pulled models eventually.
const dmrModelsTTL = 1 * time.Minute

// dmrModelsCache memoizes the result of DMR model discovery, including
// failures, so an unreachable or absent Model Runner is not re-queried on
// every picker open. The mutex only guards the cached fields; the lookup
// itself runs outside the lock, coalesced by the singleflight group so
// concurrent callers share one in-flight request.
type dmrModelsCache struct {
	mu        sync.Mutex
	sf        singleflight.Group
	models    []dmrmodels.Model
	err       error
	fetchedAt time.Time
}

// listDMRModels returns the models available to Docker Model Runner, using
// the runtime's cache when fresh. It returns (nil, nil) when no lister is
// configured (e.g. runtimes built directly in tests), so DMR discovery is
// opt-in via NewLocalRuntime.
func (r *LocalRuntime) listDMRModels(ctx context.Context) ([]dmrmodels.Model, error) {
	if r.dmrModelLister == nil {
		slog.DebugContext(ctx, "DMR model discovery skipped; no lister configured")
		return nil, nil
	}

	now := time.Now
	if r.now != nil {
		now = r.now
	}

	c := &r.dmrModels

	readFresh := func() (ids []dmrmodels.Model, ok bool, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.fetchedAt.IsZero() && now().Sub(c.fetchedAt) < dmrModelsTTL {
			return c.models, true, c.err
		}
		return nil, false, nil
	}

	if ids, ok, err := readFresh(); ok {
		slog.DebugContext(ctx, "DMR model discovery cache hit", "models", len(ids), "error", err)
		return ids, err
	}

	start := time.Now()
	v, err, _ := c.sf.Do("models", func() (any, error) {
		// Double-check the cache now that we hold the in-flight slot: a caller
		// that read a stale cache right before a concurrent singleflight
		// completed would otherwise trigger a redundant fetch.
		if ids, ok, err := readFresh(); ok {
			return ids, err
		}

		ids, err := r.dmrModelLister(ctx)
		if err != nil && ctx.Err() != nil {
			return ids, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.models, c.err, c.fetchedAt = ids, err, now()
		return ids, err
	})
	if err != nil {
		slog.DebugContext(ctx, "DMR model discovery fetch completed", "duration", time.Since(start), "error", err)
		return nil, err
	}
	ids := v.([]dmrmodels.Model)
	slog.DebugContext(ctx, "DMR model discovery fetch completed", "duration", time.Since(start), "models", len(ids))
	return ids, nil
}

// buildDMRChoices builds ModelChoice entries for the models currently pulled
// in Docker Model Runner, deduplicated against the explicitly configured
// models. DMR models aren't part of the models.dev catalog, so without this
// the picker shows nothing for a working local Model Runner. When DMR is not
// installed or unreachable it returns nil.
func (r *LocalRuntime) buildDMRChoices(ctx context.Context) []ModelChoice {
	ids, err := r.listDMRModels(ctx)
	if err != nil {
		slog.DebugContext(ctx, "DMR model discovery failed, skipping DMR picker entries", "error", err)
		return nil
	}
	if len(ids) == 0 {
		return nil
	}

	existingRefs := make(map[string]bool, len(r.modelSwitcherCfg.Models)*2)
	for name, cfg := range r.modelSwitcherCfg.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			existingRefs[cfg.Provider+"/"+cfg.Model] = true
		}
	}

	choices := make([]ModelChoice, 0, len(ids))
	for _, model := range ids {
		id := model.ID
		// DMR model IDs (e.g. "ai/qwen3:latest") contain slashes; the ref is
		// "dmr/<id>" and ParseModelRef cuts on the first slash, so it
		// round-trips back to provider="dmr", model="<id>".
		ref := "dmr/" + id

		// Resolve catalog metadata before the embedding filter so a model
		// whose models.dev Family is "text-embedding" is filtered even when
		// its ID doesn't contain "embed".
		var meta *modelsdev.Model
		if r.modelsStore != nil {
			if m, err := r.modelsStore.GetModel(ctx, modelsdev.NewID("dmr", id)); err == nil {
				meta = m
			}
		}
		family := ""
		if meta != nil {
			family = meta.Family
		}
		if isEmbeddingModel(family, id) {
			continue
		}

		if existingRefs[ref] {
			continue
		}
		existingRefs[ref] = true

		choice := ModelChoice{
			Name:     id,
			Ref:      ref,
			Provider: "dmr",
			Model:    id,
			// Discovered (not explicitly configured), so it groups under the
			// picker's "Other models" separator alongside gateway/catalog
			// entries rather than intermixing with the configured models.
			IsCatalog: true,
		}
		if meta != nil {
			if meta.Name != "" {
				choice.Name = meta.Name
			}
			applyCatalogMetadata(&choice, meta)
		}
		applyDMRMetadata(&choice, model.Metadata)
		choices = append(choices, choice)
	}

	slog.DebugContext(ctx, "Built DMR model choices", "count", len(choices))
	return choices
}

func applyDMRMetadata(choice *ModelChoice, metadata *dmrmodels.Metadata) {
	if metadata == nil {
		return
	}
	if metadata.ContextWindow > 0 {
		choice.ContextLimit = int(metadata.ContextWindow)
	}
	choice.Architecture = metadata.Architecture
	choice.Parameters = metadata.Parameters
	choice.Quantization = metadata.Quantization
	choice.Size = metadata.Size
}

// Configured models at explicit endpoints must not inherit another runner's metadata.
func (r *LocalRuntime) populateDMRChoices(ctx context.Context, choices []ModelChoice) {
	if r.modelSwitcherCfg.ModelsGateway != "" {
		return
	}
	models, err := r.listDMRModels(ctx)
	if err != nil {
		return
	}
	metadata := make(map[string]*dmrmodels.Metadata, len(models))
	for _, model := range models {
		metadata[model.ID] = model.Metadata
	}
	for i := range choices {
		choice := &choices[i]
		cfg := r.modelSwitcherCfg.Models[choice.Ref]
		if cfg.Provider != "dmr" || cfg.BaseURL != "" {
			continue
		}
		meta := metadata[cfg.Model]
		if meta == nil {
			meta = metadata[cfg.Model+":latest"]
		}
		applyDMRMetadata(choice, meta)
		if n := latest.ContextSizeFromProviderOpts(cfg.ProviderOpts); n > 0 {
			if n > math.MaxInt {
				choice.ContextLimit = math.MaxInt
			} else {
				choice.ContextLimit = int(n)
			}
		}
	}
}
