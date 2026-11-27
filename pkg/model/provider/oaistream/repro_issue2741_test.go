package oaistream

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// TestReproIssue2741_CustomProviderAttachmentDegradesToTextOnly reproduces
// https://github.com/docker/docker-agent/issues/2741.
//
// Custom / aliased OpenAI-compatible providers (xai, mistral, nebius, ollama,
// minimax, ...) route through this package with the raw Provider string from
// the agent config. Capability detection does a models.dev lookup keyed on
// modelsdev.NewID(cfg.Provider, cfg.DisplayOrModel()) — exactly what
// base.Config.ID() builds in production. When that "provider/model" pair is
// not present in the models.dev catalogue, modelinfo.LoadCaps swallows the
// "not found" error and returns the conservative, text-only capability set.
// attachment.Decide then drops every image/PDF part — silently, with only a
// warning log — even though the real endpoint serves a vision-capable model.
//
// This is a DIFFERENT code path from the already-fixed bare-name bug
// (#2737 / #2738, see TestConvertDocument_QualifiedIDRequired): there the
// provider component was empty, so the ID failed the id.IsValid() guard in
// Store.GetModel. Here the provider string is non-empty and valid; the lookup
// reaches the catalogue and misses, which is the residual bug #2741 tracks.
//
// The synthetic store mirrors the real models.dev catalogue (verified against
// https://models.dev/api.json):
//   - there is no "ollama" provider key (local tags like "llava" are never
//     catalogued),
//   - the "xai" provider exists but the legacy "grok-2-vision-1212" is gone
//     (the catalogue has drifted to the grok-4.x family),
//   - vision-capable models that ARE catalogued resolve correctly.
func TestReproIssue2741_CustomProviderAttachmentDegradesToTextOnly(t *testing.T) {
	t.Parallel()
	// In-memory catalogue: NewDatabaseStore sets the db directly, so no network
	// fetch and no knownProvider predicate are involved — fully deterministic.
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			// Catalogued, vision-capable. Used for the "preserved" controls.
			"openai": {Models: map[string]modelsdev.Model{
				"gpt-4o": {Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}},
			}},
			// "xai" exists, but only the current grok-4.x family is catalogued;
			// the legacy grok-2-vision-1212 the user configured is absent.
			"xai": {Models: map[string]modelsdev.Model{
				"grok-4.3": {Modalities: modelsdev.Modalities{Input: []string{"text", "image", "pdf"}}},
			}},
			// Note: there is deliberately NO "ollama" and NO "my-proxy" provider,
			// matching reality — models.dev has no such keys.
		},
	})

	imageDoc := []chat.MessagePart{{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "photo.jpg",
			MimeType: "image/jpeg",
			Source:   chat.DocumentSource{InlineData: minJPEG},
		},
	}}

	// dropped asserts that the image attachment is silently dropped for the
	// given provider/model ID with no capability override (the #2741 bug).
	dropped := func(t *testing.T, id modelsdev.ID) {
		t.Helper()
		parts := ConvertMultiContent(t.Context(), imageDoc, id, store, nil)
		assert.Empty(t, parts,
			"issue #2741: image silently dropped for %q (uncatalogued provider/model → text-only caps)", id.String())
	}

	// preserved asserts that the image survives as a native image part for the
	// given ID and optional override.
	preserved := func(t *testing.T, id modelsdev.ID, override *modelinfo.CapsOverride) {
		t.Helper()
		parts := ConvertMultiContent(t.Context(), imageDoc, id, store, override)
		require.Len(t, parts, 1, "expected the image to be preserved for %q", id.String())
		assert.Equal(t, "image_url", parts[0].Type, "expected a native image part for %q", id.String())
	}

	t.Run("ollama/llava: provider absent from models.dev → image dropped", func(t *testing.T) {
		// The issue's Ollama example. There is no "ollama" provider key at all,
		// so Store.GetModel returns "provider not found".
		dropped(t, modelsdev.NewID("ollama", "llava"))
	})

	t.Run("xai/grok-2-vision-1212: model absent under existing provider → image dropped", func(t *testing.T) {
		// The issue's xAI example. "xai" exists but this legacy vision model is
		// not catalogued, so Store.GetModel returns "model not found in provider".
		dropped(t, modelsdev.NewID("xai", "grok-2-vision-1212"))
	})

	t.Run("custom OpenAI-compatible proxy: provider absent → image dropped", func(t *testing.T) {
		// A self-contained custom endpoint (base_url + token) serving a
		// vision-capable gpt-4o. The provider string is non-empty and valid, so
		// this is NOT the bare-name bug — it misses at the catalogue lookup.
		id := modelsdev.NewID("my-proxy", "gpt-4o")
		require.True(t, id.IsValid(),
			"the provider string must be valid (non-empty) — otherwise this would be the already-fixed bare-name bug, not #2741")
		dropped(t, id)
	})

	t.Run("control: catalogued vision models keep their image", func(t *testing.T) {
		// Same image, same code path: the only thing that changes the outcome is
		// whether the provider/model string matches models.dev.
		preserved(t, modelsdev.NewID("openai", "gpt-4o"), nil)
		preserved(t, modelsdev.NewID("xai", "grok-4.3"), nil)
	})

	t.Run("fix: capabilities override restores the image for uncatalogued models", func(t *testing.T) {
		// With an explicit override (config: capabilities.image=true), the same
		// uncatalogued provider/model IDs that dropped above now keep the image,
		// because ResolveCaps short-circuits the models.dev lookup. This is what
		// base.Config.CapsOverride() supplies in production from the YAML.
		override := &modelinfo.CapsOverride{Image: true}
		preserved(t, modelsdev.NewID("ollama", "llava"), override)
		preserved(t, modelsdev.NewID("xai", "grok-2-vision-1212"), override)
		preserved(t, modelsdev.NewID("my-proxy", "gpt-4o"), override)
	})
}
