package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/skills"
)

func TestFilterSkillsByName_NoFilterReturnsAll(t *testing.T) {
	t.Parallel()
	loaded := []skills.Skill{
		{Name: "git"},
		{Name: "docker"},
		{Name: "kubernetes"},
	}

	result := filterSkillsByName(loaded, nil)
	assert.Equal(t, loaded, result)

	result = filterSkillsByName(loaded, []string{})
	assert.Equal(t, loaded, result)
}

func TestFilterSkillsByName_KeepsMatchingSkills(t *testing.T) {
	t.Parallel()
	loaded := []skills.Skill{
		{Name: "git"},
		{Name: "docker"},
		{Name: "kubernetes"},
	}

	result := filterSkillsByName(loaded, []string{"git", "kubernetes"})
	assert.Equal(t, []skills.Skill{
		{Name: "git"},
		{Name: "kubernetes"},
	}, result)
}

func TestFilterSkillsByName_PreservesOriginalOrder(t *testing.T) {
	t.Parallel()
	loaded := []skills.Skill{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}

	// Include list order should not reorder filtered output.
	result := filterSkillsByName(loaded, []string{"c", "a"})
	assert.Equal(t, []skills.Skill{
		{Name: "a"},
		{Name: "c"},
	}, result)
}

func TestFilterSkillsByName_IgnoresUnknownNames(t *testing.T) {
	t.Parallel()
	loaded := []skills.Skill{
		{Name: "git"},
	}

	result := filterSkillsByName(loaded, []string{"git", "does-not-exist"})
	assert.Equal(t, []skills.Skill{{Name: "git"}}, result)
}

func TestFilterSkillsByName_EmptyLoaded(t *testing.T) {
	t.Parallel()
	result := filterSkillsByName(nil, []string{"git"})
	assert.Empty(t, result)
}

func TestInlineSkills_Conversion(t *testing.T) {
	t.Parallel()
	defs := []latest.InlineSkill{
		{
			Name:         "triage",
			Description:  "Triage a bug.",
			Instructions: "Do the triage.",
			Context:      "fork",
			Model:        "openai/gpt-4o-mini",
			AllowedTools: []string{"read_file"},
		},
	}

	result := inlineSkills(defs)
	require.Len(t, result, 1)

	skill := result[0]
	assert.Equal(t, "triage", skill.Name)
	assert.Equal(t, "Triage a bug.", skill.Description)
	assert.Equal(t, "Do the triage.", skill.InlineContent)
	assert.True(t, skill.IsInline())
	assert.True(t, skill.IsFork())
	assert.Equal(t, "openai/gpt-4o-mini", skill.Model)
	assert.Equal(t, []string{"read_file"}, skill.AllowedTools)
	// Inline skills have no filesystem footprint.
	assert.Empty(t, skill.FilePath)
	assert.Empty(t, skill.BaseDir)
	assert.False(t, skill.Local)
}

func TestInlineSkills_Empty(t *testing.T) {
	t.Parallel()
	assert.Nil(t, inlineSkills(nil))
}

func TestOverrideWithInlineSkills_NoInlineKeepsLoaded(t *testing.T) {
	t.Parallel()
	loaded := []skills.Skill{{Name: "git"}, {Name: "docker"}}

	assert.Equal(t, loaded, overrideWithInlineSkills(loaded, nil))
}

func TestOverrideWithInlineSkills_InlineWinsOverDiscovered(t *testing.T) {
	t.Parallel()
	// A discovered skill sharing a name with an inline one must be dropped,
	// not merely appended after it: otherwise the model sees the name twice
	// and lookups resolve to the discovered skill.
	loaded := []skills.Skill{
		{Name: "commit", Description: "from disk", FilePath: "/skills/commit/SKILL.md", Local: true},
		{Name: "docker", Description: "from disk", Local: true},
	}
	inline := []skills.Skill{{Name: "commit", Description: "from config", InlineContent: "body"}}

	result := overrideWithInlineSkills(loaded, inline)

	require.Len(t, result, 2)
	assert.Equal(t, "docker", result[0].Name)
	assert.Equal(t, "commit", result[1].Name)
	assert.Equal(t, "from config", result[1].Description)
	assert.True(t, result[1].IsInline())
}

func TestOverrideWithInlineSkills_DropsEveryShadowedDuplicate(t *testing.T) {
	t.Parallel()
	// skills.Load keys local and remote skills separately, so the same name can
	// appear more than once. An inline definition must displace all of them.
	loaded := []skills.Skill{
		{Name: "git", Description: "local", Local: true},
		{Name: "git", Description: "remote"},
	}
	inline := []skills.Skill{{Name: "git", Description: "inline", InlineContent: "body"}}

	result := overrideWithInlineSkills(loaded, inline)

	require.Len(t, result, 1)
	assert.Equal(t, "inline", result[0].Description)
}

func TestOverrideWithInlineSkills_DisjointNamesAreConcatenated(t *testing.T) {
	t.Parallel()
	loaded := []skills.Skill{{Name: "git", Local: true}}
	inline := []skills.Skill{{Name: "changelog", InlineContent: "body"}}

	result := overrideWithInlineSkills(loaded, inline)

	require.Len(t, result, 2)
	assert.Equal(t, "git", result[0].Name)
	assert.Equal(t, "changelog", result[1].Name)
}

func TestOverrideWithInlineSkills_NoLoadedSkills(t *testing.T) {
	t.Parallel()
	inline := []skills.Skill{{Name: "changelog", InlineContent: "body"}}

	assert.Equal(t, inline, overrideWithInlineSkills(nil, inline))
}

func TestFilterSkillsByName_KeepsAllDuplicateNameMatches(t *testing.T) {
	t.Parallel()
	// The loaded slice may contain multiple skills with the same name (e.g. one
	// from the local filesystem and one from a remote source, which are keyed
	// separately in skills.Load). The filter must not silently drop duplicates
	// — both should be included so downstream code (NewSkillsToolset) can apply
	// its own precedence rules.
	loaded := []skills.Skill{
		{Name: "git", Description: "local"},
		{Name: "git", Description: "remote"},
		{Name: "docker"},
	}

	result := filterSkillsByName(loaded, []string{"git"})
	assert.Equal(t, []skills.Skill{
		{Name: "git", Description: "local"},
		{Name: "git", Description: "remote"},
	}, result)
}
