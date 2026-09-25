package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/paths"
)

// ExtraWorkspace returns the directory to mount as a read-only extra
// workspace when the agent file lives outside the main workspace.
//
// The agent reference may be a path, an OCI/URL reference, a built-in
// name, or an alias defined in the user's config — ExtraWorkspace
// delegates resolution to [sources.Resolve] so all of those forms are
// handled the same way runtime code handles them. Only [Source]s that
// expose a containing directory (i.e. local file sources) produce a
// mount; OCI / URL / built-in / bytes sources return "" because there
// is no host file to bind-mount.
//
// Returns "" when no extra mount is needed (the agent file is already
// under wd), the reference cannot be resolved, or the resolved source
// has no on-disk parent directory.
func ExtraWorkspace(wd, agentRef string) string {
	if agentRef == "" {
		return ""
	}

	source, err := sources.Resolve(agentRef, nil)
	if err != nil {
		return ""
	}

	return ExtraWorkspaceForSource(wd, source)
}

// ExtraWorkspaceForSource uses the frozen selection rather than reloading aliases.
func ExtraWorkspaceForSource(wd string, source config.Source) string {
	parent := source.ParentDir()
	if parent == "" {
		return ""
	}

	absParent, err := CanonicalPath(parent)
	if err != nil {
		return ""
	}
	absWd, err := CanonicalPath(wd)
	if err != nil {
		return ""
	}

	// No extra mount needed if the file is already under the workspace.
	rel, err := filepath.Rel(absWd, absParent)
	if err == nil && rel != ".." && !startsWithParent(rel) {
		return ""
	}

	return absParent
}

// startsWithParent reports whether rel begins with a "../" segment,
// which means absParent is not a subdirectory of absWd.
func startsWithParent(rel string) bool {
	const dotdot = ".." + string(filepath.Separator)
	return len(rel) >= len(dotdot) && rel[:len(dotdot)] == dotdot
}

// GuestAgentRef pins built-ins to a file so the guest cannot resolve their names
// through a different alias in the mounted user configuration.
func GuestAgentRef(ctx context.Context, source config.Source) (ref, extra string, err error) {
	if source.ParentDir() != "" {
		ref, err = CanonicalFilePath(source.Name())
		return ref, "", err
	}
	if !slices.Contains(sources.BuiltinAgentNames(), source.Name()) {
		return source.Name(), "", nil
	}
	data, err := source.Read(ctx)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(paths.GetCacheDir(), "sandbox-agents", source.Name())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("staging builtin agent: %w", err)
	}
	dir, err = CanonicalPath(dir)
	if err != nil {
		return "", "", err
	}
	ref = filepath.Join(dir, "agent.yaml")
	if err := atomicfile.Write(ref, bytes.NewReader(data), 0o600); err != nil {
		return "", "", fmt.Errorf("staging builtin agent: %w", err)
	}
	return ref, dir, nil
}
