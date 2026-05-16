package toolinstall

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Install downloads and installs a package at the specified version.
// Returns the path to the installed binary.
func (r *Registry) Install(ctx context.Context, pkg *Package, version string) (string, error) {
	if pkg.IsGoPackage() {
		return installGoPackage(ctx, pkg, version)
	}
	return r.installGitHubRelease(ctx, pkg, version)
}

// installGoPackage installs a Go package using "go install".
func installGoPackage(ctx context.Context, pkg *Package, version string) (string, error) {
	binaryName := pkg.BinaryName()
	if binaryName == "" {
		return "", fmt.Errorf("cannot determine binary name for %s/%s", pkg.RepoOwner, pkg.RepoName)
	}

	binDir := BinDir()
	binaryPath := filepath.Join(binDir, binaryName)

	// Already installed?
	if _, err := os.Stat(binaryPath); err == nil {
		return binaryPath, nil
	}

	// Determine the Go module path.
	goPath := pkg.GoInstallPath
	if goPath == "" {
		if pkg.RepoOwner == "golang" {
			goPath = fmt.Sprintf("golang.org/x/%s/%s", pkg.RepoName, binaryName)
		} else {
			goPath = fmt.Sprintf("github.com/%s/%s", pkg.RepoOwner, pkg.RepoName)
		}
	}

	// Strip multi-module tag prefix: "gopls/v0.21.1" → "v0.21.1".
	installVersion := version
	if idx := strings.LastIndex(version, "/"); idx >= 0 {
		installVersion = version[idx+1:]
	}
	if !strings.HasPrefix(installVersion, "v") && installVersion != "latest" {
		installVersion = "v" + installVersion
	}

	if err := os.MkdirAll(binDir, 0o755); err != nil { //nolint:gosec // bin dir holds installed tool binaries; needs traversal/exec
		return "", fmt.Errorf("creating bin directory: %w", err)
	}

	installArg := goPath + "@" + installVersion
	cmd := exec.CommandContext(ctx, "go", "install", installArg)
	cmd.Env = append(os.Environ(), "GOBIN="+binDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go install %s: %w", installArg, err)
	}

	if _, err := os.Stat(binaryPath); err != nil {
		return "", fmt.Errorf("binary %q not found after go install %s", binaryName, installArg)
	}

	return binaryPath, nil
}

// installGitHubRelease downloads and installs a package from GitHub releases.
func (r *Registry) installGitHubRelease(ctx context.Context, pkg *Package, version string) (string, error) {
	binaryName := pkg.BinaryName()
	if binaryName == "" {
		return "", fmt.Errorf("cannot determine binary name for %s/%s", pkg.RepoOwner, pkg.RepoName)
	}

	pkgDir := PackageDir(pkg.RepoOwner, pkg.RepoName, version)
	binaryPath := filepath.Join(pkgDir, binaryName)

	// Already installed?
	if _, err := os.Stat(binaryPath); err == nil {
		if err := ensureSymlink(binaryName, binaryPath); err != nil {
			return "", err
		}
		return binaryPath, nil
	}

	pc := resolveForPlatform(pkg, version)

	assetName, err := renderTemplate(pc.Asset, pc.TemplateData)
	if err != nil {
		return "", fmt.Errorf("rendering asset template: %w", err)
	}

	downloadURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s",
		pkg.RepoOwner, pkg.RepoName, version, assetName)

	body, err := r.download(ctx, downloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", downloadURL, err)
	}
	defer body.Close()

	if err := os.MkdirAll(pkgDir, 0o755); err != nil { //nolint:gosec // package dir holds installed tool binaries; needs traversal/exec
		return "", fmt.Errorf("creating package directory: %w", err)
	}

	switch pc.Format {
	case "raw", "":
		// Single-binary download — write the body directly to binaryPath.
		if err := writeRawBinary(body, binaryPath); err != nil {
			return "", err
		}
	default:
		if err := extractRelease(body, pkgDir, pc.Format, pc.Files, pc.TemplateData); err != nil {
			return "", err
		}
	}

	if err := os.Chmod(binaryPath, 0o755); err != nil { //nolint:gosec // installed binary must be executable
		return "", fmt.Errorf("setting executable permissions: %w", err)
	}

	if err := ensureSymlink(binaryName, binaryPath); err != nil {
		return "", err
	}

	return binaryPath, nil
}

// platformConfig holds all platform-resolved package fields needed for
// downloading and extracting a release asset.
type platformConfig struct {
	Format       string
	Asset        string
	Files        []PackageFile
	TemplateData templateData
}

// resolveForPlatform applies platform-specific overrides from pkg.Overrides
// in a single pass and returns a fully merged view for the current OS/arch.
func resolveForPlatform(pkg *Package, version string) platformConfig {
	format := pkg.Format
	asset := pkg.Asset
	files := pkg.Files
	replacements := pkg.Replacements

	for _, o := range pkg.Overrides {
		if o.GOOS != "" && o.GOOS != runtime.GOOS {
			continue
		}
		if o.GOArch != "" && o.GOArch != runtime.GOARCH {
			continue
		}
		if o.Format != "" {
			format = o.Format
		}
		if o.Asset != "" {
			asset = o.Asset
		}
		if len(o.Files) > 0 {
			files = o.Files
		}
		if len(o.Replacements) > 0 {
			if replacements == nil {
				replacements = make(map[string]string)
			}
			maps.Copy(replacements, o.Replacements)
		}
	}

	osName := runtime.GOOS
	archName := runtime.GOARCH
	if r, ok := replacements[runtime.GOOS]; ok {
		osName = r
	}
	if r, ok := replacements[runtime.GOARCH]; ok {
		archName = r
	}

	templateVersion := version
	if pkg.VersionPrefix == "" {
		templateVersion = strings.TrimPrefix(version, "v")
	}

	return platformConfig{
		Format: format,
		Asset:  asset,
		Files:  files,
		TemplateData: templateData{
			Version: templateVersion,
			OS:      osName,
			Arch:    archName,
			Format:  format,
		},
	}
}

// ensureSymlink atomically creates a symlink in BinDir() pointing to the binary.
// It uses a temporary symlink + rename to avoid TOCTOU races when multiple
// goroutines create symlinks concurrently.
func ensureSymlink(name, target string) error {
	binDir := BinDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil { //nolint:gosec // bin dir holds installed tool binaries; needs traversal/exec
		return fmt.Errorf("creating bin directory: %w", err)
	}

	link := filepath.Join(binDir, name)

	// Create a temporary symlink in the same directory, then atomically
	// rename it over the target. This avoids the Remove+Symlink TOCTOU race.
	tmpLink := link + ".tmp." + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmpLink) // clean up any stale temp from a previous crash

	if err := os.Symlink(target, tmpLink); err != nil {
		return fmt.Errorf("creating temp symlink %s -> %s: %w", tmpLink, target, err)
	}

	if err := os.Rename(tmpLink, link); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("renaming symlink %s -> %s: %w", tmpLink, link, err)
	}

	return nil
}
