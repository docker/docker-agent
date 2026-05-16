package paths

import (
	"os"
	"path/filepath"
	"sync/atomic"
)

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

// get returns the override if set, or falls back to the result of defaultFn.
func (o *overridable) get(defaultFn func() string) string {
	if p := o.p.Load(); p != nil {
		return filepath.Clean(*p)
	}
	return defaultFn()
}

var (
	cacheDirOverride  overridable
	configDirOverride overridable
	dataDirOverride   overridable
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

// GetCacheDir returns the user's cache directory for docker agent.
//
// If an override has been set via [SetCacheDir] it is returned instead.
//
// On Linux this follows XDG: $XDG_CACHE_HOME/cagent (default ~/.cache/cagent).
// On macOS this uses ~/Library/Caches/cagent.
// On Windows this uses %LocalAppData%/cagent.
//
// If the cache directory cannot be determined, it falls back to a directory
// under the system temporary directory.
func GetCacheDir() string {
	return cacheDirOverride.get(func() string {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return filepath.Clean(filepath.Join(os.TempDir(), ".cagent-cache"))
		}
		return filepath.Clean(filepath.Join(cacheDir, "cagent"))
	})
}

// GetConfigDir returns the user's config directory for docker agent.
//
// If an override has been set via [SetConfigDir] it is returned instead.
//
// If the home directory cannot be determined, it falls back to a directory
// under the system temporary directory. This is a best-effort fallback and
// not intended to be a security boundary.
func GetConfigDir() string {
	return configDirOverride.get(func() string {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return filepath.Clean(filepath.Join(os.TempDir(), ".cagent-config"))
		}
		return filepath.Clean(filepath.Join(homeDir, ".config", "cagent"))
	})
}

// GetDataDir returns the user's data directory for docker agent (caches, content, logs).
//
// If an override has been set via [SetDataDir] it is returned instead.
//
// If the home directory cannot be determined, it falls back to a directory
// under the system temporary directory.
func GetDataDir() string {
	return dataDirOverride.get(func() string {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return filepath.Clean(filepath.Join(os.TempDir(), ".cagent"))
		}
		return filepath.Clean(filepath.Join(homeDir, ".cagent"))
	})
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
