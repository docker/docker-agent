package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetRootState clears the process-wide decisions and overrides between
// tests. None of these tests is parallel: they mutate environment and globals.
func resetRootState(t *testing.T) {
	t.Helper()
	reset := func() {
		for _, r := range allRoots {
			r.mode.Store(int32(modeUndecided))
			r.override.Set("")
		}
	}
	reset()
	t.Cleanup(reset)
}

func clearXDG(t *testing.T) {
	t.Helper()
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, "")
	}
}

// setTestHome isolates home and the Windows known folders under a temp dir so
// native defaults stay inside the test's own filesystem.
func setTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	return home
}

func xdgTargets(t *testing.T, base string) []string {
	t.Helper()
	bases := []string{
		filepath.Join(base, "config"),
		filepath.Join(base, "data"),
		filepath.Join(base, "cache"),
	}
	t.Setenv("XDG_CONFIG_HOME", bases[0])
	t.Setenv("XDG_DATA_HOME", bases[1])
	t.Setenv("XDG_CACHE_HOME", bases[2])
	return []string{
		filepath.Join(bases[0], "cagent"),
		filepath.Join(bases[1], "cagent"),
		filepath.Join(bases[2], "cagent"),
	}
}

func writeState(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	p := filepath.Join(dir, "existing-state")
	require.NoError(t, os.WriteFile(p, []byte("do not move me"), 0o600))
	return p
}

// expectedDefaults is the XDG/native layout for a given platform with a known
// home. It is deliberately spelled out rather than derived from the code under
// test so a regression in any branch is caught.
func expectedDefaults(goos, home string) (config, data, cache string) {
	switch goos {
	case "darwin":
		support := filepath.Join(home, "Library", "Application Support")
		return filepath.Join(support, "cagent"),
			filepath.Join(support, "cagent"),
			filepath.Join(home, "Library", "Caches", "cagent")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "cagent"),
			filepath.Join(home, "AppData", "Local", "cagent"),
			filepath.Join(home, "AppData", "Local", "cagent")
	default:
		return filepath.Join(home, ".config", "cagent"),
			filepath.Join(home, ".local", "share", "cagent"),
			filepath.Join(home, ".cache", "cagent")
	}
}

func TestEnsureRootsFreshInstall(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	home := setTestHome(t)

	require.NoError(t, EnsureRoots())

	config, data, cache := expectedDefaults(runtime.GOOS, home)
	for r, dir := range map[*root]string{configRoot: config, dataRoot: data, cacheRoot: cache} {
		assert.DirExists(t, dir)
		assert.FileExists(t, filepath.Join(dir, r.marker()))
	}
	assert.Equal(t, config, GetConfigDir())
	assert.Equal(t, data, GetDataDir())
	assert.Equal(t, cache, GetCacheDir())

	// No legacy directory is created, and nothing is copied into one.
	assert.NoDirExists(t, filepath.Join(home, ".cagent"))

	// Ensuring again is a no-op.
	require.NoError(t, EnsureRoots())
	assert.Equal(t, data, GetDataDir())
}

func TestEnsureRootsKeepsNonEmptyLegacyRoots(t *testing.T) {
	resetRootState(t)
	home := setTestHome(t)
	targets := xdgTargets(t, home)

	legacy := []string{
		filepath.Join(home, ".config", "cagent"),
		filepath.Join(home, ".cagent"),
		nativeCacheDir(),
	}
	var stateFiles []string
	for _, dir := range legacy {
		stateFiles = append(stateFiles, writeState(t, dir))
	}

	require.NoError(t, EnsureRoots())

	getters := []func() string{GetConfigDir, GetDataDir, GetCacheDir}
	for i, get := range getters {
		assert.Equal(t, legacy[i], get(), "root %d should stay on its legacy path", i)
		assert.NoDirExists(t, targets[i], "root %d target must not be created", i)
		assert.NoFileExists(t, filepath.Join(targets[i], allRoots[i].marker()))
		assert.FileExists(t, stateFiles[i], "existing state must not be moved or deleted")
	}
}

func TestMarkerPrecedenceOverLegacy(t *testing.T) {
	resetRootState(t)
	home := setTestHome(t)
	targets := xdgTargets(t, home)

	for i, target := range targets {
		require.NoError(t, createMarker(target, allRoots[i].marker()))
	}
	// An older binary has recreated the legacy directories in the meantime.
	legacyFiles := []string{
		writeState(t, filepath.Join(home, ".config", "cagent")),
		writeState(t, filepath.Join(home, ".cagent")),
		writeState(t, nativeCacheDir()),
	}

	// A marker is honoured even before EnsureRoots runs.
	assert.Equal(t, targets[1], GetDataDir())

	require.NoError(t, EnsureRoots())
	getters := []func() string{GetConfigDir, GetDataDir, GetCacheDir}
	for i, get := range getters {
		assert.Equal(t, targets[i], get(), "root %d must stay on the marked path", i)
		assert.FileExists(t, legacyFiles[i], "legacy state must be left untouched")
	}
}

func TestEmptyLegacyRootDoesNotBlockAdoption(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	home := setTestHome(t)

	legacy := filepath.Join(home, ".cagent")
	require.NoError(t, os.MkdirAll(legacy, 0o700))

	require.NoError(t, EnsureRoots())
	_, data, _ := expectedDefaults(runtime.GOOS, home)
	assert.Equal(t, data, GetDataDir())
	assert.FileExists(t, filepath.Join(data, dataRoot.marker()))
	assert.DirExists(t, legacy, "empty legacy directory is never removed")
}

func TestAbsoluteXDGValuesAreHonoured(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	setTestHome(t)
	targets := xdgTargets(t, t.TempDir())

	assert.Equal(t, targets[0], GetConfigDir())
	assert.Equal(t, targets[1], GetDataDir())
	assert.Equal(t, targets[2], GetCacheDir())

	require.NoError(t, EnsureRoots())
	assert.Equal(t, targets[1], GetDataDir())
	assert.FileExists(t, filepath.Join(targets[1], dataRoot.marker()))
}

func TestRelativeXDGValuesAreIgnored(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	home := setTestHome(t)

	t.Setenv("XDG_CONFIG_HOME", filepath.Join("some", "relative", "config"))
	t.Setenv("XDG_DATA_HOME", "relative-data")
	t.Setenv("XDG_CACHE_HOME", ".")

	config, data, cache := expectedDefaults(runtime.GOOS, home)
	assert.Equal(t, config, GetConfigDir())
	assert.Equal(t, data, GetDataDir())
	assert.Equal(t, cache, GetCacheDir())

	require.NoError(t, EnsureRoots())
	assert.Equal(t, data, GetDataDir())
	assert.FileExists(t, filepath.Join(data, dataRoot.marker()))
	assert.NoDirExists(t, filepath.Join("relative-data", "cagent"))
}

func TestCrossPlatformDefaults(t *testing.T) {
	original := currentOS
	t.Cleanup(func() { currentOS = original })

	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			resetRootState(t)
			clearXDG(t)
			home := setTestHome(t)
			currentOS = goos

			config, data, cache := expectedDefaults(goos, home)
			assert.Equal(t, config, GetConfigDir())
			assert.Equal(t, data, GetDataDir())
			assert.Equal(t, cache, GetCacheDir())

			require.NoError(t, EnsureRoots())
			assert.Equal(t, config, GetConfigDir())
			assert.Equal(t, data, GetDataDir())
			assert.Equal(t, cache, GetCacheDir())
			assert.FileExists(t, filepath.Join(data, dataRoot.marker()))
		})
	}
}

func TestEnsureRootsMarkerFailureFallsBackToLegacy(t *testing.T) {
	resetRootState(t)
	home := setTestHome(t)
	clearXDG(t)

	// A regular file where the target directory should be makes MkdirAll fail.
	base := t.TempDir()
	blocked := filepath.Join(base, "cagent")
	require.NoError(t, os.MkdirAll(base, 0o700))
	require.NoError(t, os.WriteFile(blocked, []byte("blocked"), 0o600))
	t.Setenv("XDG_DATA_HOME", base)

	legacy := writeState(t, filepath.Join(home, ".cagent"))
	require.NoError(t, EnsureRoots())
	assert.Equal(t, filepath.Join(home, ".cagent"), GetDataDir())
	assert.FileExists(t, legacy)
}

func TestEnsureRootsMarkerFailureWithoutLegacy(t *testing.T) {
	resetRootState(t)
	setTestHome(t)
	clearXDG(t)

	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, "cagent"), []byte("blocked"), 0o600))
	t.Setenv("XDG_DATA_HOME", base)

	err := EnsureRoots()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data root marker")
}

func TestEnsureRootsSkipsOverrides(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	setTestHome(t)

	override := t.TempDir()
	SetDataDir(override)
	SetConfigDir(override)
	SetCacheDir(override)

	require.NoError(t, EnsureRoots())
	assert.Equal(t, override, GetDataDir())
	assert.Equal(t, override, GetConfigDir())
	assert.Equal(t, override, GetCacheDir())
	assert.NoFileExists(t, filepath.Join(override, dataRoot.marker()))
}

func TestEnsureRootsSkipsSetRoot(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	setTestHome(t)

	root := t.TempDir()
	SetRoot(root)
	t.Cleanup(func() { SetRoot("") })

	require.NoError(t, EnsureRoots())
	assert.Equal(t, filepath.Join(root, "data"), GetDataDir())
	assert.Equal(t, filepath.Join(root, "config"), GetConfigDir())
	assert.Equal(t, filepath.Join(root, "cache"), GetCacheDir())
	assert.NoFileExists(t, filepath.Join(root, "data", dataRoot.marker()))
}

// Config and data share one directory on macOS: a marker created for one root
// must not make the other adopt it and abandon its legacy state.
func TestSharedTargetDoesNotAdoptOtherRootsLegacyState(t *testing.T) {
	original := currentOS
	t.Cleanup(func() { currentOS = original })

	tests := []struct {
		name         string
		legacyConfig bool
		legacyData   bool
	}{
		{"legacy data only", false, true},
		{"legacy config only", true, false},
		{"legacy config and data", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRootState(t)
			clearXDG(t)
			home := setTestHome(t)
			currentOS = "darwin"

			wantConfig, wantData, _ := expectedDefaults("darwin", home)
			var files []string
			if tt.legacyConfig {
				wantConfig = filepath.Join(home, ".config", "cagent")
				files = append(files, writeState(t, wantConfig))
			}
			if tt.legacyData {
				wantData = filepath.Join(home, ".cagent")
				files = append(files, writeState(t, wantData))
			}

			// Run twice, resetting in-memory decisions in between, to model two
			// separate process launches.
			for range 2 {
				require.NoError(t, EnsureRoots())
				assert.Equal(t, wantConfig, GetConfigDir())
				assert.Equal(t, wantData, GetDataDir())
				for _, r := range allRoots {
					r.mode.Store(int32(modeUndecided))
				}
			}
			assert.Equal(t, wantConfig, GetConfigDir())
			assert.Equal(t, wantData, GetDataDir())
			for _, f := range files {
				assert.FileExists(t, f)
			}
		})
	}
}
