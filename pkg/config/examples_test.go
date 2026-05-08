package config

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func collectExamples(t *testing.T) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(filepath.Join("..", "..", "examples"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".yaml" {
			files = append(files, path)
		}
		return nil
	})
	require.NoError(t, err)
	assert.NotEmpty(t, files)

	return files
}

func TestParseExamples(t *testing.T) {
	modelsStore, err := modelsdev.NewStore()
	require.NoError(t, err)

	for _, file := range collectExamples(t) {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(t.Context(), NewFileSource(file, nil))

			require.NoError(t, err)
			require.Equal(t, latest.Version, cfg.Version, "Version should be %d in %s", latest.Version, file)
			require.NotEmpty(t, cfg.Agents)
			require.NotEmpty(t, cfg.Agents.First().Description, "Description should not be empty in %s", file)

			for _, agent := range cfg.Agents {
				require.NotEmpty(t, agent.Model)
				require.NotEmpty(t, agent.Instruction, "Instruction should not be empty in %s", file)
			}

			for _, model := range cfg.Models {
				require.NotEmpty(t, model.Provider)
				require.NotEmpty(t, model.Model)
				// Skip providers that don't have entries in models.dev
				if model.Provider == "dmr" {
					continue
				}
				// Skip models with routing rules - they use multiple providers
				if len(model.Routing) > 0 {
					continue
				}
				// Skip models that use custom providers (defined in cfg.Providers)
				if _, isCustomProvider := cfg.Providers[model.Provider]; isCustomProvider {
					continue
				}

				model, err := modelsStore.GetModel(t.Context(), model.Provider+"/"+model.Model)
				require.NoError(t, err)
				require.NotNil(t, model)
			}
		})
	}
}

func TestParseExamplesAfterMarshalling(t *testing.T) {
	for _, file := range collectExamples(t) {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			src := NewFileSource(file, nil)
			cfg, err := Load(t.Context(), NewFileSource(file, nil))
			require.NoError(t, err)

			// Make sure that a config can be marshalled and parsed again.
			// We've had marshalling issues in the past.
			buf, err := yaml.Marshal(cfg)
			require.NoError(t, err)

			_, err = Load(t.Context(), NewBytesSource(src.Name(), buf, nil))
			require.NoError(t, err)
		})
	}
}
