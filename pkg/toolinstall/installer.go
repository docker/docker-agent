package toolinstall

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Install downloads and installs a package at the specified version.
// Returns the path to the installed binary.
func (r *Registry) Install(ctx context.Context, pkg *Package, version string) (string, error) {
	// Apply the aqua version_override matching this version before deciding
	// how to install (asset, format, checksum, and even type can differ
	// between versions).
	pkg = resolveVersionOverride(pkg, version)

	if pkg.IsGoPackage() {
		return installGoPackage(ctx, pkg, version)
	}
	return r.installGitHubRelease(ctx, pkg, version)
}

// installGoPackage installs a Go package using "go install".
func installGoPackage(ctx context.Context, pkg *Package, version string) (string, error) {
	logicalName := pkg.BinaryName()
	if logicalName == "" {
		return "", fmt.Errorf("cannot determine binary name for %s/%s", pkg.RepoOwner, pkg.RepoName)
	}
	binaryName := executableName(logicalName)

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
			goPath = fmt.Sprintf("golang.org/x/%s/%s", pkg.RepoName, logicalName)
		} else {
			goPath = fmt.Sprintf("github.com/%s/%s", pkg.RepoOwner, pkg.RepoName)
		}
	}

	// Strip multi-module tag prefix: "gopls/v0.21.1" → "v0.21.1".
	installVersion := version
	if _, after, ok := strings.CutLast(version, "/"); ok {
		installVersion = after
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
	if pkg.NoAsset {
		return "", fmt.Errorf("%s/%s has no downloadable asset for version %s", pkg.RepoOwner, pkg.RepoName, version)
	}

	binaryName := executableName(pkg.BinaryName())
	if binaryName == "" {
		return "", fmt.Errorf("cannot determine binary name for %s/%s", pkg.RepoOwner, pkg.RepoName)
	}

	pkgDir := PackageDir(pkg.RepoOwner, pkg.RepoName, version)
	binaryPath := filepath.Join(pkgDir, binaryName)

	// Already installed?
	if _, err := os.Stat(binaryPath); err == nil {
		if err := publishBinary(binaryName, binaryPath); err != nil {
			return "", err
		}
		return binaryPath, nil
	}

	pc := resolveForPlatform(pkg, version)

	assetName, err := renderTemplate(pc.Asset, pc.TemplateData)
	if err != nil {
		return "", fmt.Errorf("rendering asset template: %w", err)
	}
	if assetName == "" {
		return "", fmt.Errorf("%s/%s has no asset configured for version %s", pkg.RepoOwner, pkg.RepoName, version)
	}

	downloadURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s",
		pkg.RepoOwner, pkg.RepoName, version, assetName)

	body, err := r.download(ctx, downloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", downloadURL, err)
	}
	defer body.Close()

	// Spool the asset to disk so its checksum can be verified before any of
	// its bytes are extracted and made executable.
	asset, err := r.extractor().spoolToTemp(body)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", downloadURL, err)
	}
	defer func() {
		asset.Close()
		_ = os.Remove(asset.Name())
	}()

	if pc.Checksum.isEnabled() {
		if err := r.verifyAssetChecksum(ctx, pkg, version, assetName, asset.Name(), pc.Checksum, pc.TemplateData); err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(pkgDir, 0o755); err != nil { //nolint:gosec // package dir holds installed tool binaries; needs traversal/exec
		return "", fmt.Errorf("creating package directory: %w", err)
	}

	switch pc.Format {
	case "raw", "":
		// Single-binary download — write the asset directly to binaryPath.
		if err := r.extractor().writeRawBinary(asset, binaryPath); err != nil {
			return "", err
		}
	default:
		if err := r.extractor().extractRelease(asset, pkgDir, pc.Format, pc.Files, pc.TemplateData); err != nil {
			return "", err
		}
	}

	if err := os.Chmod(binaryPath, 0o755); err != nil { //nolint:gosec // installed binary must be executable
		return "", fmt.Errorf("setting executable permissions: %w", err)
	}

	if err := publishBinary(binaryName, binaryPath); err != nil {
		return "", err
	}

	return binaryPath, nil
}

// platformConfig holds all platform-resolved package fields needed for
// downloading, verifying, and extracting a release asset.
type platformConfig struct {
	Format       string
	Asset        string
	Files        []PackageFile
	Checksum     *Checksum
	TemplateData templateData
}

// resolveForPlatform applies platform-specific overrides from pkg.Overrides
// in a single pass and returns a fully merged view for the current OS/arch.
func resolveForPlatform(pkg *Package, version string) platformConfig {
	format := pkg.Format
	asset := pkg.Asset
	files := pkg.Files
	replacements := pkg.Replacements
	replacementsOwned := false
	checksum := pkg.Checksum

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
		if o.Checksum != nil {
			checksum = mergeChecksum(checksum, o.Checksum)
		}
		if len(o.Replacements) > 0 {
			// Copy-on-write so we never mutate the package's shared
			// Replacements map.
			if !replacementsOwned {
				cp := make(map[string]string, len(replacements)+len(o.Replacements))
				maps.Copy(cp, replacements)
				replacements = cp
				replacementsOwned = true
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
		Format:   format,
		Asset:    asset,
		Files:    files,
		Checksum: checksum,
		TemplateData: templateData{
			Version: templateVersion,
			OS:      osName,
			Arch:    archName,
			Format:  format,
		},
	}
}

// spoolToTemp copies an asset stream to a temporary file so it can be
// checksum-verified before extraction. The returned file is positioned at the
// start; the caller must close and remove it. The copy is bounded by
// maxArchiveCompressed to defend against an attacker-controlled release
// streaming an unbounded body.
func (l limits) spoolToTemp(r io.Reader) (*os.File, error) {
	f, err := os.CreateTemp("", "cagent-asset-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}

	cleanup := func() {
		f.Close()
		_ = os.Remove(f.Name())
	}

	n, err := io.Copy(f, io.LimitReader(r, l.maxArchiveCompressed+1))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("buffering asset: %w", err)
	}
	if n > l.maxArchiveCompressed {
		cleanup()
		return nil, errExtractTooLarge
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}

	return f, nil
}
