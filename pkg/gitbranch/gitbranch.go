// Package gitbranch resolves the current branch of a git working directory by
// reading .git metadata directly, without shelling out to the git binary.
package gitbranch

import (
	"os"
	"path/filepath"
	"strings"
)

// Current returns the current branch name (or the short commit for a detached
// HEAD) for the repository containing dir, walking up parent directories until
// a .git entry is found. It returns "" when dir is empty or not inside a
// repository.
func Current(dir string) string {
	if head := findHead(dir); head != "" {
		return readHead(head)
	}
	return ""
}

func findHead(dir string) string {
	if dir == "" {
		return ""
	}
	d := dir
	for {
		gitPath := filepath.Join(d, ".git")
		info, err := os.Stat(gitPath)
		switch {
		case err == nil && info.IsDir():
			return filepath.Join(gitPath, "HEAD")
		case err == nil:
			// A .git file points at the real git dir (submodule or worktree).
			if gd := parseGitdir(gitPath, d); gd != "" {
				return filepath.Join(gd, "HEAD")
			}
		}

		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func parseGitdir(gitFile, base string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(base, gd)
	}
	return gd
}

func readHead(headPath string) string {
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(line, "ref: "); ok {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if len(line) >= 7 {
		return line[:7] // detached HEAD
	}
	return ""
}
