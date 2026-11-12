package latest

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsetMaxOutputBytesValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		toolset Toolset
		wantErr string
	}{
		{name: "omitted", toolset: Toolset{Type: "openapi", URL: "https://api.example.com/spec.yaml"}},
		{name: "disabled", toolset: Toolset{Type: "openapi", URL: "https://api.example.com/spec.yaml", MaxOutputBytes: new(0)}},
		{name: "positive", toolset: Toolset{Type: "openapi", URL: "https://api.example.com/spec.yaml", MaxOutputBytes: new(1024)}},
		{name: "negative", toolset: Toolset{Type: "openapi", URL: "https://api.example.com/spec.yaml", MaxOutputBytes: new(-1)}, wantErr: "max_output_bytes must not be negative"},
		{name: "shell zero", toolset: Toolset{Type: "shell", MaxOutputBytes: new(0)}, wantErr: "max_output_bytes can only be used with type 'openapi'"},
		{name: "fetch positive", toolset: Toolset{Type: "fetch", MaxOutputBytes: new(1024)}, wantErr: "max_output_bytes can only be used with type 'openapi'"},
		{name: "missing type", toolset: Toolset{MaxOutputBytes: new(0)}, wantErr: "max_output_bytes can only be used with type 'openapi'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inline := Config{Agents: Agents{{Name: "root", Toolsets: []Toolset{tt.toolset}}}}
			named := Config{Toolsets: map[string]Toolset{"api": tt.toolset}}
			data, err := yaml.Marshal(tt.toolset)
			require.NoError(t, err)
			var parsed Toolset
			parseErr := yaml.Unmarshal(data, &parsed)

			if tt.wantErr != "" {
				require.EqualError(t, tt.toolset.validate(), tt.wantErr)
				require.ErrorContains(t, inline.Validate(), tt.wantErr)
				require.ErrorContains(t, named.Validate(), "toolsets.api: "+tt.wantErr)
				require.ErrorContains(t, parseErr, tt.wantErr)
				return
			}
			require.NoError(t, tt.toolset.validate())
			require.NoError(t, inline.Validate())
			require.NoError(t, named.Validate())
			require.NoError(t, parseErr)
			assert.Equal(t, tt.toolset.MaxOutputBytes, parsed.MaxOutputBytes)
		})
	}
}

func TestToolsetMaxOutputBytesRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value *int
	}{
		{name: "omitted"},
		{name: "disabled", value: new(0)},
		{name: "positive", value: new(1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			toolset := Toolset{Type: "openapi", URL: "https://api.example.com/spec.yaml", MaxOutputBytes: tt.value}

			jsonData, err := json.Marshal(toolset)
			require.NoError(t, err)
			var jsonFields map[string]any
			require.NoError(t, json.Unmarshal(jsonData, &jsonFields))
			var fromJSON Toolset
			require.NoError(t, json.Unmarshal(jsonData, &fromJSON))
			assert.Equal(t, tt.value, fromJSON.MaxOutputBytes)

			yamlData, err := yaml.Marshal(toolset)
			require.NoError(t, err)
			var yamlFields map[string]any
			require.NoError(t, yaml.Unmarshal(yamlData, &yamlFields))
			var fromYAML Toolset
			require.NoError(t, yaml.Unmarshal(yamlData, &fromYAML))
			assert.Equal(t, tt.value, fromYAML.MaxOutputBytes)

			if tt.value == nil {
				assert.NotContains(t, jsonFields, "max_output_bytes")
				assert.NotContains(t, yamlFields, "max_output_bytes")
				return
			}
			assert.EqualValues(t, *tt.value, jsonFields["max_output_bytes"])
			assert.EqualValues(t, *tt.value, yamlFields["max_output_bytes"])
		})
	}
}
