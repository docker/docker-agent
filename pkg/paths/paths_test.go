package paths_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		set    func(string)
		get    func() string
		custom string
	}{
		{"CacheDir", paths.SetCacheDir, paths.GetCacheDir, "/custom/cache"},
		{"ConfigDir", paths.SetConfigDir, paths.GetConfigDir, "/custom/config"},
		{"DataDir", paths.SetDataDir, paths.GetDataDir, "/custom/data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Restore default after the test.
			t.Cleanup(func() { tt.set("") })

			original := tt.get()
			assert.NotEmpty(t, original)

			tt.set(tt.custom)
			assert.Equal(t, filepath.Clean(tt.custom), tt.get())

			// Empty string restores the default.
			tt.set("")
			assert.Equal(t, filepath.Clean(original), tt.get())
		})
	}
}

func TestGetHomeDir(t *testing.T) {
	t.Parallel()

	assert.NotEmpty(t, paths.GetHomeDir())
}

func TestSetRoot(t *testing.T) {
	t.Cleanup(func() { paths.SetRoot("") })

	defaultData := paths.GetDataDir()
	defaultConfig := paths.GetConfigDir()
	defaultCache := paths.GetCacheDir()

	paths.SetRoot("/custom/root")
	assert.Equal(t, filepath.Clean("/custom/root/data"), paths.GetDataDir())
	assert.Equal(t, filepath.Clean("/custom/root/config"), paths.GetConfigDir())
	assert.Equal(t, filepath.Clean("/custom/root/cache"), paths.GetCacheDir())

	// Empty root restores the defaults.
	paths.SetRoot("")
	assert.Equal(t, defaultData, paths.GetDataDir())
	assert.Equal(t, defaultConfig, paths.GetConfigDir())
	assert.Equal(t, defaultCache, paths.GetCacheDir())
}
