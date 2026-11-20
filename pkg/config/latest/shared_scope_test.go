package latest

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedScope_Unmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		want    SharedScope
		wantErr string
	}{
		{name: "omitted", yaml: "type: todo", want: SharedScopeNone},
		{name: "true", yaml: "type: todo\nshared: true", want: SharedScopeProcess},
		{name: "false", yaml: "type: todo\nshared: false", want: SharedScopeNone},
		{name: "team", yaml: "type: todo\nshared: team", want: SharedScopeTeam},
		{name: "unknown string", yaml: "type: todo\nshared: everyone", wantErr: `invalid shared value "everyone"`},
		{name: "process is not a spelling", yaml: "type: todo\nshared: process", wantErr: `invalid shared value "process"`},
		{name: "number", yaml: "type: todo\nshared: 1", wantErr: "invalid shared value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var ts Toolset
			err := yaml.Unmarshal([]byte(tt.yaml), &ts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, ts.Shared)
		})
	}
}

func TestSharedScope_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, scope := range []SharedScope{SharedScopeNone, SharedScopeProcess, SharedScopeTeam} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()

			in := Toolset{Type: "todo", Shared: scope}

			data, err := json.Marshal(in)
			require.NoError(t, err)
			var fromJSON Toolset
			require.NoError(t, json.Unmarshal(data, &fromJSON))
			assert.Equal(t, scope, fromJSON.Shared)

			data, err = yaml.Marshal(in)
			require.NoError(t, err)
			var fromYAML Toolset
			require.NoError(t, yaml.Unmarshal(data, &fromYAML))
			assert.Equal(t, scope, fromYAML.Shared)
		})
	}
}

func TestSharedScope_MarshalsAsConfigSpelling(t *testing.T) {
	t.Parallel()

	shared := func(scope SharedScope) (any, bool) {
		data, err := json.Marshal(Toolset{Type: "todo", Shared: scope})
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		v, ok := fields["shared"]
		return v, ok
	}

	v, ok := shared(SharedScopeProcess)
	assert.True(t, ok)
	assert.Equal(t, true, v)

	v, ok = shared(SharedScopeTeam)
	assert.True(t, ok)
	assert.Equal(t, "team", v)

	_, ok = shared(SharedScopeNone)
	assert.False(t, ok, "unset is omitted")
}
