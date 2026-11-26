package testutil

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCacheVariables pins the resolver input for every platform: these are
// the variables os.UserCacheDir (and therefore pkg/paths.GetCacheDir) reads.
func TestCacheVariables(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	windows := envValue{name: "LocalAppData", value: filepath.Join(home, "AppData", "Local")}
	xdg := envValue{name: "XDG_CACHE_HOME", value: filepath.Join(home, ".cache")}

	tests := []struct {
		goos string
		want []envValue
	}{
		{"windows", []envValue{windows}},
		{"linux", []envValue{xdg}},
		{"freebsd", []envValue{xdg}},
		// The macOS cache directory derives from HOME, which homeVariables
		// already redirects.
		{"darwin", nil},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()

			got := cacheVariables(tt.goos, home)
			assert.Equal(t, tt.want, got)
			for _, variable := range got {
				assert.True(t, filepath.IsAbs(variable.value), "%s must be absolute", variable.name)
				assert.True(t, withinDir(home, variable.value), "%s=%s must stay inside the temporary home", variable.name, variable.value)
			}
		})
	}
}
