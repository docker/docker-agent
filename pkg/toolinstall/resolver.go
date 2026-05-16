package toolinstall

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"golang.org/x/sync/singleflight"
)

// EnsureCommand makes sure a command binary is available.
// It checks PATH first, then the docker agent tools directory, then
// attempts to install from the aqua registry if auto-install is enabled.
//
// Returns the resolved command (may be the same string if found in PATH,
// or a full path to the installed binary) and any error encountered.
// When auto-install is disabled (globally or per-toolset), the original
// command is returned with no error.
func EnsureCommand(ctx context.Context, command, version string) (string, error) {
	if strings.EqualFold(os.Getenv("DOCKER_AGENT_AUTO_INSTALL"), "false") {
		return command, nil
	}

	lower := strings.ToLower(strings.TrimSpace(version))
	if lower == "false" || lower == "off" {
		return command, nil
	}

	resolvedPath, err := resolve(ctx, command, version)
	if err != nil {
		return "", fmt.Errorf("auto-installing command %q: %w", command, err)
	}

	return resolvedPath, nil
}

// installGroup deduplicates concurrent installations of the same command.
// If two goroutines call resolve("fzf") simultaneously, only one performs
// the actual download and install; the other waits and receives the same result.
var installGroup singleflight.Group

// resolve checks if a command is available and installs it if needed.
// Returns the path to the usable binary.
func resolve(ctx context.Context, command, version string) (string, error) {
	// Check system PATH first — return original command name (not full path)
	// so the caller uses it as-is via exec.Command.
	if _, err := exec.LookPath(command); err == nil {
		return command, nil
	}

	// Check if already installed in our bin dir.
	binPath := filepath.Join(BinDir(), command)
	if info, err := os.Stat(binPath); err == nil && info.Mode()&0o111 != 0 {
		return binPath, nil
	}

	// Use singleflight to deduplicate concurrent installs of the same command.
	result, err, _ := installGroup.Do(command, func() (any, error) {
		return safeInstall(ctx, command, version)
	})
	if err != nil {
		return "", err
	}

	return result.(string), nil
}

// safeInstall wraps doInstall with panic recovery. Without this,
// singleflight wraps any panic in *panicError and re-raises it via
// `go panic(...)` (see golang.org/x/sync/singleflight), which is
// unrecoverable by callers and crashes the process. Issue #2765.
func safeInstall(ctx context.Context, command, version string) (path string, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "Panic during tool auto-install",
				"command", command, "panic", r, "stack", string(debug.Stack()))
			path = ""
			err = fmt.Errorf("auto-install for %q panicked: %v", command, r)
		}
	}()
	return doInstallFn(ctx, command, version)
}

// doInstallFn is the install function safeInstall delegates to. Indirected
// through a var so tests can swap in a panicking implementation to verify
// the recover path. Production code never reassigns this.
var doInstallFn = doInstall

// doInstall performs the actual package resolution and installation.
func doInstall(ctx context.Context, command, versionRef string) (string, error) {
	// Re-check bin dir under singleflight — another goroutine may have
	// just finished installing while we were waiting.
	binPath := filepath.Join(BinDir(), command)
	if info, err := os.Stat(binPath); err == nil && info.Mode()&0o111 != 0 {
		return binPath, nil
	}

	slog.InfoContext(ctx, "Auto-installing missing command via aqua registry", "command", command)

	registry := SharedRegistry()

	pkg, version, err := lookupPackage(ctx, registry, command, versionRef)
	if err != nil {
		return "", err
	}

	if version == "" {
		version, err = resolveVersion(ctx, registry, pkg)
		if err != nil {
			return "", fmt.Errorf("resolving latest version for %s/%s: %w", pkg.RepoOwner, pkg.RepoName, err)
		}
	}

	pkgName := pkg.RepoOwner + "/" + pkg.RepoName
	slog.InfoContext(ctx, "Installing tool", "command", command, "package", pkgName, "version", version)

	binaryPath, err := registry.Install(ctx, pkg, version)
	if err != nil {
		return "", fmt.Errorf("installing %s@%s: %w", pkgName, version, err)
	}

	slog.InfoContext(ctx, "Successfully installed command",
		"command", command, "package", fmt.Sprintf("%s@%s", pkgName, version), "path", binaryPath)

	return binaryPath, nil
}

// lookupPackage resolves the aqua package for a command.
// If versionRef is provided (e.g. "owner/repo@v1.0"), it parses the reference
// and looks up by name. Otherwise, it searches by command name.
// Returns the package, the explicit version (if any), and any error.
func lookupPackage(ctx context.Context, registry *Registry, command, versionRef string) (*Package, string, error) {
	if versionRef == "" {
		pkg, err := registry.LookupByCommand(ctx, command)
		if err != nil {
			return nil, "", fmt.Errorf("looking up command %q in aqua registry: %w", command, err)
		}
		return pkg, "", nil
	}

	owner, repo, version, err := parseAquaRef(versionRef)
	if err != nil {
		return nil, "", fmt.Errorf("parsing aqua reference: %w", err)
	}

	pkg, err := registry.LookupByName(ctx, owner+"/"+repo)
	if err != nil {
		return nil, "", fmt.Errorf("looking up aqua package %s/%s: %w", owner, repo, err)
	}

	return pkg, version, nil
}

// resolveVersion determines the latest version for a package.
func resolveVersion(ctx context.Context, registry *Registry, pkg *Package) (string, error) {
	// Check for a version_filter with a "startsWith" prefix (multi-module repos).
	if prefix := extractVersionPrefix(pkg.VersionFilter); prefix != "" {
		return registry.latestVersionFiltered(ctx, pkg.RepoOwner, pkg.RepoName, prefix)
	}

	// For go types, use "latest" and let go install resolve it.
	if pkg.IsGoPackage() {
		return "latest", nil
	}

	return registry.latestVersion(ctx, pkg.RepoOwner, pkg.RepoName)
}

// extractVersionPrefix parses an aqua version_filter expression like
// 'Version startsWith "gopls/"' and returns the prefix string.
// Returns "" if the filter doesn't match this pattern.
func extractVersionPrefix(filter string) string {
	filter = strings.TrimSpace(filter)
	const marker = "startsWith"
	_, after, ok := strings.Cut(filter, marker)
	if !ok {
		return ""
	}

	rest := strings.TrimSpace(after)
	if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') {
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end >= 0 {
			return rest[1 : end+1]
		}
	}

	return ""
}

// parseAquaRef parses an aqua reference string into owner, repo, and version.
// Format: "owner/repo" or "owner/repo@version"
func parseAquaRef(ref string) (owner, repo, version string, err error) {
	ref = strings.TrimSpace(ref)

	atParts := strings.SplitN(ref, "@", 2)
	namePart := atParts[0]
	if len(atParts) == 2 {
		version = atParts[1]
	}

	parts := strings.SplitN(namePart, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("invalid aqua reference %q: expected owner/repo[@version] format", ref)
	}

	return parts[0], parts[1], version, nil
}
