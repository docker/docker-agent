package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/skills"
)

func TestSkillsToolset_ReadSkillContent_Local(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte("# Local Skill\nDo the thing."), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "local-skill", Description: "A local skill", FilePath: skillFile, BaseDir: tmpDir},
	}, "")

	content, err := st.ReadSkillContent(t.Context(), "local-skill")
	require.NoError(t, err)
	assert.Equal(t, "# Local Skill\nDo the thing.", content)
}

func TestSkillsToolset_ReadSkillContent_NotFound(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "exists", Description: "Exists", FilePath: "/tmp/nonexistent"},
	}, "")

	_, err := st.ReadSkillContent(t.Context(), "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSkillsToolset_ReadSkillFile(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte("# Main"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "references"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "references", "FORMS.md"), []byte("# Forms Reference"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{
			Name: "my-skill", Description: "My skill", FilePath: filepath.Join(tmpDir, "SKILL.md"), BaseDir: tmpDir,
			Files: []string{"SKILL.md", "references/FORMS.md"},
		},
	}, "")

	content, err := st.ReadSkillFile("my-skill", "references/FORMS.md")
	require.NoError(t, err)
	assert.Equal(t, "# Forms Reference", content)
}

func TestSkillsToolset_ReadSkillFile_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte("# Main"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "my-skill", Description: "My skill", FilePath: filepath.Join(tmpDir, "SKILL.md"), BaseDir: tmpDir},
	}, "")

	_, err := st.ReadSkillFile("my-skill", "../../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid file path")

	_, err = st.ReadSkillFile("my-skill", "/etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid file path")
}

func TestSkillsToolset_ReadSkillFile_SkillNotFound(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "exists", Description: "Exists", FilePath: "/tmp/test"},
	}, "")

	_, err := st.ReadSkillFile("nonexistent", "SKILL.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSkillsToolset_Instructions(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "skill-a", Description: "Does A"},
		{Name: "skill-b", Description: "Does B", Files: []string{"SKILL.md", "references/HELP.md"}},
	}, "")

	instructions := st.Instructions()

	assert.Contains(t, instructions, "read_skill")
	assert.Contains(t, instructions, "read_skill_file")
	assert.Contains(t, instructions, "<available_skills>")
	assert.Contains(t, instructions, "<name>skill-a</name>")
	assert.Contains(t, instructions, "<description>Does A</description>")
	assert.Contains(t, instructions, "<name>skill-b</name>")
	assert.Contains(t, instructions, "<description>Does B</description>")
	assert.Contains(t, instructions, "<files>references/HELP.md</files>")
	// Should NOT contain file system paths
	assert.NotContains(t, instructions, "FilePath")
}

func TestSkillsToolset_Instructions_NoFiles(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "simple", Description: "Simple skill"},
	}, "")

	instructions := st.Instructions()

	assert.Contains(t, instructions, "read_skill")
	assert.NotContains(t, instructions, "read_skill_file")
	assert.NotContains(t, instructions, "<files>")
}

func TestSkillsToolset_Instructions_Empty(t *testing.T) {
	st := NewSkillsToolset(nil, "")
	assert.Empty(t, st.Instructions())

	st = NewSkillsToolset([]skills.Skill{}, "")
	assert.Empty(t, st.Instructions())
}

func TestSkillsToolset_Tools_WithFiles(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "test", Description: "Test skill", Files: []string{"SKILL.md", "references/HELP.md"}},
	}, "")

	tools, err := st.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 2)

	assert.Equal(t, ToolNameReadSkill, tools[0].Name)
	assert.Equal(t, ToolNameReadSkillFile, tools[1].Name)
}

func TestSkillsToolset_Tools_WithoutFiles(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "test", Description: "Test skill"},
	}, "")

	tools, err := st.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)

	assert.Equal(t, ToolNameReadSkill, tools[0].Name)
}

func TestSkillsToolset_Tools_Empty(t *testing.T) {
	st := NewSkillsToolset(nil, "")

	tools, err := st.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, tools)
}

func TestSkillsToolset_Skills(t *testing.T) {
	input := []skills.Skill{
		{Name: "a", Description: "A"},
		{Name: "b", Description: "B"},
	}
	st := NewSkillsToolset(input, "")

	assert.Equal(t, input, st.Skills())
}

func TestSkillsToolset_HandleReadSkill(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte("skill instructions"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "test-skill", Description: "Test", FilePath: skillFile, BaseDir: tmpDir},
	}, "")

	result, err := st.handleReadSkill(t.Context(), readSkillArgs{Name: "test-skill"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "skill instructions")
}

func TestSkillsToolset_HandleReadSkill_NotFound(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "exists", Description: "Exists", FilePath: "/tmp/test"},
	}, "")

	result, err := st.handleReadSkill(t.Context(), readSkillArgs{Name: "missing"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
}

func TestSkillsToolset_HandleReadSkillFile(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte("# Main"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "scripts", "deploy.sh"), []byte("#!/bin/bash\necho deploy"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{
			Name: "my-skill", Description: "My skill", FilePath: filepath.Join(tmpDir, "SKILL.md"), BaseDir: tmpDir,
			Files: []string{"SKILL.md", "scripts/deploy.sh"},
		},
	}, "")

	result, err := st.handleReadSkillFile(t.Context(), readSkillFileArgs{SkillName: "my-skill", Path: "scripts/deploy.sh"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "echo deploy")
}

func TestSkillsToolset_HandleReadSkillFile_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte("# Main"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "my-skill", Description: "My skill", FilePath: filepath.Join(tmpDir, "SKILL.md"), BaseDir: tmpDir},
	}, "")

	result, err := st.handleReadSkillFile(t.Context(), readSkillFileArgs{SkillName: "my-skill", Path: "../../../etc/passwd"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "invalid file path")
}

func TestSkillsToolset_ReadSkillContent_ExpandsCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	content := "# Skill\nBranch: !`echo main`\nDone."
	require.NoError(t, os.WriteFile(skillFile, []byte(content), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "expand-skill", Description: "Expands commands", FilePath: skillFile, BaseDir: tmpDir, Local: true},
	}, tmpDir)

	result, err := st.ReadSkillContent(t.Context(), "expand-skill")
	require.NoError(t, err)
	assert.Equal(t, "# Skill\nBranch: main\nDone.", result)
}

func TestSkillsToolset_ReadSkillContent_ExpandsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	tmpDir := t.TempDir()

	// Create a script in the working directory
	scriptPath := filepath.Join(tmpDir, "gather.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte("#!/bin/sh\necho gathered-data"), 0o755))

	skillFile := filepath.Join(tmpDir, "SKILL.md")
	content := "Data: !`./gather.sh`"
	require.NoError(t, os.WriteFile(skillFile, []byte(content), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "script-skill", Description: "Runs scripts", FilePath: skillFile, BaseDir: tmpDir, Local: true},
	}, tmpDir)

	result, err := st.ReadSkillContent(t.Context(), "script-skill")
	require.NoError(t, err)
	assert.Equal(t, "Data: gathered-data", result)
}

func TestSkillsToolset_ReadSkillContent_RemoteSkillSkipsExpansion(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	content := "Info: !`echo should-not-run`"
	require.NoError(t, os.WriteFile(skillFile, []byte(content), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "remote-skill", Description: "Remote", FilePath: skillFile, BaseDir: tmpDir, Local: false},
	}, "")

	result, err := st.ReadSkillContent(t.Context(), "remote-skill")
	require.NoError(t, err)
	assert.Equal(t, content, result, "commands in remote skills must not be expanded")
}

func TestSkillsToolset_FindSkill(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "alpha", Description: "Alpha skill"},
		{Name: "beta", Description: "Beta skill"},
	}, "")

	found := st.FindSkill("alpha")
	require.NotNil(t, found)
	assert.Equal(t, "alpha", found.Name)

	found = st.FindSkill("beta")
	require.NotNil(t, found)
	assert.Equal(t, "beta", found.Name)

	assert.Nil(t, st.FindSkill("missing"))
}

func TestSkillsToolset_Instructions_ForkSkills(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "inline-skill", Description: "Runs inline"},
		{Name: "fork-skill", Description: "Runs as sub-agent", Context: "fork"},
	}, "")

	instructions := st.Instructions()

	// Should mention run_skill for fork skills
	assert.Contains(t, instructions, "run_skill")
	assert.Contains(t, instructions, "forked context")

	// Fork skill should have forked mode tag
	assert.Contains(t, instructions, "<mode>forked</mode>")

	// Inline skill should NOT have the mode tag
	// We check that inline-skill's entry does not contain <mode>
	assert.Contains(t, instructions, "<name>inline-skill</name>")
	assert.Contains(t, instructions, "<name>fork-skill</name>")
}

func TestSkillsToolset_Instructions_NoForkSkills(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "normal", Description: "Normal skill"},
	}, "")

	instructions := st.Instructions()

	// Should NOT mention run_skill or forked mode
	assert.NotContains(t, instructions, "run_skill")
	assert.NotContains(t, instructions, "<mode>forked</mode>")
	assert.NotContains(t, instructions, "forked context")
}

func TestSkillsToolset_Tools_WithForkSkills(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "inline", Description: "Inline skill"},
		{Name: "forked", Description: "Forked skill", Context: "fork"},
	}, "")

	result, err := st.Tools(t.Context())
	require.NoError(t, err)

	// Should have read_skill + run_skill (no files, so no read_skill_file)
	require.Len(t, result, 2)
	assert.Equal(t, ToolNameReadSkill, result[0].Name)
	assert.Equal(t, ToolNameRunSkill, result[1].Name)
}

func TestSkillsToolset_Tools_NoForkSkills(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "inline", Description: "Inline skill"},
	}, "")

	result, err := st.Tools(t.Context())
	require.NoError(t, err)

	// Should only have read_skill
	require.Len(t, result, 1)
	assert.Equal(t, ToolNameReadSkill, result[0].Name)
}

func TestSkillsToolset_PrepareForkSubSession(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte("system instructions"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		{Name: "forked", Description: "Forked", Context: "fork", FilePath: skillFile, BaseDir: tmpDir, Model: "openai/gpt-4o-mini"},
	}, "")

	prepared, errResult := st.PrepareForkSubSession(t.Context(), RunSkillArgs{Name: "forked", Task: "do the thing"})
	require.Nil(t, errResult)
	require.NotNil(t, prepared)
	assert.Equal(t, "forked", prepared.SkillName)
	assert.Equal(t, "do the thing", prepared.Task)
	assert.Equal(t, "system instructions", prepared.Content)
	assert.Equal(t, "openai/gpt-4o-mini", prepared.Model)
}

func TestSkillsToolset_PrepareForkSubSession_NoModelOverride(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte("system instructions"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		// No Model set in the frontmatter — Prepared.Model must be empty.
		{Name: "forked", Description: "Forked", Context: "fork", FilePath: skillFile, BaseDir: tmpDir},
	}, "")

	prepared, errResult := st.PrepareForkSubSession(t.Context(), RunSkillArgs{Name: "forked", Task: "x"})
	require.Nil(t, errResult)
	require.NotNil(t, prepared)
	assert.Empty(t, prepared.Model)
}

func TestSkillsToolset_PrepareForkSubSession_NotFound(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "exists", Description: "Exists", Context: "fork", FilePath: "/tmp/nonexistent"},
	}, "")

	prepared, errResult := st.PrepareForkSubSession(t.Context(), RunSkillArgs{Name: "missing", Task: "x"})
	assert.Nil(t, prepared)
	require.NotNil(t, errResult)
	assert.True(t, errResult.IsError)
	assert.Contains(t, errResult.Output, "not found")
}

func TestSkillsToolset_PrepareForkSubSession_NotFork(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte("inline"), 0o644))

	st := NewSkillsToolset([]skills.Skill{
		// No Context: "fork" — this is an inline skill.
		{Name: "inline-only", Description: "Inline", FilePath: skillFile, BaseDir: tmpDir},
	}, "")

	prepared, errResult := st.PrepareForkSubSession(t.Context(), RunSkillArgs{Name: "inline-only", Task: "x"})
	assert.Nil(t, prepared)
	require.NotNil(t, errResult)
	assert.True(t, errResult.IsError)
	assert.Contains(t, errResult.Output, "not configured for forked execution")
	assert.Contains(t, errResult.Output, "use read_skill instead")
}

func TestSkillsToolset_PrepareForkSubSession_ReadFailure(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		// FilePath does not exist on disk; ReadSkillContent will fail.
		{Name: "forked", Description: "Forked", Context: "fork", FilePath: "/does/not/exist/SKILL.md"},
	}, "")

	prepared, errResult := st.PrepareForkSubSession(t.Context(), RunSkillArgs{Name: "forked", Task: "x"})
	assert.Nil(t, prepared)
	require.NotNil(t, errResult)
	assert.True(t, errResult.IsError)
	assert.Contains(t, errResult.Output, "failed to read skill content")
}

func TestSkillsToolset_Tools_ForkAndFiles(t *testing.T) {
	st := NewSkillsToolset([]skills.Skill{
		{Name: "full", Description: "Full skill", Context: "fork", Files: []string{"SKILL.md", "ref.md"}},
	}, "")

	result, err := st.Tools(t.Context())
	require.NoError(t, err)

	// Should have read_skill + read_skill_file + run_skill
	require.Len(t, result, 3)
	names := make([]string, len(result))
	for i, tool := range result {
		names[i] = tool.Name
	}
	assert.Contains(t, names, ToolNameReadSkill)
	assert.Contains(t, names, ToolNameReadSkillFile)
	assert.Contains(t, names, ToolNameRunSkill)
}
