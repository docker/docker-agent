package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/testutil"
)

// None of these tests is parallel: they mutate the environment and globals.

func resetRootState(t *testing.T) {
	t.Helper()
	reset := func() {
		ResetRootsForTesting()
		SetRoot("")
	}
	reset()
	t.Cleanup(reset)
}

// forgetDecisions models a new process launch on the same filesystem.
func forgetDecisions() { ResetRootsForTesting() }

func clearXDG(t *testing.T) {
	t.Helper()
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, "")
	}
}

// setTestHome isolates home and the Windows known folders under a temp dir,
// on every host, so the currentOS seam can exercise each platform.
func setTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	return home
}

func useOS(t *testing.T, goos string) {
	t.Helper()
	original := currentOS
	currentOS = goos
	t.Cleanup(func() { currentOS = original })
}

// freshInstall is a clean home with no XDG variables, on the given platform.
func freshInstall(t *testing.T, goos string) string {
	t.Helper()
	resetRootState(t)
	clearXDG(t)
	useOS(t, goos)
	return setTestHome(t)
}

func writeState(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	p := filepath.Join(dir, "existing-state")
	require.NoError(t, os.WriteFile(p, []byte("do not move me"), 0o600))
	return p
}

// ensureRoots runs EnsureRoots expecting nothing worth logging.
func ensureRoots(t *testing.T) []string {
	t.Helper()
	warnings, err := EnsureRoots()
	require.NoError(t, err)
	return warnings
}

func skipWithoutPermissionChecks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permission checks as a non-root user")
	}
}

func TestDefaultsPerPlatform(t *testing.T) {
	// Only a root with a distinct legacy location has a choice to record.
	marked := map[string][]*root{
		"linux":   {dataRoot},
		"darwin":  {configRoot, dataRoot},
		"windows": {configRoot, dataRoot},
	}
	for goos, wantMarked := range marked {
		t.Run(goos, func(t *testing.T) {
			home := freshInstall(t, goos)
			config, data, cache := testutil.DefaultDirs(goos, home)
			dirs := map[*root]string{configRoot: config, dataRoot: data, cacheRoot: cache}

			assert.Equal(t, config, GetConfigDir())
			assert.Equal(t, data, GetDataDir())
			assert.Equal(t, cache, GetCacheDir())

			// Ensuring twice is a no-op.
			for range 2 {
				assert.Empty(t, ensureRoots(t))
				assert.Equal(t, config, GetConfigDir())
				assert.Equal(t, data, GetDataDir())
				assert.Equal(t, cache, GetCacheDir())
			}

			for _, r := range allRoots {
				marker := filepath.Join(dirs[r], r.marker())
				if slices.Contains(wantMarked, r) {
					assert.FileExists(t, marker)
				} else {
					assert.NoFileExists(t, marker)
				}
			}
			assert.NoDirExists(t, filepath.Join(home, ".cagent"))
		})
	}
}

func TestLegacyRootsWithStateStayInPlace(t *testing.T) {
	home := freshInstall(t, runtime.GOOS)
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))

	legacyConfig := filepath.Join(home, ".config", "cagent")
	legacyData := filepath.Join(home, ".cagent")
	files := []string{writeState(t, legacyConfig), writeState(t, legacyData)}

	assert.Empty(t, ensureRoots(t))

	assert.Equal(t, legacyConfig, GetConfigDir())
	assert.Equal(t, legacyData, GetDataDir())
	assert.NoDirExists(t, filepath.Join(base, "config", "cagent"), "target must not be created")
	assert.NoDirExists(t, filepath.Join(base, "data", "cagent"), "target must not be created")
	for _, f := range files {
		assert.FileExists(t, f, "existing state must not be moved or deleted")
	}
	assert.NoFileExists(t, filepath.Join(legacyData, dataRoot.marker()))
}

// What counts as state in a legacy root: anything but a marker, on purpose.
func TestLegacyStateRule(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, legacy string)
		wantLegacy bool
		symlink    bool
	}{
		{name: "missing", setup: func(*testing.T, string) {}},
		{name: "empty directory", setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(legacy, 0o700))
		}},
		{name: "only a root marker", setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, createMarker(legacy, cacheRoot.marker()))
		}},
		{name: "regular file", setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, os.WriteFile(legacy, []byte("not a directory"), 0o600))
		}},
		{name: "dangling symlink", symlink: true, setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), legacy))
		}},
		{name: "debug log only", wantLegacy: true, setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(legacy, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(legacy, "cagent.debug.log"), nil, 0o600))
		}},
		{name: ".DS_Store only", wantLegacy: true, setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(legacy, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(legacy, ".DS_Store"), nil, 0o600))
		}},
		{name: "empty subdirectory", wantLegacy: true, setup: func(t *testing.T, legacy string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(legacy, "sessions"), 0o700))
		}},
		{name: "symlink to a directory with state", wantLegacy: true, symlink: true, setup: func(t *testing.T, legacy string) {
			t.Helper()
			elsewhere := t.TempDir()
			writeState(t, elsewhere)
			require.NoError(t, os.Symlink(elsewhere, legacy))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.symlink && runtime.GOOS == "windows" {
				t.Skip("creating symlinks needs a privilege on Windows")
			}
			home := freshInstall(t, runtime.GOOS)
			legacy := filepath.Join(home, ".cagent")
			tt.setup(t, legacy)
			_, target, _ := testutil.DefaultDirs(runtime.GOOS, home)

			want := target
			if tt.wantLegacy {
				want = legacy
			}
			assert.Equal(t, want, GetDataDir(), "before EnsureRoots")
			assert.Empty(t, ensureRoots(t))
			assert.Equal(t, want, GetDataDir())
			assert.Equal(t, !tt.wantLegacy, markerExists(target, dataRoot.marker()))
		})
	}
}

// An unreadable legacy root is not an empty one: adopting the new root would
// hide the user's data behind a marker for good.
func TestUnreadableLegacyRootIsKept(t *testing.T) {
	skipWithoutPermissionChecks(t)
	home := freshInstall(t, runtime.GOOS)
	legacy := filepath.Join(home, ".cagent")
	writeState(t, legacy)
	require.NoError(t, os.Chmod(legacy, 0))
	t.Cleanup(func() { _ = os.Chmod(legacy, 0o700) })
	_, target, _ := testutil.DefaultDirs(runtime.GOOS, home)

	assert.Equal(t, legacy, GetDataDir(), "before EnsureRoots")

	warnings, err := EnsureRoots()
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "cannot read "+legacy)
	assert.Equal(t, legacy, GetDataDir())
	assert.NoDirExists(t, target, "no marker may pin the new root")
}

func TestMarkerWinsOverRecreatedLegacyRoot(t *testing.T) {
	home := freshInstall(t, runtime.GOOS)
	_, target, _ := testutil.DefaultDirs(runtime.GOOS, home)
	assert.Empty(t, ensureRoots(t))
	require.FileExists(t, filepath.Join(target, dataRoot.marker()))

	// An older binary recreates the legacy root after adoption.
	legacyFile := writeState(t, filepath.Join(home, ".cagent"))
	forgetDecisions()
	assert.Equal(t, target, GetDataDir(), "a marker is honoured before EnsureRoots runs")

	warnings, err := EnsureRoots()
	assert.Empty(t, warnings)
	require.ErrorContains(t, err, "also holds state and is ignored")
	assert.Equal(t, target, GetDataDir())
	assert.FileExists(t, legacyFile, "legacy state must be left untouched")
}

func TestAbsoluteXDGValuesAreHonoured(t *testing.T) {
	freshInstall(t, runtime.GOOS)
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))

	for range 2 {
		assert.Equal(t, filepath.Join(base, "config", "cagent"), GetConfigDir())
		assert.Equal(t, filepath.Join(base, "data", "cagent"), GetDataDir())
		assert.Equal(t, filepath.Join(base, "cache", "cagent"), GetCacheDir())
		assert.Empty(t, ensureRoots(t))
	}
	assert.FileExists(t, filepath.Join(base, "data", "cagent", dataRoot.marker()))
}

func TestRelativeXDGValuesAreIgnored(t *testing.T) {
	home := freshInstall(t, runtime.GOOS)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join("some", "relative", "config"))
	t.Setenv("XDG_DATA_HOME", "relative-data")
	t.Setenv("XDG_CACHE_HOME", ".")

	config, data, cache := testutil.DefaultDirs(runtime.GOOS, home)
	for range 2 {
		assert.Equal(t, config, GetConfigDir())
		assert.Equal(t, data, GetDataDir())
		assert.Equal(t, cache, GetCacheDir())
		assert.Empty(t, ensureRoots(t))
	}
	assert.NoDirExists(t, filepath.Join("relative-data", "cagent"))
}

// Earlier versions resolved the cache through os.UserCacheDir, which honours
// XDG_CACHE_HOME everywhere but macOS and Windows.
func TestExistingCacheIsNotRelocated(t *testing.T) {
	t.Run("linux keeps a cache under XDG_CACHE_HOME", func(t *testing.T) {
		home := freshInstall(t, "linux")
		base := t.TempDir()
		t.Setenv("XDG_CACHE_HOME", base)
		existing := filepath.Join(base, "cagent")
		writeState(t, existing)
		writeState(t, filepath.Join(home, ".cache", "cagent"))

		assert.Empty(t, ensureRoots(t))
		assert.Equal(t, existing, GetCacheDir())
	})

	t.Run("darwin keeps the native cache when XDG_CACHE_HOME appears", func(t *testing.T) {
		home := freshInstall(t, "darwin")
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		existing := filepath.Join(home, "Library", "Caches", "cagent")
		writeState(t, existing)

		assert.Empty(t, ensureRoots(t))
		assert.Equal(t, existing, GetCacheDir())
	})
}

// A root whose legacy location is the target itself (Linux config without
// XDG_CONFIG_HOME) has no choice to record, so nothing is written into it.
func TestTargetEqualToLegacyWritesNoMarker(t *testing.T) {
	home := freshInstall(t, "linux")
	config := filepath.Join(home, ".config", "cagent")
	file := writeState(t, config)

	assert.Empty(t, ensureRoots(t))
	assert.Equal(t, config, GetConfigDir())

	entries, err := os.ReadDir(config)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the pre-existing file")
	assert.Equal(t, filepath.Base(file), entries[0].Name())
}

// Pointing XDG_DATA_HOME elsewhere after adoption selects a fresh root; the
// user is told once that the adopted one is left behind.
func TestXDGChangedAfterAdoptionWarnsOnce(t *testing.T) {
	home := freshInstall(t, runtime.GOOS)
	_, adopted, _ := testutil.DefaultDirs(runtime.GOOS, home)
	assert.Empty(t, ensureRoots(t))
	session := writeState(t, adopted)

	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", base)
	moved := filepath.Join(base, "cagent")

	forgetDecisions()
	warnings := ensureRoots(t)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], adopted)
	assert.Contains(t, warnings[0], moved)
	assert.Equal(t, moved, GetDataDir())
	assert.FileExists(t, session, "the adopted root is left untouched")

	forgetDecisions()
	assert.Empty(t, ensureRoots(t), "the new root is marked, so the warning does not repeat")
	assert.Equal(t, moved, GetDataDir())
}

// Config and data share one directory on macOS: a marker created for one root
// must not make the other adopt it and abandon its legacy state.
func TestSharedTargetDoesNotAdoptOtherRootsLegacyState(t *testing.T) {
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
			home := freshInstall(t, "darwin")
			wantConfig, wantData, _ := testutil.DefaultDirs("darwin", home)
			var files []string
			if tt.legacyConfig {
				wantConfig = filepath.Join(home, ".config", "cagent")
				files = append(files, writeState(t, wantConfig))
			}
			if tt.legacyData {
				wantData = filepath.Join(home, ".cagent")
				files = append(files, writeState(t, wantData))
			}

			for range 2 {
				assert.Empty(t, ensureRoots(t))
				assert.Equal(t, wantConfig, GetConfigDir())
				assert.Equal(t, wantData, GetDataDir())
				forgetDecisions()
			}
			assert.Equal(t, wantConfig, GetConfigDir())
			assert.Equal(t, wantData, GetDataDir())
			for _, f := range files {
				assert.FileExists(t, f)
			}
		})
	}
}

// A marker that cannot be written must not stop the command: the root is used
// unmarked and the failure is only reported for logging.
func TestMarkerFailureUsesTargetUnmarked(t *testing.T) {
	t.Run("directory squatting on the marker path", func(t *testing.T) {
		home := freshInstall(t, runtime.GOOS)
		_, target, _ := testutil.DefaultDirs(runtime.GOOS, home)
		require.NoError(t, os.MkdirAll(filepath.Join(target, dataRoot.marker()), 0o700))
		// An empty legacy directory is no reason to fall back to it.
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".cagent"), 0o700))

		require.Error(t, createMarker(target, dataRoot.marker()))

		warnings, err := EnsureRoots()
		assert.Empty(t, warnings)
		require.ErrorContains(t, err, "data directory: using "+target+" unmarked")
		assert.Equal(t, target, GetDataDir())
		assert.False(t, markerExists(target, dataRoot.marker()))
	})

	t.Run("read-only home on a fresh install", func(t *testing.T) {
		skipWithoutPermissionChecks(t)
		home := freshInstall(t, runtime.GOOS)
		config, data, cache := testutil.DefaultDirs(runtime.GOOS, home)
		require.NoError(t, os.Chmod(home, 0o500))
		t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

		warnings, err := EnsureRoots()
		assert.Empty(t, warnings)
		require.ErrorContains(t, err, "unmarked")
		assert.Equal(t, config, GetConfigDir())
		assert.Equal(t, data, GetDataDir())
		assert.Equal(t, cache, GetCacheDir())

		entries, readErr := os.ReadDir(home)
		require.NoError(t, readErr)
		assert.Empty(t, entries)
	})
}

// Without a home directory, state falls back to a temp dir shared between
// users: no marker is written there and nothing fails.
func TestUnknownHomeUsesTempDirWithoutMarkers(t *testing.T) {
	resetRootState(t)
	clearXDG(t)
	tmp := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(name, "")
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tmp)
	}
	require.Empty(t, GetHomeDir())

	assert.Empty(t, ensureRoots(t))
	assert.Equal(t, filepath.Join(os.TempDir(), ".cagent-config"), GetConfigDir())
	assert.Equal(t, filepath.Join(os.TempDir(), ".cagent"), GetDataDir())
	assert.Equal(t, filepath.Join(os.TempDir(), ".cagent-cache"), GetCacheDir())

	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created in the shared temp dir")
}

func TestCreateMarkerIsRaceSafe(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cagent")
	const workers = 16

	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			<-start
			errs[i] = createMarker(target, dataRoot.marker())
		})
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.True(t, markerExists(target, dataRoot.marker()))
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestEnsureRootsSkipsOverrides(t *testing.T) {
	freshInstall(t, runtime.GOOS)
	override := t.TempDir()
	SetDataDir(override)
	SetConfigDir(override)
	SetCacheDir(override)

	assert.Empty(t, ensureRoots(t))
	assert.Equal(t, override, GetDataDir())
	assert.Equal(t, override, GetConfigDir())
	assert.Equal(t, override, GetCacheDir())
	assert.NoFileExists(t, filepath.Join(override, dataRoot.marker()))
}

func TestEnsureRootsSkipsSetRoot(t *testing.T) {
	freshInstall(t, runtime.GOOS)
	root := t.TempDir()
	SetRoot(root)
	t.Cleanup(func() { SetRoot("") })

	assert.Empty(t, ensureRoots(t))
	assert.Equal(t, filepath.Join(root, "data"), GetDataDir())
	assert.Equal(t, filepath.Join(root, "config"), GetConfigDir())
	assert.Equal(t, filepath.Join(root, "cache"), GetCacheDir())
	assert.NoFileExists(t, filepath.Join(root, "data", dataRoot.marker()))
}
