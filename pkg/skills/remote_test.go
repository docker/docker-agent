package skills

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRemoteSkills(t *testing.T) {
	t.Run("valid index with skills and prefetch", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/.well-known/skills/index.json":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{
					"skills": [
						{
							"name": "docker-build",
							"description": "Build Docker images",
							"files": ["SKILL.md", "references/COMMANDS.md"]
						},
						{
							"name": "k8s-deploy",
							"description": "Deploy to Kubernetes",
							"files": ["SKILL.md"]
						}
					]
				}`)
			case "/.well-known/skills/docker-build/SKILL.md":
				fmt.Fprint(w, "# Docker Build")
			case "/.well-known/skills/docker-build/references/COMMANDS.md":
				fmt.Fprint(w, "# Docker Commands Reference")
			case "/.well-known/skills/k8s-deploy/SKILL.md":
				fmt.Fprint(w, "# K8s Deploy")
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		cacheDir := t.TempDir()
		cache := newDiskCache(cacheDir)
		skills := loadRemoteSkills(t.Context(), srv.URL, cache)

		require.Len(t, skills, 2)

		assert.Equal(t, "docker-build", skills[0].Name)
		assert.Equal(t, "Build Docker images", skills[0].Description)
		assert.Equal(t, []string{"SKILL.md", "references/COMMANDS.md"}, skills[0].Files)

		// Verify SKILL.md was prefetched to disk
		skillMD, err := os.ReadFile(skills[0].FilePath)
		require.NoError(t, err)
		assert.Equal(t, "# Docker Build", string(skillMD))

		// Verify reference file was prefetched
		refFile := filepath.Join(skills[0].BaseDir, "references", "COMMANDS.md")
		refContent, err := os.ReadFile(refFile)
		require.NoError(t, err)
		assert.Equal(t, "# Docker Commands Reference", string(refContent))

		assert.Equal(t, "k8s-deploy", skills[1].Name)
	})

	t.Run("trailing slash on base URL", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/.well-known/skills/index.json":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"skills": [{"name": "test", "description": "Test skill", "files": ["SKILL.md"]}]}`)
			case "/.well-known/skills/test/SKILL.md":
				fmt.Fprint(w, "# Test")
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		cache := newDiskCache(t.TempDir())
		skills := loadRemoteSkills(t.Context(), srv.URL+"/", cache)
		require.Len(t, skills, 1)

		content, err := os.ReadFile(skills[0].FilePath)
		require.NoError(t, err)
		assert.Equal(t, "# Test", string(content))
	})

	t.Run("empty skills array", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"skills": []}`)
		}))
		defer srv.Close()

		skills := loadRemoteSkills(t.Context(), srv.URL, newDiskCache(t.TempDir()))
		assert.Empty(t, skills)
	})

	t.Run("skips entries with missing name", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"skills": [{"name": "", "description": "No name", "files": ["SKILL.md"]}]}`)
		}))
		defer srv.Close()

		skills := loadRemoteSkills(t.Context(), srv.URL, newDiskCache(t.TempDir()))
		assert.Empty(t, skills)
	})

	t.Run("skips entries with missing description", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"skills": [{"name": "test", "description": "", "files": ["SKILL.md"]}]}`)
		}))
		defer srv.Close()

		skills := loadRemoteSkills(t.Context(), srv.URL, newDiskCache(t.TempDir()))
		assert.Empty(t, skills)
	})

	t.Run("server returns 404", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		skills := loadRemoteSkills(t.Context(), srv.URL, newDiskCache(t.TempDir()))
		assert.Empty(t, skills)
	})

	t.Run("server returns invalid JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `not json`)
		}))
		defer srv.Close()

		skills := loadRemoteSkills(t.Context(), srv.URL, newDiskCache(t.TempDir()))
		assert.Empty(t, skills)
	})

	t.Run("unreachable server", func(t *testing.T) {
		skills := loadRemoteSkills(t.Context(), "http://127.0.0.1:1", newDiskCache(t.TempDir()))
		assert.Empty(t, skills)
	})

	t.Run("uses cached files instead of re-fetching", func(t *testing.T) {
		fetchCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fetchCount++
			switch r.URL.Path {
			case "/.well-known/skills/index.json":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"skills": [{"name": "cached-skill", "description": "Cached", "files": ["SKILL.md"]}]}`)
			case "/.well-known/skills/cached-skill/SKILL.md":
				w.Header().Set("Cache-Control", "max-age=3600")
				fmt.Fprint(w, "# Cached Skill")
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		cache := newDiskCache(t.TempDir())

		// First load
		skills1 := loadRemoteSkills(t.Context(), srv.URL, cache)
		require.Len(t, skills1, 1)
		assert.Equal(t, 2, fetchCount) // index.json + SKILL.md

		// Second load — SKILL.md should be cached
		skills2 := loadRemoteSkills(t.Context(), srv.URL, cache)
		require.Len(t, skills2, 1)
		assert.Equal(t, 3, fetchCount) // only index.json re-fetched, SKILL.md from cache
	})

	t.Run("skips invalid file paths", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/.well-known/skills/index.json":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"skills": [{"name": "test", "description": "Test", "files": ["SKILL.md", "../../../etc/passwd", "/absolute/path"]}]}`)
			case "/.well-known/skills/test/SKILL.md":
				fmt.Fprint(w, "# Test")
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		cache := newDiskCache(t.TempDir())
		skills := loadRemoteSkills(t.Context(), srv.URL, cache)
		require.Len(t, skills, 1)
		// Only SKILL.md should have been fetched, not the malicious paths
	})
}

func TestLoadWithRemoteSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/skills/index.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"skills": [{"name": "remote-skill", "description": "A remote skill", "files": ["SKILL.md"]}]}`)
		case "/.well-known/skills/remote-skill/SKILL.md":
			fmt.Fprint(w, "# Remote Skill")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	skills := Load([]string{srv.URL})

	found := false
	for _, s := range skills {
		if s.Name != "remote-skill" {
			continue
		}
		found = true
		assert.Equal(t, "A remote skill", s.Description)
		// FilePath should now be a local cache path
		content, err := os.ReadFile(s.FilePath)
		require.NoError(t, err)
		assert.Equal(t, "# Remote Skill", string(content))
	}
	assert.True(t, found, "Expected to find remote-skill")
}

func TestLoadWithMixedSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/skills/index.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"skills": [{"name": "remote-skill", "description": "A remote skill", "files": ["SKILL.md"]}]}`)
		case "/.well-known/skills/remote-skill/SKILL.md":
			fmt.Fprint(w, "# Remote")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", t.TempDir())

	skills := Load([]string{"local", srv.URL})

	found := false
	for _, s := range skills {
		if s.Name == "remote-skill" {
			found = true
			assert.Equal(t, "A remote skill", s.Description)
		}
	}
	assert.True(t, found, "Expected to find remote-skill from mixed sources")
}

func TestLoadWithEmptySources(t *testing.T) {
	skills := Load(nil)
	assert.Empty(t, skills)

	skills = Load([]string{})
	assert.Empty(t, skills)
}

func TestRemoteIndex_JSONParsing(t *testing.T) {
	input := `{
		"skills": [
			{
				"name": "test-skill",
				"description": "A test skill",
				"files": ["SKILL.md", "README.md", "templates/"]
			}
		]
	}`

	var idx remoteIndex
	err := json.Unmarshal([]byte(input), &idx)
	require.NoError(t, err)
	require.Len(t, idx.Skills, 1)
	assert.Equal(t, "test-skill", idx.Skills[0].Name)
	assert.Equal(t, "A test skill", idx.Skills[0].Description)
	assert.Equal(t, []string{"SKILL.md", "README.md", "templates/"}, idx.Skills[0].Files)
}

func TestIsValidFilePath(t *testing.T) {
	tests := []struct {
		path  string
		valid bool
	}{
		{"SKILL.md", true},
		{"references/FORMS.md", true},
		{"scripts/extract.py", true},
		{"assets/config.template.yaml", true},
		{"", false},
		{"/absolute/path", false},
		{"../escape", false},
		{"sub/../escape", false},
		{"back\\slash", false},
		{"query?param", false},
		{"hash#fragment", false},
		{"bracket[0]", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.valid, isValidFilePath(tt.path))
		})
	}
}

func TestIsValidSkillName(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"docker-build", true},
		{"k8s_deploy", true},
		{"My.Skill", true},
		{"abc123", true},
		{"", false},
		{".", false},
		{"..", false},
		{".hidden", false},
		{"../escape", false},
		{"with/slash", false},
		{"with\\slash", false},
		{"with space", false},
		{"with:colon", false},
		{"evil?name", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.valid, isValidSkillName(tt.name))
		})
	}
}

// TestLoadRemoteSkills_RejectsSkillNameTraversal ensures a hostile remote
// index cannot use the skill name to place cache files outside the cache
// directory.
func TestLoadRemoteSkills_RejectsSkillNameTraversal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/skills/index.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"skills": [
				{"name": "../evil",   "description": "d", "files": ["SKILL.md"]},
				{"name": "a/b",      "description": "d", "files": ["SKILL.md"]},
				{"name": ".hidden",  "description": "d", "files": ["SKILL.md"]},
				{"name": "ok-skill", "description": "d", "files": ["SKILL.md"]}
			]}`)
		default:
			// Any per-file request succeeds so we can detect it happening.
			fmt.Fprint(w, "# payload")
		}
	}))
	defer srv.Close()

	cacheBase := t.TempDir()
	cache := newDiskCache(cacheBase)
	skills := loadRemoteSkills(t.Context(), srv.URL, cache)

	require.Len(t, skills, 1)
	assert.Equal(t, "ok-skill", skills[0].Name)

	// Nothing should have been written outside the cache base directory.
	parent := filepath.Dir(cacheBase)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, "evil", e.Name(), "cache escape via skill name")
	}
}
