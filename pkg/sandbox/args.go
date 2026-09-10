package sandbox

import (
	"path/filepath"

	"github.com/docker/docker-agent/pkg/config/sources"
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

	parent := source.ParentDir()
	if parent == "" {
		return ""
	}

	absParent, err := filepath.Abs(parent)
	if err != nil {
		return ""
	}
	absWd, err := filepath.Abs(wd)
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
