// Package paths resolves the config, data and cache directories.
//
// Each root is, in order: an explicit override, an adopted XDG/native root, a
// legacy root (~/.cagent, ~/.config/cagent) that still holds state, else the
// XDG/native root. [EnsureRoots] fixes that choice for the process; callers
// that never run it get path-only resolution, which re-reads the disk on each
// call and can change answer mid-process.
package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

// markerPrefix names the per-root marker file (".cagent-root-data") that
// records an adopted XDG/native root. Markers are per root because roots can
// share a directory (config and data on macOS, data and cache on Windows).
const markerPrefix = ".cagent-root-"

// currentOS is a test seam for the per-platform defaults.
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
	modeUndecided rootMode = iota // EnsureRoots has not run
	modeTarget                    // use the XDG/native root
	modeLegacy                    // keep the legacy root
)

type root struct {
	name     string
	override *overridable
	mode     *atomic.Int32
	target   func() string // XDG value, else the OS-native default
	native   func() string // OS-native default, ignoring XDG
	legacy   func() string // where earlier versions kept this root
}

func (r *root) marker() string { return markerPrefix + r.name }

var (
	cacheDirOverride  overridable
	configDirOverride overridable
	dataDirOverride   overridable

	cacheMode  atomic.Int32
	configMode atomic.Int32
	dataMode   atomic.Int32
)

var (
	configRoot = &root{"config", &configDirOverride, &configMode, xdgConfigDir, nativeConfigDir, legacyConfigDir}
	dataRoot   = &root{"data", &dataDirOverride, &dataMode, xdgDataDir, nativeDataDir, legacyDataDir}
	cacheRoot  = &root{"cache", &cacheDirOverride, &cacheMode, xdgCacheDir, nativeCacheDir, legacyCacheDir}
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

// EnsureRoots decides once, for each root without an override, whether it
// lives at its XDG/native location or stays on a legacy one, and marks roots
// it adopts. Call it after applying overrides and before writing state.
//
// Resolution never fails. warnings are meant for the user: a decision that
// leaves existing state unused or unreadable. err only reports conditions
// worth logging. No file is ever moved, copied, merged, or deleted.
func EnsureRoots() (warnings []string, err error) {
	var errs []error
	for _, r := range allRoots {
		if r.override.isSet() || r.mode.Load() != int32(modeUndecided) {
			continue
		}
		mode, warning, err := r.decide()
		r.mode.Store(int32(mode))
		if warning != "" {
			warnings = append(warnings, warning)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s directory: %w", r.name, err))
		}
	}
	return warnings, errors.Join(errs...)
}

// ResetRootsForTesting forgets the decisions made by [EnsureRoots].
func ResetRootsForTesting() {
	for _, r := range allRoots {
		r.mode.Store(int32(modeUndecided))
	}
}

func (r *root) decide() (mode rootMode, warning string, err error) {
	target := filepath.Clean(r.target())
	legacy := r.distinctLegacy(target)

	if markerExists(target, r.marker()) {
		// An older binary may have recreated the legacy root since adoption.
		if state, _ := hasState(legacy); state {
			err = fmt.Errorf("using %s; %s also holds state and is ignored", target, legacy)
		}
		return modeTarget, "", err
	}

	state, err := hasState(legacy)
	if err != nil {
		// Unreadable is not empty: adopting now would hide that state for good.
		return modeLegacy, fmt.Sprintf("cannot read %s (%v); keeping it as the %s directory instead of adopting %s", legacy, err, r.name, target), nil
	}
	if state {
		return modeLegacy, "", nil
	}

	// A root whose legacy location is the target itself has no choice to
	// record, and the temp-dir fallback is shared between users.
	legacyIsTarget := legacy == "" && r.legacy() != ""
	if legacyIsTarget || isTempFallback(target) {
		return modeTarget, "", nil
	}

	if native := filepath.Clean(r.native()); native != target {
		if state, _ := hasState(native); state || markerExists(native, r.marker()) {
			warning = fmt.Sprintf("%s holds existing %s state that is no longer used: the %s directory now resolves to %s", native, r.name, r.name, target)
		}
	}
	// Without a marker the root still works; it is just not pinned against an
	// older binary recreating the legacy one.
	if markErr := createMarker(target, r.marker()); markErr != nil {
		return modeTarget, warning, fmt.Errorf("using %s unmarked: %w", target, markErr)
	}
	return modeTarget, warning, nil
}

// distinctLegacy returns the legacy root when it is a different directory
// than target, or "" when there is none to choose between.
func (r *root) distinctLegacy(target string) string {
	legacy := r.legacy()
	if legacy == "" {
		return ""
	}
	if legacy = filepath.Clean(legacy); legacy == target {
		return ""
	}
	return legacy
}

// GetCacheDir returns the cache directory: the [SetCacheDir] override, else
// $XDG_CACHE_HOME/cagent when absolute, else the OS-native cache directory.
func GetCacheDir() string {
	return cacheDirOverride.get(func() string { return resolve(cacheRoot) })
}

// GetConfigDir returns the config directory (config.yaml, aliases, user id,
// MCP OAuth tokens, sandbox tokens): the [SetConfigDir] override, else an
// existing ~/.config/cagent, else $XDG_CONFIG_HOME/cagent when absolute, else
// the OS-native config directory. On Windows that is the roaming %AppData%.
func GetConfigDir() string {
	return configDirOverride.get(func() string { return resolve(configRoot) })
}

// GetDataDir returns the data directory (sessions, history, installed tools,
// OCI store, memory, worktrees, snapshots, ...): the [SetDataDir] override,
// else an existing ~/.cagent, else $XDG_DATA_HOME/cagent when absolute, else
// the OS-native data directory.
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

// resolve applies the root precedence: marked root, then a legacy root
// holding state, then the XDG/native root. Before EnsureRoots has run it reads
// the disk on every call.
func resolve(r *root) string {
	target := filepath.Clean(r.target())
	legacy := r.distinctLegacy(target)

	switch r.mode.Load() {
	case int32(modeTarget):
		return target
	case int32(modeLegacy):
		if legacy != "" {
			return legacy
		}
		return target
	}

	if markerExists(target, r.marker()) {
		return target
	}
	if state, err := hasState(legacy); state || err != nil {
		return legacy
	}
	return target
}

func createMarker(target, name string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	marker := filepath.Join(target, name)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Another process won the race; a directory at the path is not a marker.
		if os.IsExist(err) && markerExists(target, name) {
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

func markerExists(target, name string) bool {
	info, err := os.Lstat(filepath.Join(target, name))
	return err == nil && !info.IsDir()
}

// hasState reports whether p is a directory holding anything beyond root
// markers. Any other entry counts, even a stray log file: an existing install
// is never abandoned on a guess. A missing path or a regular file is empty;
// an unreadable directory is an error, never "empty".
func hasState(p string) (bool, error) {
	if p == "" {
		return false, nil
	}
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), markerPrefix) {
			return true, nil
		}
	}
	return false, nil
}

// isTempFallback reports whether target is the shared temp-dir location used
// when the home directory is unknown.
func isTempFallback(target string) bool {
	if GetHomeDir() != "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(os.TempDir()), target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

func xdgConfigDir() string { return xdgOr("XDG_CONFIG_HOME", nativeConfigDir) }
func xdgDataDir() string   { return xdgOr("XDG_DATA_HOME", nativeDataDir) }
func xdgCacheDir() string  { return xdgOr("XDG_CACHE_HOME", nativeCacheDir) }

func xdgOr(env string, native func() string) string {
	if dir := absEnv(env); dir != "" {
		return filepath.Join(dir, "cagent")
	}
	return native()
}

func nativeConfigDir() string {
	switch currentOS {
	case "darwin":
		return nativeDir(".cagent-config", "Library", "Application Support", "cagent")
	case "windows":
		return windowsNative("APPDATA", ".cagent-config", "AppData", "Roaming", "cagent")
	default:
		return nativeDir(".cagent-config", ".config", "cagent")
	}
}

func nativeDataDir() string {
	switch currentOS {
	case "darwin":
		return nativeDir(".cagent", "Library", "Application Support", "cagent")
	case "windows":
		return windowsNative("LOCALAPPDATA", ".cagent", "AppData", "Local", "cagent")
	default:
		return nativeDir(".cagent", ".local", "share", "cagent")
	}
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

// legacyCacheDir mirrors os.UserCacheDir, which earlier versions used: it
// honoured XDG_CACHE_HOME everywhere but macOS and Windows.
func legacyCacheDir() string {
	if currentOS != "darwin" && currentOS != "windows" {
		return xdgCacheDir()
	}
	return nativeCacheDir()
}
