package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
)

// remoteIndex represents the index.json served at /.well-known/skills/index.json
type remoteIndex struct {
	Skills []remoteSkillEntry `json:"skills"`
}

type remoteSkillEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

// loadRemoteSkills fetches skills from a remote URL per the well-known skills
// discovery spec. It fetches /.well-known/skills/index.json, then prefetches
// all listed files into the given disk cache so the agent can read them
// without network requests during task execution.
func loadRemoteSkills(ctx context.Context, baseURL string, cache *diskCache) []Skill {
	baseURL = strings.TrimRight(baseURL, "/")
	indexURL := baseURL + "/.well-known/skills/index.json"

	slog.DebugContext(ctx, "Fetching remote skills index", "url", indexURL)

	resp, err := httpGet(ctx, indexURL)
	if err != nil {
		slog.WarnContext(ctx, "Failed to fetch remote skills index", "url", indexURL, "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "Remote skills index returned non-OK status", "url", indexURL, "status", resp.StatusCode)
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		slog.WarnContext(ctx, "Failed to read remote skills index", "url", indexURL, "error", err)
		return nil
	}

	var index remoteIndex
	if err := json.Unmarshal(body, &index); err != nil {
		slog.WarnContext(ctx, "Failed to parse remote skills index", "url", indexURL, "error", err)
		return nil
	}

	var skills []Skill
	for _, entry := range index.Skills {
		if entry.Name == "" || entry.Description == "" {
			continue
		}
		if !isValidSkillName(entry.Name) {
			slog.WarnContext(ctx, "Skipping remote skill with invalid name", "url", baseURL, "name", entry.Name)
			continue
		}

		cacheDir := cache.cacheDir(baseURL, entry.Name)
		prefetchFiles(ctx, cache, baseURL, entry.Name, entry.Files)

		skill := Skill{
			Name:        entry.Name,
			Description: entry.Description,
			FilePath:    filepath.Join(cacheDir, "SKILL.md"),
			BaseDir:     cacheDir,
			Files:       entry.Files,
		}
		skills = append(skills, skill)
	}

	slog.DebugContext(ctx, "Loaded remote skills", "url", baseURL, "count", len(skills))
	return skills
}

// prefetchFiles downloads all files listed in the index for a skill,
// storing them in the disk cache. Files already in cache (and not expired)
// are skipped.
func prefetchFiles(ctx context.Context, cache *diskCache, baseURL, skillName string, files []string) {
	for _, file := range files {
		if !isValidFilePath(file) {
			slog.DebugContext(ctx, "Skipping invalid file path in skill", "skill", skillName, "file", file)
			continue
		}

		if _, ok := cache.Get(baseURL, skillName, file); ok {
			continue
		}

		fileURL := fmt.Sprintf("%s/.well-known/skills/%s/%s", baseURL, skillName, file)
		if _, err := cache.FetchAndStore(ctx, baseURL, skillName, file, fileURL); err != nil {
			slog.WarnContext(ctx, "Failed to prefetch skill file", "skill", skillName, "file", file, "error", err)
		}
	}
}

// isValidFilePath checks a relative file path from the index for safety.
// Rejects absolute paths, parent traversals, and characters that are not
// safe in both filesystem paths and URL path segments.
func isValidFilePath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return false
	}
	for _, c := range path {
		if c < 0x20 || c > 0x7E {
			return false
		}
		if strings.ContainsRune(`\?#[]`, c) {
			return false
		}
	}
	return true
}

// skillNameRe matches a safe single path component: ASCII letters, digits,
// '.', '-' or '_', not starting with '.', 1-128 chars.
//
// Skill names from a remote index are used both as a filesystem path
// component (filepath.Join(cacheBase, urlHash, skillName)) and as a URL path
// segment, so anything fancier than this conservative set is unsafe.
var skillNameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

func isValidSkillName(name string) bool {
	return skillNameRe.MatchString(name)
}
