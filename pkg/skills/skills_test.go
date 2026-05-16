package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Skill
		wantOK  bool
	}{
		{
			name: "valid frontmatter",
			content: `---
name: my-skill
description: A test skill
---

# Skill Content`,
			want:   Skill{Name: "my-skill", Description: "A test skill"},
			wantOK: true,
		},
		{
			name: "quoted values",
			content: `---
name: "quoted-name"
description: 'single quoted desc'
---

Body`,
			want:   Skill{Name: "quoted-name", Description: "single quoted desc"},
			wantOK: true,
		},
		{
			name:    "no frontmatter",
			content: "# Just content\n\nNo frontmatter here.",
			want:    Skill{},
			wantOK:  false,
		},
		{
			name: "only description",
			content: `---
description: Just a description
---

Content`,
			want:   Skill{Description: "Just a description"},
			wantOK: true,
		},
		{
			name:    "windows line endings",
			content: "---\r\nname: windows\r\ndescription: Windows skill\r\n---\r\n\r\nBody",
			want:    Skill{Name: "windows", Description: "Windows skill"},
			wantOK:  true,
		},
		{
			name:    "unclosed frontmatter",
			content: "---\nname: unclosed\ndescription: No closing\n\nBody",
			want:    Skill{},
			wantOK:  false,
		},
		{
			name: "all optional fields",
			content: `---
name: full-skill
description: A complete skill
license: Apache-2.0
compatibility: Requires docker and git
metadata:
  author: test-org
  version: "1.0"
allowed-tools:
  - Bash(git:*)
  - Read
  - Write
---

Body`,
			want: Skill{
				Name:          "full-skill",
				Description:   "A complete skill",
				License:       "Apache-2.0",
				Compatibility: "Requires docker and git",
				Metadata:      map[string]string{"author": "test-org", "version": "1.0"},
				AllowedTools:  []string{"Bash(git:*)", "Read", "Write"},
			},
			wantOK: true,
		},
		{
			name: "allowed-tools as comma-separated string",
			content: `---
name: csv-skill
description: Skill with comma-separated allowed tools
allowed-tools: Read, Grep, Write
---

Body`,
			want: Skill{
				Name:         "csv-skill",
				Description:  "Skill with comma-separated allowed tools",
				AllowedTools: []string{"Read", "Grep", "Write"},
			},
			wantOK: true,
		},
		{
			name: "allowed-tools as single string without commas",
			content: `---
name: single-tool-skill
description: Skill with a single allowed tool
allowed-tools: Read
---

Body`,
			want: Skill{
				Name:         "single-tool-skill",
				Description:  "Skill with a single allowed tool",
				AllowedTools: []string{"Read"},
			},
			wantOK: true,
		},
		{
			name: "context fork",
			content: `---
name: forked-skill
description: A skill that runs as a sub-agent
context: fork
---

Body`,
			want: Skill{
				Name:        "forked-skill",
				Description: "A skill that runs as a sub-agent",
				Context:     "fork",
			},
			wantOK: true,
		},
		{
			name: "context fork with allowed-tools",
			content: `---
name: scoped-fork
description: Fork skill with tool restrictions
context: fork
allowed-tools: Read, Grep
---

Body`,
			want: Skill{
				Name:         "scoped-fork",
				Description:  "Fork skill with tool restrictions",
				Context:      "fork",
				AllowedTools: []string{"Read", "Grep"},
			},
			wantOK: true,
		},
		{
			name: "model override (named)",
			content: `---
name: model-skill
description: A skill that overrides the model
context: fork
model: my_fast_model
---

Body`,
			want: Skill{
				Name:        "model-skill",
				Description: "A skill that overrides the model",
				Context:     "fork",
				Model:       "my_fast_model",
			},
			wantOK: true,
		},
		{
			name: "model override (inline provider/model)",
			content: `---
name: inline-model-skill
description: Skill with inline provider/model override
context: fork
model: openai/gpt-4o-mini
---

Body`,
			want: Skill{
				Name:        "inline-model-skill",
				Description: "Skill with inline provider/model override",
				Context:     "fork",
				Model:       "openai/gpt-4o-mini",
			},
			wantOK: true,
		},
		{
			name:    "allowed-tools list with quoted items",
			content: "---\nname: quoted-tools\ndescription: Skill with quoted tool items\nallowed-tools:\n  - \"Bash(git:*)\"\n  - 'Read'\n---\n\nBody",
			want: Skill{
				Name:         "quoted-tools",
				Description:  "Skill with quoted tool items",
				AllowedTools: []string{"Bash(git:*)", "Read"},
			},
			wantOK: true,
		},
		{
			name:    "colon in description value",
			content: "---\nname: node-webapp-scaffold\ndescription: scaffold a minimal Node.js project. Usage: run this\n---\n\nBody",
			want: Skill{
				Name:        "node-webapp-scaffold",
				Description: "scaffold a minimal Node.js project. Usage: run this",
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseFrontmatter(tt.content)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want.Name, got.Name)
				assert.Equal(t, tt.want.Description, got.Description)
				assert.Equal(t, tt.want.License, got.License)
				assert.Equal(t, tt.want.Compatibility, got.Compatibility)
				assert.Equal(t, tt.want.Metadata, got.Metadata)
				assert.Equal(t, tt.want.AllowedTools, got.AllowedTools)
				assert.Equal(t, tt.want.Context, got.Context)
				assert.Equal(t, tt.want.Model, got.Model)
			}
		})
	}
}

func TestLoadSkillsFromDir_Flat(t *testing.T) {
	tmpDir := t.TempDir()

	skillDir := filepath.Join(tmpDir, "pdf-extractor")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))

	skillContent := `---
name: pdf-extractor
description: Extract text from PDF files
---

# PDF Extraction

Use pdftotext to extract content.
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644))

	skills := loadSkillsFromDir(tmpDir, false)

	require.Len(t, skills, 1)
	assert.Equal(t, "pdf-extractor", skills[0].Name)
	assert.Equal(t, "Extract text from PDF files", skills[0].Description)
	assert.Equal(t, filepath.Join(skillDir, "SKILL.md"), skills[0].FilePath)
	assert.Equal(t, skillDir, skills[0].BaseDir)
}

func TestLoadSkillsFromDir_Recursive(t *testing.T) {
	tmpDir := t.TempDir()

	nestedDir := filepath.Join(tmpDir, "db", "migrate")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))

	skillContent := `---
name: migrate
description: Database migration helper
---

# DB Migration

Run migrations with care.
`
	require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "SKILL.md"), []byte(skillContent), 0o644))

	skills := loadSkillsFromDir(tmpDir, true)

	require.Len(t, skills, 1)
	assert.Equal(t, "migrate", skills[0].Name)
	assert.Equal(t, "Database migration helper", skills[0].Description)
	assert.Equal(t, filepath.Join(nestedDir, "SKILL.md"), skills[0].FilePath)
	assert.Equal(t, nestedDir, skills[0].BaseDir)
}

func TestLoadSkillsFromDir_NameFromDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	skillDir := filepath.Join(tmpDir, "hola")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))

	// Skill without a name field — should derive name from directory.
	skillContent := `---
description: Say hello in Spanish
---

Run the hola command.
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644))

	skills := loadSkillsFromDir(tmpDir, false)

	require.Len(t, skills, 1)
	assert.Equal(t, "hola", skills[0].Name)
	assert.Equal(t, "Say hello in Spanish", skills[0].Description)
}

func TestLoadSkillsFromDir_SkipHiddenAndSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	hiddenDir := filepath.Join(tmpDir, ".hidden-skill")
	require.NoError(t, os.MkdirAll(hiddenDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hiddenDir, "SKILL.md"), []byte("---\ndescription: Hidden\n---\n"), 0o644))

	skills := loadSkillsFromDir(tmpDir, false)
	assert.Empty(t, skills)
}

func TestLoadSkillsFromDir_SkipNoDescription(t *testing.T) {
	tmpDir := t.TempDir()

	skillDir := filepath.Join(tmpDir, "no-desc")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))

	skillContent := `---
name: no-description
---

# No Description

This skill has no description field.
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644))

	skills := loadSkillsFromDir(tmpDir, false)
	assert.Empty(t, skills)
}

func TestLoadSkillsFromDir_AllOptionalFields(t *testing.T) {
	tmpDir := t.TempDir()

	skillDir := filepath.Join(tmpDir, "full-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))

	skillContent := `---
name: full-skill
description: A complete skill with all fields
license: Apache-2.0
compatibility: Requires docker
metadata:
  author: test-org
  version: "2.0"
allowed-tools:
  - Bash(git:*)
  - Read
---

# Full Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644))

	skills := loadSkillsFromDir(tmpDir, false)

	require.Len(t, skills, 1)
	assert.Equal(t, "full-skill", skills[0].Name)
	assert.Equal(t, "A complete skill with all fields", skills[0].Description)
	assert.Equal(t, "Apache-2.0", skills[0].License)
	assert.Equal(t, "Requires docker", skills[0].Compatibility)
	assert.Equal(t, map[string]string{"author": "test-org", "version": "2.0"}, skills[0].Metadata)
	assert.Equal(t, []string{"Bash(git:*)", "Read"}, skills[0].AllowedTools)
}

func TestLoadSkillsFromDir_NonExistentDir(t *testing.T) {
	skills := loadSkillsFromDir("/nonexistent/path/12345", false)
	assert.Empty(t, skills)
}

func TestLoad_Integration(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	claudeProjectDir := filepath.Join(tmpDir, ".claude", "skills", "test-skill")
	require.NoError(t, os.MkdirAll(claudeProjectDir, 0o755))

	skillContent := `---
name: test-skill
description: Test project skill
---

# Test Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(claudeProjectDir, "SKILL.md"), []byte(skillContent), 0o644))

	skills := Load([]string{"local"})

	found := false
	for _, s := range skills {
		if s.Name == "test-skill" {
			found = true
			assert.Equal(t, "Test project skill", s.Description)
			break
		}
	}
	assert.True(t, found, "Expected to find test-skill from project directory")
}

func TestLoad_AgentsSkillsGlobal(t *testing.T) {
	// Create a temp home directory with .agents/skills
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	agentsSkillDir := filepath.Join(tmpHome, ".agents", "skills", "global-skill")
	require.NoError(t, os.MkdirAll(agentsSkillDir, 0o755))

	skillContent := `---
name: global-skill
description: A global agents skill
---

# Global Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(agentsSkillDir, "SKILL.md"), []byte(skillContent), 0o644))

	// Change to a temp directory that doesn't have any skills
	tmpCwd := t.TempDir()
	t.Chdir(tmpCwd)

	skills := Load([]string{"local"})

	found := false
	for _, s := range skills {
		if s.Name == "global-skill" {
			found = true
			assert.Equal(t, "A global agents skill", s.Description)
			assert.Equal(t, filepath.Join(agentsSkillDir, "SKILL.md"), s.FilePath)
			break
		}
	}
	assert.True(t, found, "Expected to find global-skill from ~/.agents/skills")
}

func TestLoad_AgentsSkillsGlobalRecursive(t *testing.T) {
	// Create a temp home directory with nested .agents/skills
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a deeply nested skill under ~/.agents/skills/
	nestedSkillDir := filepath.Join(tmpHome, ".agents", "skills", "project-a", "skill-one")
	require.NoError(t, os.MkdirAll(nestedSkillDir, 0o755))

	skillContent := `---
name: skill-one
description: A nested global agents skill
---

# Nested Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(nestedSkillDir, "SKILL.md"), []byte(skillContent), 0o644))

	// Also create a flat skill to make sure both work
	flatSkillDir := filepath.Join(tmpHome, ".agents", "skills", "flat-skill")
	require.NoError(t, os.MkdirAll(flatSkillDir, 0o755))

	flatContent := `---
name: flat-skill
description: A flat global agents skill
---

# Flat Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(flatSkillDir, "SKILL.md"), []byte(flatContent), 0o644))

	// Change to a temp directory that doesn't have any skills
	tmpCwd := t.TempDir()
	t.Chdir(tmpCwd)

	skills := Load([]string{"local"})

	// Both nested and flat skills should be found
	foundNested := false
	foundFlat := false
	for _, s := range skills {
		switch s.Name {
		case "skill-one":
			foundNested = true
			assert.Equal(t, "A nested global agents skill", s.Description)
			assert.Equal(t, filepath.Join(nestedSkillDir, "SKILL.md"), s.FilePath)
		case "flat-skill":
			foundFlat = true
			assert.Equal(t, "A flat global agents skill", s.Description)
			assert.Equal(t, filepath.Join(flatSkillDir, "SKILL.md"), s.FilePath)
		}
	}
	assert.True(t, foundNested, "Expected to find nested skill-one from ~/.agents/skills/project-a/skill-one")
	assert.True(t, foundFlat, "Expected to find flat-skill from ~/.agents/skills/flat-skill")
}

func TestLoadSkillsFromDir_RecursiveSymlinkCycle(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skill in a subdirectory.
	skillDir := filepath.Join(tmpDir, "real-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))

	skillContent := `---
name: real-skill
description: A real skill
---

# Real Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644))

	// Create a symlink cycle: tmpDir/real-skill/link -> tmpDir
	require.NoError(t, os.Symlink(tmpDir, filepath.Join(skillDir, "link")))

	// loadSkillsRecursive must return without looping forever.
	skills := loadSkillsFromDir(tmpDir, true)

	require.Len(t, skills, 1)
	assert.Equal(t, "real-skill", skills[0].Name)
	assert.Equal(t, "A real skill", skills[0].Description)
}

func TestLoadSkillsFromDir_RecursiveSymlinkSelfReference(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a directory that symlinks to itself.
	require.NoError(t, os.Symlink(tmpDir, filepath.Join(tmpDir, "self")))

	// Must not loop forever.
	skills := loadSkillsFromDir(tmpDir, true)
	assert.Empty(t, skills)
}

func TestLoad_AgentsSkillsProjectFromNestedDir(t *testing.T) {
	// Create a fake git repo with .agents/skills at the root
	tmpRepo := t.TempDir()

	// Create .git directory to mark as git root
	require.NoError(t, os.Mkdir(filepath.Join(tmpRepo, ".git"), 0o755))

	// Create .agents/skills at repo root
	agentsSkillDir := filepath.Join(tmpRepo, ".agents", "skills", "repo-skill")
	require.NoError(t, os.MkdirAll(agentsSkillDir, 0o755))

	skillContent := `---
name: repo-skill
description: A skill from repo root
---

# Repo Skill
`
	require.NoError(t, os.WriteFile(filepath.Join(agentsSkillDir, "SKILL.md"), []byte(skillContent), 0o644))

	// Create a nested directory and chdir there
	nestedDir := filepath.Join(tmpRepo, "sub", "nested", "deep")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))
	t.Chdir(nestedDir)

	// Set HOME to a directory without skills to isolate test
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	skills := Load([]string{"local"})

	found := false
	for _, s := range skills {
		if s.Name == "repo-skill" {
			found = true
			assert.Equal(t, "A skill from repo root", s.Description)
			assert.Equal(t, filepath.Join(agentsSkillDir, "SKILL.md"), s.FilePath)
			break
		}
	}
	assert.True(t, found, "Expected to find repo-skill from .agents/skills at git root")
}

func TestLoad_AgentsSkillsPrecedence_ProjectOverridesGlobal(t *testing.T) {
	// Create a temp home directory with a global skill
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalSkillDir := filepath.Join(tmpHome, ".agents", "skills", "shared-skill")
	require.NoError(t, os.MkdirAll(globalSkillDir, 0o755))

	globalContent := `---
name: shared-skill
description: Global version of shared skill
---

# Global Version
`
	require.NoError(t, os.WriteFile(filepath.Join(globalSkillDir, "SKILL.md"), []byte(globalContent), 0o644))

	// Create a project with the same skill name
	tmpRepo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tmpRepo, ".git"), 0o755))

	projectSkillDir := filepath.Join(tmpRepo, ".agents", "skills", "shared-skill")
	require.NoError(t, os.MkdirAll(projectSkillDir, 0o755))

	projectContent := `---
name: shared-skill
description: Project version of shared skill
---

# Project Version
`
	require.NoError(t, os.WriteFile(filepath.Join(projectSkillDir, "SKILL.md"), []byte(projectContent), 0o644))

	t.Chdir(tmpRepo)

	skills := Load([]string{"local"})

	found := false
	for _, s := range skills {
		if s.Name == "shared-skill" {
			found = true
			// Project should win over global
			assert.Equal(t, "Project version of shared skill", s.Description)
			assert.Equal(t, filepath.Join(projectSkillDir, "SKILL.md"), s.FilePath)
			break
		}
	}
	assert.True(t, found, "Expected to find shared-skill")
}

func TestLoad_AgentsSkillsPrecedence_CloserDirWins(t *testing.T) {
	// Create a git repo with skills at both root and a subdirectory
	tmpRepo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tmpRepo, ".git"), 0o755))

	// Skill at repo root
	rootSkillDir := filepath.Join(tmpRepo, ".agents", "skills", "local-skill")
	require.NoError(t, os.MkdirAll(rootSkillDir, 0o755))

	rootContent := `---
name: local-skill
description: Root version
---

# Root
`
	require.NoError(t, os.WriteFile(filepath.Join(rootSkillDir, "SKILL.md"), []byte(rootContent), 0o644))

	// Same skill in a subdirectory
	subDir := filepath.Join(tmpRepo, "subproject")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	subSkillDir := filepath.Join(subDir, ".agents", "skills", "local-skill")
	require.NoError(t, os.MkdirAll(subSkillDir, 0o755))

	subContent := `---
name: local-skill
description: Subproject version
---

# Subproject
`
	require.NoError(t, os.WriteFile(filepath.Join(subSkillDir, "SKILL.md"), []byte(subContent), 0o644))

	// Set HOME to empty dir
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// From repo root, should get root version
	t.Chdir(tmpRepo)
	skills := Load([]string{"local"})
	for _, s := range skills {
		if s.Name == "local-skill" {
			assert.Equal(t, "Root version", s.Description)
			break
		}
	}

	// From subproject, should get subproject version (closer wins)
	t.Chdir(subDir)
	skills = Load([]string{"local"})
	for _, s := range skills {
		if s.Name == "local-skill" {
			assert.Equal(t, "Subproject version", s.Description)
			break
		}
	}
}

func TestFindGitRoot(t *testing.T) {
	t.Run("git directory at current", func(t *testing.T) {
		tmpDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, ".git"), 0o755))
		got := findGitRoot(tmpDir)
		assert.Equal(t, tmpDir, got)
	})

	t.Run("git directory at parent", func(t *testing.T) {
		tmpDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, ".git"), 0o755))
		nestedDir := filepath.Join(tmpDir, "sub", "nested")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755))
		got := findGitRoot(nestedDir)
		assert.Equal(t, tmpDir, got)
	})

	t.Run("git file (worktree)", func(t *testing.T) {
		tmpDir := t.TempDir()
		// .git as a file (like in worktrees)
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".git"), []byte("gitdir: /somewhere/else/.git"), 0o644))
		got := findGitRoot(tmpDir)
		assert.Equal(t, tmpDir, got)
	})

	t.Run("no git repo", func(t *testing.T) {
		tmpDir := t.TempDir()
		got := findGitRoot(tmpDir)
		assert.Empty(t, got)
	})
}

func TestSkill_IsFork(t *testing.T) {
	assert.True(t, (&Skill{Context: "fork"}).IsFork())
	assert.False(t, (&Skill{Context: ""}).IsFork())
	assert.False(t, (&Skill{Context: "inline"}).IsFork())
	assert.False(t, (&Skill{}).IsFork())
}

func TestProjectSearchDirs(t *testing.T) {
	t.Run("in git repo", func(t *testing.T) {
		tmpRepo := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tmpRepo, ".git"), 0o755))

		nestedDir := filepath.Join(tmpRepo, "a", "b", "c")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755))

		dirs := projectSearchDirs(nestedDir)

		// Should be ordered from root to nested (root first, nested last)
		require.Len(t, dirs, 4)
		assert.Equal(t, tmpRepo, dirs[0])
		assert.Equal(t, filepath.Join(tmpRepo, "a"), dirs[1])
		assert.Equal(t, filepath.Join(tmpRepo, "a", "b"), dirs[2])
		assert.Equal(t, filepath.Join(tmpRepo, "a", "b", "c"), dirs[3])
	})

	t.Run("not in git repo", func(t *testing.T) {
		tmpDir := t.TempDir()

		dirs := projectSearchDirs(tmpDir)

		require.Len(t, dirs, 1)
		assert.Equal(t, tmpDir, dirs[0])
	})

	t.Run("at git root", func(t *testing.T) {
		tmpRepo := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tmpRepo, ".git"), 0o755))

		dirs := projectSearchDirs(tmpRepo)

		require.Len(t, dirs, 1)
		assert.Equal(t, tmpRepo, dirs[0])
	})
}
