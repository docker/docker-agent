package v15

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upper is a stand-in expander: it substitutes ${env.NAME} / ${NAME} markers
// from a fixed table so the test stays free of pkg/environment.
func tableExpander(vars map[string]string) func(string) (string, error) {
	return func(s string) (string, error) {
		out := s
		for k, v := range vars {
			out = strings.ReplaceAll(out, "${env."+k+"}", v)
			out = strings.ReplaceAll(out, "${"+k+"}", v)
		}
		return out, nil
	}
}

func TestModelConfig_ExpandEnv(t *testing.T) {
	t.Parallel()

	vars := map[string]string{
		"NEMOTRON3_MODEL": "huggingface.co/unsloth/nemotron-3-nano:Q3_K_M",
		"DMR_BASE_URL":    "http://localhost:12434/engines/v1",
	}

	t.Run("expands model and base_url", func(t *testing.T) {
		t.Parallel()
		m := &ModelConfig{
			Provider: "dmr",
			Model:    "${env.NEMOTRON3_MODEL}",
			BaseURL:  "${DMR_BASE_URL}",
			TokenKey: "NEMOTRON3_MODEL", // must NOT be expanded: it names a var
		}
		require.NoError(t, m.ExpandEnv(tableExpander(vars)))
		assert.Equal(t, "huggingface.co/unsloth/nemotron-3-nano:Q3_K_M", m.Model)
		assert.Equal(t, "http://localhost:12434/engines/v1", m.BaseURL)
		assert.Equal(t, "NEMOTRON3_MODEL", m.TokenKey)
		assert.Equal(t, "dmr", m.Provider)
	})

	t.Run("plain values unchanged", func(t *testing.T) {
		t.Parallel()
		m := &ModelConfig{Model: "gpt-4o", BaseURL: "https://api.openai.com/v1"}
		require.NoError(t, m.ExpandEnv(tableExpander(vars)))
		assert.Equal(t, "gpt-4o", m.Model)
		assert.Equal(t, "https://api.openai.com/v1", m.BaseURL)
	})

	t.Run("empty fields and nil expander are no-ops", func(t *testing.T) {
		t.Parallel()
		m := &ModelConfig{Model: "${env.NEMOTRON3_MODEL}"}
		require.NoError(t, m.ExpandEnv(nil))
		assert.Equal(t, "${env.NEMOTRON3_MODEL}", m.Model)

		empty := &ModelConfig{}
		require.NoError(t, empty.ExpandEnv(tableExpander(vars)))
		assert.Empty(t, empty.Model)
		assert.Empty(t, empty.BaseURL)
	})

	t.Run("nil receiver is a no-op", func(t *testing.T) {
		t.Parallel()
		var m *ModelConfig
		require.NoError(t, m.ExpandEnv(tableExpander(vars)))
	})

	t.Run("propagates expander error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("variable not set")
		m := &ModelConfig{Model: "${env.MISSING}"}
		err := m.ExpandEnv(func(string) (string, error) { return "", boom })
		assert.ErrorIs(t, err, boom)
	})
}
