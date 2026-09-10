package latest

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRAGConfig_GetIndexingTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *RAGConfig
		want time.Duration
	}{
		{name: "omitted defaults to 30m", cfg: &RAGConfig{}, want: 30 * time.Minute},
		{name: "explicit 0s is unbounded", cfg: &RAGConfig{IndexingTimeout: &Duration{Duration: 0}}, want: 0},
		{name: "explicit value", cfg: &RAGConfig{IndexingTimeout: &Duration{Duration: 2 * time.Hour}}, want: 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.cfg.GetIndexingTimeout())
		})
	}
}

func TestRAGConfig_Validate_IndexingTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *RAGConfig
		wantErr string
	}{
		{name: "nil is valid (default applies)", cfg: &RAGConfig{}},
		{name: "explicit zero is valid (unbounded)", cfg: &RAGConfig{IndexingTimeout: &Duration{Duration: 0}}},
		{name: "positive is valid", cfg: &RAGConfig{IndexingTimeout: &Duration{Duration: 2 * time.Hour}}},
		{
			name:    "negative is rejected",
			cfg:     &RAGConfig{IndexingTimeout: &Duration{Duration: -time.Second}},
			wantErr: "indexing_timeout must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestConfigValidate_RAGTopLevelIndexingTimeout pins that a top-level "rag:"
// definition is validated even though it skips Toolset.validate() during
// YAML unmarshaling (see RAGToolset.UnmarshalYAML) — Config.Validate() must
// check it directly.
func TestConfigValidate_RAGTopLevelIndexingTimeout(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		RAG: map[string]RAGToolset{
			"docs": {Toolset: Toolset{Type: "rag", RAGConfig: &RAGConfig{
				IndexingTimeout: &Duration{Duration: -time.Second},
			}}},
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rag.docs")
	assert.Contains(t, err.Error(), "indexing_timeout must not be negative")
}

// TestRAGToolset_MarshalYAML_IndexingTimeout pins the flattening added
// alongside respect_vcs: an explicit "0s" (unbounded) must round-trip
// distinctly from an omitted field (which defaults to 30m via
// GetIndexingTimeout), so RAGConfig.IndexingTimeout.String() is used
// directly rather than Duration.MarshalYAML (which renders zero as "").
func TestRAGToolset_MarshalYAML_IndexingTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		indexingTimeout *Duration
		wantYAML        string // substring expected in the marshaled output
		wantAbsent      bool
	}{
		{name: "omitted", indexingTimeout: nil, wantAbsent: true},
		{name: "explicit zero (unbounded)", indexingTimeout: &Duration{Duration: 0}, wantYAML: "indexing_timeout: 0s"},
		{name: "explicit value", indexingTimeout: &Duration{Duration: 2 * time.Hour}, wantYAML: "indexing_timeout: 2h0m0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			toolset := RAGToolset{Toolset: Toolset{
				Type:      "rag",
				RAGConfig: &RAGConfig{IndexingTimeout: tt.indexingTimeout},
			}}

			out, err := toolset.MarshalYAML()
			require.NoError(t, err)

			data, err := yaml.Marshal(out)
			require.NoError(t, err)

			if tt.wantAbsent {
				assert.NotContains(t, string(data), "indexing_timeout")
				return
			}
			assert.Contains(t, string(data), tt.wantYAML)
		})
	}
}

// TestRAGToolset_UnmarshalYAML_IndexingTimeout pins the top-level "rag:"
// spelling (flattened alongside docs/strategies), mirroring the inline
// "rag_config:" spelling exercised elsewhere.
func TestRAGToolset_UnmarshalYAML_IndexingTimeout(t *testing.T) {
	t.Parallel()

	input := []byte(`tool:
  description: test
indexing_timeout: 2h
docs: [./docs]
strategies:
  - type: bm25
`)

	var toolset RAGToolset
	require.NoError(t, yaml.Unmarshal(input, &toolset))

	require.NotNil(t, toolset.RAGConfig)
	require.NotNil(t, toolset.RAGConfig.IndexingTimeout)
	assert.Equal(t, 2*time.Hour, toolset.RAGConfig.IndexingTimeout.Duration)
	assert.Equal(t, 2*time.Hour, toolset.RAGConfig.GetIndexingTimeout())
}

// TestRAGConfig_UnmarshalYAML_InlineIndexingTimeout mirrors the above for
// the "rag_config:" inline spelling used directly on an agent's toolset.
func TestRAGConfig_UnmarshalYAML_InlineIndexingTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{name: "omitted defaults to 30m", yaml: `tool: {}`, want: 30 * time.Minute},
		{name: "explicit 0s is unbounded", yaml: "tool: {}\nindexing_timeout: 0s\n", want: 0},
		{name: "explicit value", yaml: "tool: {}\nindexing_timeout: 2h\n", want: 2 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg RAGConfig
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &cfg))
			assert.Equal(t, tt.want, cfg.GetIndexingTimeout())
		})
	}
}
