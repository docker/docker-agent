package skills

import (
	"cmp"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
)

const skillFile = "SKILL.md"

// KitDirEnv names the environment variable that points at a docker-agent kit
// directory staged by the host before launching a sandbox. When set, local
// skill discovery is rooted exclusively at the kit's skills directory; the
// usual $HOME / git-walking lookups are skipped because they target paths
// that don't exist inside the sandbox.
const KitDirEnv = "DOCKER_AGENT_KIT_DIR"

// KitSkillsSubdir is the path inside a kit that holds the staged skills.
const KitSkillsSubdir = "skills"

// localSearchPath describes one directory to scan for local skills, and
// whether subdirectories should be walked recursively (Codex/agents format)
// or only as immediate children of the search root (Claude format).
type localSearchPath struct {
	dir       string
	recursive bool
}

// localSearchPathsFromDir returns every directory to scan for local skills, in
// load order. Entries appearing later in the list override skills with the
// same name from earlier entries.
//
// When DOCKER_AGENT_KIT_DIR is set (i.e. running inside a sandbox where the
// host has staged a kit) only the kit's skills directory is scanned, since
// the usual host paths don't exist inside the sandbox.
//
// Otherwise the search paths are:
//
//  1. Global directories (under $HOME).
//  2. .claude/skills under cwd (Claude project format, flat).
//  3. .github/skills (GitHub Copilot format) and .agents/skills in each
//     ancestor of cwd up to $HOME — or the enclosing git root when cwd is
//     outside $HOME — down to cwd (closest wins; .agents/skills overrides
//     .github/skills at the same level).
func localSearchPathsFromDir(cwd string) []localSearchPath {
	if kit := os.Getenv(KitDirEnv); kit != "" {
		return []localSearchPath{{filepath.Join(kit, KitSkillsSubdir), true}}
	}

	var searchPaths []localSearchPath

	if home := paths.GetHomeDir(); home != "" {
		searchPaths = append(searchPaths, homeSkillSearchPaths(home)...)
	}

	if cwd == "" {
		return searchPaths
	}

	searchPaths = append(searchPaths, localSearchPath{filepath.Join(cwd, ".claude", "skills"), false})
	for _, dir := range projectSearchDirs(cwd) {
		searchPaths = append(searchPaths,
			localSearchPath{filepath.Join(dir, ".github", "skills"), false},
			localSearchPath{filepath.Join(dir, ".agents", "skills"), false},
		)
	}
	return searchPaths
}

// loadLocalSkillsFromDirInto populates skillMap with every local skill, with later
// search paths overriding earlier ones.
func loadLocalSkillsFromDirInto(skillMap map[string]Skill, dir string) {
	for _, p := range localSearchPathsFromDir(dir) {
		for _, skill := range loadSkillsFromDir(p.dir, p.recursive) {
			skillMap[skill.Name] = skill
		}
	}
}

// IsHomeSkillPath reports whether path is under one of the global skill
// directories in the user's home directory.
func IsHomeSkillPath(path string) bool {
	home := paths.GetHomeDir()
	if home == "" {
		return false
	}

	for _, p := range homeSkillSearchPaths(home) {
		if pathx.IsWithin(path, p.dir) {
			return true
		}
	}
	return false
}

func homeSkillSearchPaths(home string) []localSearchPath {
	return []localSearchPath{
		{filepath.Join(home, ".codex", "skills"), true},
		{filepath.Join(home, ".claude", "skills"), false},
		{filepath.Join(home, ".agents", "skills"), true},
	}
}

// projectSearchDirs returns the directories to scan for repo-local skill
// roots (.github/skills, .agents/skills), ordered from the topmost ancestor
// down to cwd (inclusive).
// Ordering matters: later (deeper) entries override skills from parents, so a
// project subdirectory can shadow a skill defined higher up.
//
// When cwd is inside $HOME the walk climbs all the way up to (but not
// including) $HOME. This makes a .agents/skills placed in a non-git "grouping"
// directory — e.g. ~/work/org holding several independent repos — reachable
// from inside any sub-repo, instead of being hidden by the sub-repo's own
// .git. $HOME itself is excluded because ~/.agents/skills is already scanned
// as a global path.
//
// When cwd is outside $HOME (or is $HOME itself) the walk is anchored at the
// enclosing git root, so discovery never climbs into system directories.
func projectSearchDirs(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return []string{cwd}
	}

	// Walk up to $HOME only when cwd is inside it (and not $HOME itself, which
	// is already covered by the global ~/.agents/skills scan).
	if home := paths.GetHomeDir(); home != "" && abs != home && pathx.IsWithin(abs, home) {
		var dirs []string
		for current := abs; current != home; {
			dirs = append(dirs, current)
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
		slices.Reverse(dirs)
		return dirs
	}

	return gitAnchoredDirs(abs)
}

// gitAnchoredDirs returns the directories from the enclosing git root down to
// abs (inclusive), ordered root-to-abs, or just abs when it is not inside a git
// repository.
func gitAnchoredDirs(abs string) []string {
	var dirs []string
	for current := abs; ; {
		dirs = append(dirs, current)
		if hasGitMarker(current) {
			slices.Reverse(dirs)
			return dirs
		}
		parent := filepath.Dir(current)
		if parent == current {
			return []string{abs}
		}
		current = parent
	}
}

// hasGitMarker reports whether dir contains a .git directory or .git file
// (the latter is used by worktrees and submodules).
func hasGitMarker(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular()
}

// findGitRoot finds the enclosing git repository root, or returns "" if dir
// is not inside a git repository. Kept as a thin wrapper for test coverage
// and callers that don't need the full directory list.
func findGitRoot(dir string) string {
	for current := dir; ; {
		if hasGitMarker(current) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// loadSkillsFromDir loads skills from a directory.
// If recursive is true, it walks all subdirectories looking for SKILL.md.
// If recursive is false, it only looks at immediate subdirectories.
func loadSkillsFromDir(dir string, recursive bool) []Skill {
	if recursive {
		return loadSkillsRecursive(dir)
	}
	return loadSkillsFlat(dir)
}

// loadSkillsFlat loads skills from immediate subdirectories only (Claude
// format). Symlinks are explicitly skipped here because flat-mode skills are
// expected to live as plain directories under the search root.
func loadSkillsFlat(dir string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() || isHidden(entry) || isSymlink(entry) {
			continue
		}
		path := filepath.Join(dir, entry.Name(), skillFile)
		if skill, ok := loadSkillFile(path, entry.Name()); ok {
			skills = append(skills, skill)
		}
	}
	return skills
}

// loadSkillsRecursive walks dir for SKILL.md files (Codex format).
//
// filepath.WalkDir does not follow symlinks inside the tree (it uses
// fs.DirEntry which is Lstat-based), so a symlink-to-directory has
// IsDir() == false and is naturally not entered. The visited map is
// defensive: it catches the (rare) cases where the same real directory
// is reachable via two non-symlink paths — e.g. Linux bind mounts, or
// when the search root itself is a symlink whose target also appears
// as a sibling deeper in the tree.
func loadSkillsRecursive(dir string) []Skill {
	visited := make(map[string]bool)
	if realDir, err := filepath.EvalSymlinks(dir); err == nil {
		visited[realDir] = true
	}

	var skills []Skill
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if path == dir {
				return nil
			}
			if isHidden(d) {
				return fs.SkipDir
			}
			// De-duplicate by real path so symlink cycles are skipped.
			realPath, err := filepath.EvalSymlinks(path)
			if err == nil {
				if visited[realPath] {
					return fs.SkipDir
				}
				visited[realPath] = true
			}
			return nil
		}

		if d.Name() != skillFile {
			return nil
		}
		dirName := filepath.Base(filepath.Dir(path))
		if skill, ok := loadSkillFile(path, dirName); ok {
			skills = append(skills, skill)
		}
		return nil
	})
	return skills
}

// loadSkillFile reads and parses a SKILL.md file. dirName is used as the
// skill name when the frontmatter does not declare one.
func loadSkillFile(path, dirName string) (Skill, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}

	skill, ok := parseFrontmatter(string(content))
	if !ok {
		return Skill{}, false
	}

	skill.Name = cmp.Or(skill.Name, dirName)
	if skill.Name == "" || skill.Description == "" {
		return Skill{}, false
	}

	skill.FilePath = path
	skill.BaseDir = filepath.Dir(path)
	skill.Local = true
	return skill, true
}

func isHidden(entry fs.DirEntry) bool {
	name := entry.Name()
	return name != "" && name[0] == '.'
}

func isSymlink(entry fs.DirEntry) bool {
	return entry.Type()&os.ModeSymlink != 0
}
