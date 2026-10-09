package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// modelsDevCatalogProviders validates ChatGPT example names against OpenAI's
// catalog without treating the subscription backend as a full metadata alias.
var modelsDevCatalogProviders = map[string]string{
	"chatgpt": "openai",
}

// modelsDevAbsentProviders lists providers that are valid at runtime but
// whose example model reference can't be validated against models.dev,
// even via modelsDevCatalogProviders above. The test skips models.dev
// lookups for these to avoid false failures.
var modelsDevAbsentProviders = map[string]bool{
	"atlascloud":            true, // Atlas Cloud model IDs come from its own /v1/models endpoint, not models.dev.
	"dmr":                   true, // Docker Model Runner (local, not in catalog)
	"cloudflare-workers-ai": true, // example uses an @cf/... model id not present in the models.dev snapshot (only variant ids like -fp8 are listed)
	"cloudflare-ai-gateway": true, // multi-provider router; example model ids use the gateway's provider/model form, not guaranteed to match a models.dev id
}

func collectExamples(t *testing.T) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(filepath.Join("..", "..", "examples"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			ext := filepath.Ext(path)
			if ext == ".yaml" || ext == ".hcl" {
				files = append(files, path)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.NotEmpty(t, files)

	return files
}

// catalogModelRefs returns the models.dev IDs that TestParseExamples and
// TestExamplesAgainstLiveModelsDev must resolve for cfg, applying the same
// skip rules to both: first_available selectors are resolved at runtime
// from the environment's credentials, routed models span multiple
// providers, custom providers are self-contained (already validated via
// cfg.Providers), modelsDevAbsentProviders lists providers models.dev
// deliberately does not catalog, and modelsDevCatalogProviders validates
// ChatGPT model names against OpenAI's catalog.
func catalogModelRefs(cfg *latest.Config) []modelsdev.ID {
	var ids []modelsdev.ID
	for _, model := range cfg.Models {
		if model.IsFirstAvailable() {
			continue
		}
		if model.Provider == "" || model.Model == "" {
			continue
		}
		if modelsDevAbsentProviders[model.Provider] {
			continue
		}
		if len(model.Routing) > 0 {
			continue
		}
		if _, isCustomProvider := cfg.Providers[model.Provider]; isCustomProvider {
			continue
		}
		catalogProvider := model.Provider
		if id, ok := modelsDevCatalogProviders[model.Provider]; ok {
			catalogProvider = id
		}
		ids = append(ids, modelsdev.NewID(catalogProvider, model.Model))
	}
	return ids
}

func TestParseExamples(t *testing.T) {
	t.Parallel()

	// Resolved against the embedded/committed snapshot.json only — never the
	// network — so this is safe as a required, PR-blocking check: it can only
	// fail when the PR's own diff (an example or the committed snapshot)
	// introduces an inconsistency, never because the live models.dev catalog
	// moved on since the snapshot was last refreshed. See issue #4134.
	modelsStore := modelsdev.NewDatabaseStore(modelsdev.EmbeddedSnapshot())

	for _, file := range collectExamples(t) {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(t.Context(), hcl.NewSource(NewFileSource(file)))

			require.NoError(t, err)
			require.Equal(t, latest.Version, cfg.Version, "Version should be %d in %s", latest.Version, file)
			require.NotEmpty(t, cfg.Agents)
			require.NotEmpty(t, cfg.Agents.First().Description, "Description should not be empty in %s", file)

			for _, agent := range cfg.Agents {
				if agent.Harness == nil {
					require.NotEmpty(t, agent.Model)
				}
				require.NotEmpty(t, agent.Instruction, "Instruction should not be empty in %s", file)
			}

			for _, model := range cfg.Models {
				// Skip first_available selectors - their provider/model is
				// resolved at load time from the environment's credentials.
				if model.IsFirstAvailable() {
					continue
				}
				require.NotEmpty(t, model.Provider)
				require.NotEmpty(t, model.Model)
			}

			for _, id := range catalogModelRefs(cfg) {
				model, err := modelsStore.GetModel(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, model)
			}
		})
	}
}

// TestExamplesAgainstLiveModelsDev validates the same example model
// references as TestParseExamples, but against the live models.dev API
// instead of the committed snapshot. It is opt-in (gated on
// CHECK_MODELS_DEV_LIVE, mirroring TestSnapshotDateIsFresh) and skipped by
// default: a failure here means the *external* catalog has drifted since
// the snapshot was last refreshed, which is unrelated to any PR's diff and
// must never gate a merge (issue #4134). It is intended to run on a
// schedule; see .github/workflows/models-live-check.yml.
func TestExamplesAgainstLiveModelsDev(t *testing.T) {
	t.Parallel()

	if os.Getenv("CHECK_MODELS_DEV_LIVE") == "" {
		t.Skip("set CHECK_MODELS_DEV_LIVE=1 to validate examples against the live models.dev catalog")
	}

	db, err := modelsdev.Fetch(t.Context())
	require.NoError(t, err, "failed to fetch the live models.dev catalog")
	modelsStore := modelsdev.NewDatabaseStore(db)

	var drifted []string
	for _, file := range collectExamples(t) {
		cfg, err := Load(t.Context(), hcl.NewSource(NewFileSource(file)))
		require.NoError(t, err)

		for _, id := range catalogModelRefs(cfg) {
			if _, err := modelsStore.GetModel(t.Context(), id); err != nil {
				drifted = append(drifted, fmt.Sprintf("%s: %s (%v)", file, id.String(), err))
			}
		}
	}

	if len(drifted) == 0 {
		return
	}
	sort.Strings(drifted)
	t.Fatalf("live models.dev catalog drift detected (NOT caused by this PR's diff — the\n"+
		"committed snapshot and TestParseExamples are unaffected): the following example model\n"+
		"references resolve against the committed snapshot (dated %s) but not against the\n"+
		"live models.dev API:\n  %s\n\n"+
		"Fix by refreshing the snapshot (`task update-models`) and/or updating the affected\n"+
		"example(s) to reference a model the live catalog still carries.",
		modelsdev.SnapshotDate().Format("2006-01-02"), strings.Join(drifted, "\n  "))
}

func TestParseExamplesAfterMarshalling(t *testing.T) {
	t.Parallel()

	for _, file := range collectExamples(t) {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(t.Context(), hcl.NewSource(NewFileSource(file)))
			require.NoError(t, err)

			// Make sure that a config can be marshalled and parsed again.
			// We've had marshalling issues in the past.
			buf, err := yaml.Marshal(cfg)
			require.NoError(t, err)

			// The marshalled bytes are always YAML, so re-load them under a
			// .yaml-named source even when the original example was HCL.
			name := strings.TrimSuffix(file, filepath.Ext(file)) + ".yaml"
			_, err = Load(t.Context(), NewBytesSource(name, buf))
			require.NoError(t, err)
		})
	}
}

// TestHCLExamplesMatchYAML verifies that every .hcl example file produces a
// configuration identical to its .yaml sibling, ensuring the HCL surface
// stays in sync with the YAML schema.
func TestHCLExamplesMatchYAML(t *testing.T) {
	t.Parallel()

	for _, file := range collectExamples(t) {
		if filepath.Ext(file) != ".hcl" {
			continue
		}
		yamlFile := strings.TrimSuffix(file, ".hcl") + ".yaml"
		if _, err := os.Stat(yamlFile); err != nil {
			continue
		}
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			cfgHCL, err := Load(t.Context(), hcl.NewSource(NewFileSource(file)))
			require.NoError(t, err)
			cfgYAML, err := Load(t.Context(), NewFileSource(yamlFile))
			require.NoError(t, err)

			require.Equal(t, cfgYAML, cfgHCL, "HCL config %s differs from YAML sibling %s", file, yamlFile)
		})
	}
}

func TestExampleProvidersCanonical(t *testing.T) {
	t.Parallel()
	for _, example := range collectExamples(t) {
		t.Run(example, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(t.Context(), hcl.NewSource(NewFileSource(example)))
			require.NoError(t, err)
			data, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			var document any
			require.NoError(t, yaml.Unmarshal(data, &document))
			var check func(any)
			check = func(value any) {
				switch node := value.(type) {
				case map[string]any:
					for key, child := range node {
						if key == "provider" {
							if id, ok := child.(string); ok {
								assert.Equal(t, id, modelsdev.CanonicalProviderID(id), "provider in %s", example)
							}
						}
						check(child)
					}
				case []any:
					for _, child := range node {
						check(child)
					}
				case string:
					// Inline refs also appear in fallback, routing and first_available lists.
					if id, _, ok := strings.Cut(node, "/"); ok {
						assert.Equal(t, id, modelsdev.CanonicalProviderID(id), "inline reference %s", node)
					}
				}
			}
			check(document)
		})
	}
}
