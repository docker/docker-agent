package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/skills"
)

func TestFilterSkillsByName_NoFilterReturnsAll(t *testing.T) {
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
	loaded := []skills.Skill{
		{Name: "git"},
	}

	result := filterSkillsByName(loaded, []string{"git", "does-not-exist"})
	assert.Equal(t, []skills.Skill{{Name: "git"}}, result)
}

func TestFilterSkillsByName_EmptyLoaded(t *testing.T) {
	result := filterSkillsByName(nil, []string{"git"})
	assert.Empty(t, result)
}

func TestFilterSkillsByName_KeepsAllDuplicateNameMatches(t *testing.T) {
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
