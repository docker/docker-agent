package sandbox

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Options selects the sandbox launch source. Kits are resolved and validated by
// sbx, including v3 composition, permissions, arguments, and build caching.
type Options struct {
	Cloud    bool
	Template string
	Workload string
	Kits     []string
	KitArgs  []string
	TTL      time.Duration
}

func (o Options) agent() string {
	if o.Workload != "" {
		return o.Workload
	}
	return "docker-agent"
}

func (o Options) kitFlags() []string {
	var args []string
	for _, ref := range o.Kits {
		args = append(args, "--kit", csvValue(ref))
	}
	for _, arg := range o.KitArgs {
		args = append(args, "--kit-arg", arg)
	}
	return args
}

func csvValue(value string) string {
	var encoded strings.Builder
	writer := csv.NewWriter(&encoded)
	_ = writer.Write([]string{value})
	writer.Flush()
	return strings.TrimSuffix(encoded.String(), "\n")
}

func launchName(wd string, extras []string, loginKit string, opts Options) (string, error) {
	mounts := slices.Clone(extras)
	slices.Sort(mounts)
	h := sha256.New()
	if err := json.NewEncoder(h).Encode([]any{wd, mounts, opts}); err != nil {
		return "", err
	}
	if loginKit != "" {
		filename := "spec.yaml"
		if opts.Workload != "" {
			filename = "gateway.yaml"
		}
		if err := hashKitFile(h, filepath.Join(loginKit, filename)); err != nil {
			return "", err
		}
	}
	for _, ref := range append([]string{opts.Workload}, opts.Kits...) {
		if ref == "" {
			continue
		}
		info, err := os.Stat(ref)
		if os.IsNotExist(err) && !filepath.IsAbs(ref) && !strings.HasPrefix(ref, ".") {
			continue // Registry or git reference; pin a digest for reproducibility.
		}
		if err != nil {
			return "", fmt.Errorf("reading sandbox kit %q: %w", ref, err)
		}
		if !info.IsDir() {
			if err := hashKitFile(h, ref); err != nil {
				return "", err
			}
			continue
		}
		if err := filepath.WalkDir(ref, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Name() == ".git" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			return hashKitFile(h, path)
		}); err != nil {
			return "", fmt.Errorf("hashing sandbox kit %q: %w", ref, err)
		}
	}
	return fmt.Sprintf("docker-agent-%x", h.Sum(nil)[:12]), nil
}

func hashKitFile(h io.Writer, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		// Do not follow a kit's links into arbitrary host directories.
		return fmt.Errorf("sandbox kit contains symlink %q; use a published kit instead", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("sandbox kit contains non-regular file %q", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	content := sha256.New()
	if _, err := io.Copy(content, f); err != nil {
		return err
	}
	return json.NewEncoder(h).Encode(struct {
		Path   string
		Mode   fs.FileMode
		Digest []byte
	}{path, info.Mode(), content.Sum(nil)})
}
