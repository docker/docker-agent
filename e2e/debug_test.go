package e2e_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/skills"
)

func TestDebug_Toolsets_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "testdata/no_tools.yaml")

	require.Equal(t, "No tools for root\n", output)
}

func TestDebug_Toolsets_Todo(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "testdata/todo_tools.yaml")

	require.Equal(t, "2 tool(s) for root:\n + create_todo - Create a new todo item with a description\n + list_todos - List all current todos with their status\n", output)
}

func TestDebug_Toolsets_JSON_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "--json", "testdata/no_tools.yaml")

	var got []struct {
		Agent string            `json:"agent"`
		Tools []json.RawMessage `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "root", got[0].Agent)
	assert.NotNil(t, got[0].Tools, "tools must be [] not null")
	assert.Empty(t, got[0].Tools)
}

func TestDebug_Toolsets_JSON_Todo(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "--json", "testdata/todo_tools.yaml")

	var got []struct {
		Agent string                       `json:"agent"`
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "root", got[0].Agent)
	require.Len(t, got[0].Tools, 2)

	wantKeys := []string{"annotations", "category", "description", "name", "outputSchema", "parameters"}
	for _, tool := range got[0].Tools {
		assert.ElementsMatch(t, wantKeys, slices.Collect(maps.Keys(tool)))
	}
	assert.JSONEq(t, `"create_todo"`, string(got[0].Tools[0]["name"]))
	assert.JSONEq(t, `"Create a new todo item with a description"`, string(got[0].Tools[0]["description"]))
	assert.JSONEq(t, `"todo"`, string(got[0].Tools[0]["category"]))
	assert.NotEqual(t, "null", string(got[0].Tools[0]["parameters"]))
	assert.JSONEq(t, `"list_todos"`, string(got[0].Tools[1]["name"]))
	assert.JSONEq(t, `"List all current todos with their status"`, string(got[0].Tools[1]["description"]))
}

func TestDebug_Skills_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "skills", "testdata/no_tools.yaml")

	require.Equal(t, "No skills for root\n", output)
}

func TestDebug_Skills_JSON_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "skills", "--json", "testdata/no_tools.yaml")

	var got []struct {
		Agent  string            `json:"agent"`
		Skills []json.RawMessage `json:"skills"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "root", got[0].Agent)
	assert.NotNil(t, got[0].Skills, "skills must be [] not null")
	assert.Empty(t, got[0].Skills)
}

func TestDebug_Skills_JSON_Local(t *testing.T) {
	kit := t.TempDir()
	writeSkill(t, filepath.Join(kit, skills.KitSkillsSubdir, "plain"),
		"---\nname: plain\ndescription: A plain skill\n---\nbody\n")
	writeSkill(t, filepath.Join(kit, skills.KitSkillsSubdir, "forky"),
		"---\nname: forky\ndescription: A forked skill\ncontext: fork\n---\nbody\n")

	t.Setenv(skills.KitDirEnv, kit)

	output := runCLI(t, "debug", "skills", "--json", "testdata/skills_local.yaml")

	var got []struct {
		Agent  string `json:"agent"`
		Skills []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Forked      bool   `json:"forked"`
			Path        string `json:"path"`
		} `json:"skills"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "root", got[0].Agent)
	require.Len(t, got[0].Skills, 2)

	assert.Equal(t, "forky", got[0].Skills[0].Name)
	assert.Equal(t, "A forked skill", got[0].Skills[0].Description)
	assert.True(t, got[0].Skills[0].Forked)
	assert.Equal(t, filepath.Join(kit, skills.KitSkillsSubdir, "forky", "SKILL.md"), got[0].Skills[0].Path)

	assert.Equal(t, "plain", got[0].Skills[1].Name)
	assert.Equal(t, "A plain skill", got[0].Skills[1].Description)
	assert.False(t, got[0].Skills[1].Forked)
	assert.Equal(t, filepath.Join(kit, skills.KitSkillsSubdir, "plain", "SKILL.md"), got[0].Skills[1].Path)
}

// TestDebug_Skills_Local stages two skills (one regular, one forked) in an
// isolated kit directory and asserts that `debug skills` lists each one with
// its name, description, and the [forked] marker for fork-context skills.
func TestDebug_Skills_Local(t *testing.T) {
	kit := t.TempDir()
	writeSkill(t, filepath.Join(kit, skills.KitSkillsSubdir, "plain"),
		"---\nname: plain\ndescription: A plain skill\n---\nbody\n")
	writeSkill(t, filepath.Join(kit, skills.KitSkillsSubdir, "forky"),
		"---\nname: forky\ndescription: A forked skill\ncontext: fork\n---\nbody\n")

	t.Setenv(skills.KitDirEnv, kit)

	output := runCLI(t, "debug", "skills", "testdata/skills_local.yaml")

	require.Equal(t,
		"2 skill(s) for root:\n"+
			" + forky [forked] - A forked skill\n"+
			" + plain - A plain skill\n",
		output,
	)
}

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}
