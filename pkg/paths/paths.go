package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
)

// markerName records that a root lives at its XDG/native location. It is
// never written into a legacy root, so its presence is a durable "adopted"
// decision shared with older binaries that still know only the legacy paths.
const markerName = ".cagent-root"

// currentOS is a test seam: defaults are OS-native, but the tests run on one
// host and still exercise every platform branch.
var currentOS = runtime.GOOS

// overridable holds an optional directory override backed by an atomic pointer.
// A nil pointer (the zero value) means "use the default".
type overridable struct{ p atomic.Pointer[string] }

// Set stores an override directory. An empty value clears the override.
func (o *overridable) Set(dir string) {
	if dir == "" {
		o.p.Store(nil)
	} else {
		o.p.Store(&dir)
	}
}

func (o *overridable) isSet() bool { return o.p.Load() != nil }

// get returns the override if set, or falls back to the result of defaultFn.
func (o *overridable) get(defaultFn func() string) string {
	if p := o.p.Load(); p != nil {
		return filepath.Clean(*p)
	}
	return defaultFn()
}

type rootMode int32

const (
	modeUndecided rootMode = iota // EnsureRoots has not made a decision yet
	modeMarked                    // the XDG/native root is marked as adopted
	modeLegacy                    // keep using the legacy root (marker could not be created)
)

type root struct {
	name     string
	override *overridable
	mode     *atomic.Int32
	target   func() string
	legacy   func() string
}

var (
	cacheDirOverride  overridable
	configDirOverride overridable
	dataDirOverride   overridable

	cacheMode  atomic.Int32
	configMode atomic.Int32
	dataMode   atomic.Int32
)

var (
	configRoot = &root{"config", &configDirOverride, &configMode, xdgConfigDir, legacyConfigDir}
	dataRoot   = &root{"data", &dataDirOverride, &dataMode, xdgDataDir, legacyDataDir}
	cacheRoot  = &root{"cache", &cacheDirOverride, &cacheMode, xdgCacheDir, legacyCacheDir}
	allRoots   = []*root{configRoot, dataRoot, cacheRoot}
)

// SetCacheDir overrides the default cache directory returned by [GetCacheDir].
// An empty value restores the default behaviour.
// This should be called early (e.g. during CLI flag processing) before any
// goroutine calls the corresponding getter.
func SetCacheDir(dir string) { cacheDirOverride.Set(dir) }

// SetConfigDir overrides the default config directory returned by [GetConfigDir].
// An empty value restores the default behaviour.
func SetConfigDir(dir string) { configDirOverride.Set(dir) }

// SetDataDir overrides the default data directory returned by [GetDataDir].
// An empty value restores the default behaviour.
func SetDataDir(dir string) { dataDirOverride.Set(dir) }

// SetRoot re-homes all docker-agent state under one directory: data,
// config, and cache land in the "data", "config", and "cache"
// subdirectories of root. It is the one-call override for embedders that
// must keep their embedded agent's state isolated from a docker-agent
// installation on the same machine (e.g. the Gordon assistant in Docker
// Sandboxes homes everything under ~/.sbx/gordon). An empty root restores
// the per-directory defaults.
func SetRoot(root string) {
	if root == "" {
		SetDataDir("")
		SetConfigDir("")
		SetCacheDir("")
		return
	}
	SetDataDir(filepath.Join(root, "data"))
	SetConfigDir(filepath.Join(root, "config"))
	SetCacheDir(filepath.Join(root, "cache"))
}

// EnsureRoots atomically establishes the root markers before any state is
// written. It must be called once, after applying overrides and before any
// state-writing entrypoint: logging, sessions, config, caches, migrations.
//
// For each non-overridden root:
//   - a root that is already marked keeps its XDG/native location;
//   - a non-empty legacy root (~/.cagent, ~/.config/cagent) stays in place,
//     unmarked, so existing installations are never relocated;
//   - otherwise the marker is created in the XDG/native root, adopting it.
//
// If a marker cannot be created, an existing legacy directory is used instead.
// When no legacy directory exists, an error is returned so callers fail before
// writing unmarked state. Markers are additive: no file is ever moved, copied,
// merged, or deleted.
//
// A marker cannot stop an older bundled binary from writing to the legacy
// paths; that version-skew split is a documented limitation, not something
// path resolution can prevent.
func EnsureRoots() error {
	var errs []error
	for _, r := range allRoots {
		if r.override.isSet() || r.mode.Load() != int32(modeUndecided) {
			continue
		}

		target := filepath.Clean(r.target())
		legacy := r.legacy()

		if markerExists(target) {
			r.mode.Store(int32(modeMarked))
			continue
		}
		if legacy != "" && dirHasEntries(legacy) {
			r.mode.Store(int32(modeLegacy))
			continue
		}
		if err := createMarker(target); err != nil {
			if legacy != "" && dirExists(legacy) {
				r.mode.Store(int32(modeLegacy))
				continue
			}
			errs = append(errs, fmt.Errorf("creating %s root marker in %s: %w", r.name, target, err))
			continue
		}
		r.mode.Store(int32(modeMarked))
	}
	return errors.Join(errs...)
}

// GetCacheDir returns the user's cache directory for docker agent.
//
// An override set via [SetCacheDir] wins. Otherwise $XDG_CACHE_HOME (when
// absolute) is honoured on every platform, falling back to the OS-native
// cache directory: ~/.cache/cagent (Linux), ~/Library/Caches/cagent (macOS),
// or %LocalAppData%\cagent (Windows). See [EnsureRoots].
func GetCacheDir() string {
	return cacheDirOverride.get(func() string { return resolve(cacheRoot) })
}

// GetConfigDir returns the user's config directory for docker agent
// (config.yaml, aliases, user id, MCP OAuth tokens, sandbox tokens).
//
// An override set via [SetConfigDir] wins. Otherwise $XDG_CONFIG_HOME (when
// absolute) is honoured on every platform, falling back to the OS-native
// config directory: ~/.config/cagent (Linux),
// ~/Library/Application Support/cagent (macOS), or %AppData%\cagent (Windows).
// An existing non-empty ~/.config/cagent stays in use until explicitly moved
// by the user. See [EnsureRoots].
func GetConfigDir() string {
	return configDirOverride.get(func() string { return resolve(configRoot) })
}

// GetDataDir returns the user's data directory for docker agent (sessions,
// history, installed tools, OCI store, memory, worktrees, snapshots, ...).
//
// An override set via [SetDataDir] wins. Otherwise $XDG_DATA_HOME (when
// absolute) is honoured on every platform, falling back to the OS-native data
// directory: ~/.local/share/cagent (Linux),
// ~/Library/Application Support/cagent (macOS), or %LocalAppData%\cagent
// (Windows). An existing non-empty ~/.cagent stays in use until explicitly
// moved by the user. See [EnsureRoots].
func GetDataDir() string {
	return dataDirOverride.get(func() string { return resolve(dataRoot) })
}

// GetHomeDir returns the user's home directory.
//
// Returns an empty string if the home directory cannot be determined.
func GetHomeDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Clean(homeDir)
}

// resolve applies the root precedence: explicit override (handled by the
// caller) -> marked root -> existing non-empty legacy root -> XDG/native root.
func resolve(r *root) string {
	target := filepath.Clean(r.target())
	legacy := r.legacy()

	switch r.mode.Load() {
	case int32(modeMarked):
		return target
	case int32(modeLegacy):
		if legacy != "" {
			return filepath.Clean(legacy)
		}
	}

	// Honour a marker created by an earlier run even before EnsureRoots is
	// called: an old binary recreating the legacy directory must not make a
	// marked installation switch roots.
	if markerExists(target) {
		return target
	}
	if legacy != "" && legacy != target && dirHasEntries(legacy) {
		return filepath.Clean(legacy)
	}
	return target
}

func createMarker(target string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	marker := filepath.Join(target, markerName)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Another process won the race; the marker is established either way.
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	writeErr := f.Close()
	if writeErr == nil {
		return nil
	}
	// Only the file this call created is removed; a retry can then recreate it.
	_ = os.Remove(marker)
	return writeErr
}

func markerExists(target string) bool {
	info, err := os.Lstat(filepath.Join(target, markerName))
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func dirHasEntries(p string) bool {
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) > 0
}

// --- XDG / OS-native default resolution ---

// absEnv returns the XDG-style base directory only when it is absolute, as
// required by the XDG Base Directory Specification. Relative values are
// ignored so state never lands in the process working directory.
func absEnv(name string) string {
	if name == "" {
		return ""
	}
	dir := os.Getenv(name)
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return dir
}

func xdgConfigDir() string {
	if dir := absEnv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "cagent")
	}
	switch currentOS {
	case "darwin":
		return nativeDir(".cagent-config", "Library", "Application Support", "cagent")
	case "windows":
		return windowsNative("APPDATA", ".cagent-config", "AppData", "Roaming", "cagent")
	default:
		return nativeDir(".cagent-config", ".config", "cagent")
	}
}

func xdgDataDir() string {
	if dir := absEnv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "cagent")
	}
	switch currentOS {
	case "darwin":
		return nativeDir(".cagent", "Library", "Application Support", "cagent")
	case "windows":
		return windowsNative("LOCALAPPDATA", ".cagent", "AppData", "Local", "cagent")
	default:
		return nativeDir(".cagent", ".local", "share", "cagent")
	}
}

func xdgCacheDir() string {
	if dir := absEnv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "cagent")
	}
	return nativeCacheDir()
}

func nativeCacheDir() string {
	switch currentOS {
	case "darwin":
		return nativeDir(".cagent-cache", "Library", "Caches", "cagent")
	case "windows":
		return windowsNative("LOCALAPPDATA", ".cagent-cache", "AppData", "Local", "cagent")
	default:
		return nativeDir(".cagent-cache", ".cache", "cagent")
	}
}

func windowsNative(envName, tempFallback string, fallbackParts ...string) string {
	if dir := absEnv(envName); dir != "" {
		return filepath.Join(dir, "cagent")
	}
	return nativeDir(tempFallback, fallbackParts...)
}

func nativeDir(tempFallback string, parts ...string) string {
	home := GetHomeDir()
	if home == "" {
		return filepath.Join(os.TempDir(), tempFallback)
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func legacyConfigDir() string {
	home := GetHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "cagent")
}

func legacyDataDir() string {
	home := GetHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".cagent")
}

func legacyCacheDir() string {
	return nativeCacheDir()
}
