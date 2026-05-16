package skills

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/paths"
)

// Skill represents a loaded skill with its metadata and content location.
type Skill struct {
	Name          string
	Description   string
	FilePath      string
	BaseDir       string
	Files         []string
	Local         bool // true for filesystem-loaded skills, false for remote
	License       string
	Compatibility string
	Metadata      map[string]string
	AllowedTools  []string
	Context       string // "fork" to run the skill as an isolated sub-agent
	// Model is an optional model override applied while the skill runs as
	// a sub-agent (context: fork). It accepts either a named model from the
	// agent config or an inline "provider/model" reference (e.g.
	// "openai/gpt-4o-mini"). It is ignored for non-fork skills.
	Model string
}

// IsFork returns true when the skill should be executed in an isolated
// sub-agent context rather than inline in the current conversation.
// This matches Claude Code's `context: fork` frontmatter syntax.
func (s Skill) IsFork() bool {
	return s.Context == "fork"
}

// Load discovers and loads skills from the given sources.
// Each source is either "local" (for filesystem-based skills) or an HTTP/HTTPS
// URL (for remote skills per the well-known skills discovery spec).
//
// Local skills are loaded from (in order, later overrides earlier):
//
// Global locations (under $HOME):
//   - ~/.codex/skills/ (recursive)
//   - ~/.claude/skills/ (flat)
//   - ~/.agents/skills/ (recursive)
//
// Project locations (under cwd, closest wins):
//   - .claude/skills/ (flat, only at cwd)
//   - .agents/skills/ (flat, scanned from git root to cwd)
//
// The returned slice is sorted by skill name for deterministic ordering.
func Load(sources []string) []Skill {
	skillMap := make(map[string]Skill)

	var remoteCache *diskCache
	for _, source := range sources {
		switch {
		case source == "local":
			loadLocalSkillsInto(skillMap)
		case isHTTPSource(source):
			if remoteCache == nil {
				remoteCache = newDiskCache(filepath.Join(paths.GetCacheDir(), "skills"))
			}
			for _, skill := range loadRemoteSkills(context.Background(), source, remoteCache) {
				skillMap[source+"/"+skill.Name] = skill
			}
		}
	}

	return slices.SortedFunc(maps.Values(skillMap), func(a, b Skill) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		// FilePath is unique per skill, so this gives a fully
		// deterministic ordering even when a local and a remote source
		// expose a skill with the same name.
		return strings.Compare(a.FilePath, b.FilePath)
	})
}

// isHTTPSource reports whether s is an HTTP(S) URL source.
func isHTTPSource(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
