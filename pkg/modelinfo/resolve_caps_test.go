package modelinfo

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// visionStore returns a store that catalogues a single vision+pdf model under
// openai/gpt-4o and nothing else.
func visionStore() *modelsdev.Store {
	return modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {Models: map[string]modelsdev.Model{
			"gpt-4o": {Modalities: modelsdev.Modalities{Input: []string{"text", "image", "pdf"}}},
		}},
	}})
}

func TestResolveCaps_OverrideWins(t *testing.T) {
	t.Parallel()

	// An override is authoritative even when the store would say otherwise (or
	// is nil): no models.dev lookup happens.
	cases := []struct {
		name      string
		override  *CapsOverride
		store     *modelsdev.Store
		id        modelsdev.ID
		wantImage bool
		wantPDF   bool
	}{
		{
			name:      "image only, uncatalogued provider",
			override:  &CapsOverride{Image: true},
			store:     visionStore(),
			id:        modelsdev.NewID("ollama", "llava"), // absent from store
			wantImage: true,
			wantPDF:   false,
		},
		{
			name:      "image and pdf, nil store",
			override:  &CapsOverride{Image: true, PDF: true},
			store:     nil,
			id:        modelsdev.NewID("my-proxy", "gpt-4o"),
			wantImage: true,
			wantPDF:   true,
		},
		{
			name:      "explicit text-only override masks a catalogued vision model",
			override:  &CapsOverride{}, // both false
			store:     visionStore(),
			id:        modelsdev.NewID("openai", "gpt-4o"), // catalogued as vision
			wantImage: false,
			wantPDF:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mc := ResolveCaps(t.Context(), tc.store, tc.id, tc.override)
			assert.Equal(t, tc.wantImage, mc.Supports("image/jpeg"))
			assert.Equal(t, tc.wantPDF, mc.Supports("application/pdf"))
		})
	}
}

func TestResolveCaps_NilOverrideFallsBackToModelsDev(t *testing.T) {
	t.Parallel()

	store := visionStore()

	// Catalogued vision model resolves to vision caps.
	hit := ResolveCaps(t.Context(), store, modelsdev.NewID("openai", "gpt-4o"), nil)
	assert.True(t, hit.Supports("image/jpeg"))
	assert.True(t, hit.Supports("application/pdf"))

	// Uncatalogued model degrades to text-only (the #2741 default without an override).
	miss := ResolveCaps(t.Context(), store, modelsdev.NewID("ollama", "llava"), nil)
	assert.False(t, miss.Supports("image/jpeg"))
	assert.False(t, miss.Supports("application/pdf"))
	assert.True(t, miss.Supports("text/plain"))
}

// TestLoadCaps_MissDiagnosticDedup verifies the Option C diagnostic: a
// models.dev miss is logged once per model id, not on every lookup.
//
// It swaps the default slog logger and is deliberately NOT parallel: Go runs
// non-parallel tests in a sequential phase before parallel ones start, so no
// other test logs into the buffer concurrently. The assertion further filters
// by a unique model id to stay robust regardless.
func TestLoadCaps_MissDiagnosticDedup(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := visionStore()
	// Unique id so the package-global dedup map cannot hide the first warning
	// behind another test's lookup of the same id.
	id := modelsdev.NewID("dedup-probe-provider", "dedup-probe-model")

	for range 3 {
		mc := LoadCaps(t.Context(), store, id)
		require.False(t, mc.Supports("image/jpeg"))
	}

	lines := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "dedup-probe-model") {
			lines++
		}
	}
	assert.Equal(t, 1, lines, "miss diagnostic must be logged exactly once per model id, got %d", lines)
	assert.Contains(t, buf.String(), "not found in models.dev")
	assert.Contains(t, buf.String(), "capabilities", "diagnostic should point at the config override")
}

// TestResolveCapsFromModel pins the store-free resolution path used by the
// runtime's strip transform: same precedence contract as ResolveCaps, but
// operating on an already-fetched models.dev record.
func TestResolveCapsFromModel(t *testing.T) {
	t.Parallel()

	multimodal := &modelsdev.Model{Modalities: modelsdev.Modalities{Input: []string{"text", "image", "audio", "video"}}}

	cases := []struct {
		name                     string
		model                    *modelsdev.Model
		override                 *CapsOverride
		image, pdf, audio, video bool
	}{
		{name: "modalities drive caps", model: multimodal, image: true, audio: true, video: true},
		{name: "override wins over modalities", model: multimodal, override: &CapsOverride{Audio: true}, audio: true},
		{name: "override wins over nil model", model: nil, override: &CapsOverride{Image: true, PDF: true, Audio: true, Video: true}, image: true, pdf: true, audio: true, video: true},
		{name: "nil model is conservative text-only", model: nil},
		{name: "empty modalities are conservative text-only", model: &modelsdev.Model{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mc := ResolveCapsFromModel(tc.model, tc.override)
			assert.Equal(t, tc.image, mc.SupportsImage())
			assert.Equal(t, tc.pdf, mc.SupportsPDF())
			assert.Equal(t, tc.audio, mc.SupportsAudio())
			assert.Equal(t, tc.video, mc.SupportsVideo())
		})
	}
}

func TestResolveCaps_ChatGPTCatalogFallback(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		model    string
		direct   []string
		override *CapsOverride
		want     ModelCapabilities
	}{
		{name: "image input only", model: "gpt-6.1-sol", want: CapsWith(true, false, false, false)},
		{name: "unknown model", model: "unknown", want: CapsWith(false, false, false, false)},
		{name: "direct entry wins", model: "gpt-6.1-sol", direct: []string{"text", "audio"}, want: CapsWith(false, false, true, false)},
		{name: "explicit false wins", model: "gpt-6.1-sol", override: &CapsOverride{}, want: CapsWith(false, false, false, false)},
		{name: "explicit true wins even for unknown", model: "unknown", override: &CapsOverride{Image: true, PDF: true}, want: CapsWith(true, true, false, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			providers := map[string]modelsdev.Provider{
				"openai": {Models: map[string]modelsdev.Model{
					"gpt-6.1-sol": {Modalities: modelsdev.Modalities{Input: []string{"text", "image", "pdf", "audio", "video"}}},
				}},
			}
			if tc.direct != nil {
				providers["chatgpt"] = modelsdev.Provider{Models: map[string]modelsdev.Model{
					tc.model: {Modalities: modelsdev.Modalities{Input: tc.direct}},
				}}
			}
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: providers})
			got := ResolveCaps(t.Context(), store, modelsdev.NewID("chatgpt", tc.model), tc.override)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAliasedCatalogCaps_Misses(t *testing.T) {
	t.Parallel()
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {Models: map[string]modelsdev.Model{
			"vision":    {Modalities: modelsdev.Modalities{Input: []string{"image", "pdf"}}},
			"text-only": {Modalities: modelsdev.Modalities{Input: []string{"text"}, Output: []string{"image"}}},
		}},
	}})
	for _, id := range []modelsdev.ID{
		modelsdev.NewID("chatgpt", "missing"),
		modelsdev.NewID("other", "vision"),
		modelsdev.NewID("", "vision"),
		modelsdev.NewID("chatgpt", ""),
	} {
		caps, ok := AliasedCatalogCaps(t.Context(), store, id)
		assert.False(t, ok, id.String())
		assert.Equal(t, ModelCapabilities{}, caps)
	}
	caps, ok := AliasedCatalogCaps(t.Context(), store, modelsdev.NewID("chatgpt", "text-only"))
	assert.True(t, ok)
	assert.Equal(t, ModelCapabilities{}, caps, "output images must not imply image input")

	caps, ok = AliasedCatalogCaps(t.Context(), nil, modelsdev.NewID("chatgpt", "vision"))
	assert.False(t, ok)
	assert.Equal(t, ModelCapabilities{}, caps)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	caps, ok = AliasedCatalogCaps(ctx, store, modelsdev.NewID("chatgpt", "vision"))
	assert.False(t, ok)
	assert.Equal(t, ModelCapabilities{}, caps)
}
