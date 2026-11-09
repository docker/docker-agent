package path

import (
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/paths"
)

// RelativeTo returns p relative to baseDir when both paths are absolute and
// filepath.Rel can compute a relative path. Otherwise it returns p cleaned.
func RelativeTo(p, baseDir string) string {
	if p == "" {
		return p
	}

	p = filepath.Clean(p)
	if baseDir == "" || !filepath.IsAbs(p) || !filepath.IsAbs(baseDir) {
		return p
	}

	rel, err := filepath.Rel(filepath.Clean(baseDir), p)
	if err != nil {
		return p
	}
	return rel
}

// ShortenHome replaces the current user's home directory prefix with "~".
func ShortenHome(p string) string {
	return ShortenHomeDir(p, paths.GetHomeDir())
}

// ShortenHomeDir replaces a leading homeDir prefix with "~".
func ShortenHomeDir(p, homeDir string) string {
	if p == "" || homeDir == "" {
		return p
	}

	p = filepath.Clean(p)
	homeDir = filepath.Clean(homeDir)
	if !IsWithin(p, homeDir) {
		return p
	}

	rel, err := filepath.Rel(homeDir, p)
	if err != nil {
		return p
	}
	if rel == "." {
		return "~"
	}
	return filepath.Join("~", rel)
}

// IsWithin reports whether p is equal to dir or contained by dir.
func IsWithin(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}

	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
