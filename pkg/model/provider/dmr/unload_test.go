package dmr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnloadURL covers every branch of the URL-resolution algorithm
// in one place. The builtin and any other consumer of [UnloadURL]
// rely on these properties, so the test pins the convention here
// where DMR owns it (sibling to [buildConfigureURL]'s `_configure`
// math).
func TestUnloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		baseURL     string
		unloadAPI   string
		want        string
		errContains string // empty ⇒ expect success
	}{
		// Default derivation (no unload_api set).
		{
			name:    "default: standard engines path",
			baseURL: "http://127.0.0.1:12434/engines/v1/",
			want:    "http://127.0.0.1:12434/engines/_unload",
		},
		{
			name:    "default: no trailing slash",
			baseURL: "http://127.0.0.1:12434/engines/v1",
			want:    "http://127.0.0.1:12434/engines/_unload",
		},
		{
			name:    "default: Docker Desktop experimental prefix",
			baseURL: "http://_/exp/vDD4.40/engines/v1",
			want:    "http://_/exp/vDD4.40/engines/_unload",
		},
		{
			name:    "default: backend-scoped path",
			baseURL: "http://127.0.0.1:12434/engines/llama.cpp/v1/",
			want:    "http://127.0.0.1:12434/engines/llama.cpp/_unload",
		},

		// Override paths and absolute URLs.
		{
			name:      "override: absolute https URL is returned verbatim",
			baseURL:   "http://anything",
			unloadAPI: "https://api.example.com/unload",
			want:      "https://api.example.com/unload",
		},
		{
			name:      "override: rooted path drops base path",
			baseURL:   "http://localhost:12434/engines/v1",
			unloadAPI: "/engines/_unload",
			want:      "http://localhost:12434/engines/_unload",
		},
		{
			name:      "override: relative path is rooted",
			baseURL:   "http://localhost:12434/engines/v1",
			unloadAPI: "engines/_unload",
			want:      "http://localhost:12434/engines/_unload",
		},

		// Skip / error cases.
		{
			name: "skip: no base_url and no unload_api",
			want: "",
		},
		{
			name:        "error: unload_api set but base_url empty",
			unloadAPI:   "/engines/_unload",
			errContains: "is not absolute",
		},
		{
			name:        "error: base_url without scheme",
			baseURL:     "localhost:12434/engines/v1",
			unloadAPI:   "/engines/_unload",
			errContains: "is not absolute",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnloadURL(tt.baseURL, tt.unloadAPI)
			if tt.errContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
