package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
)

type checkingRuntime struct {
	confirmingRuntime

	check func(tools.SkillContent) error
}

func (r *checkingRuntime) CheckSkillContent(_ context.Context, content tools.SkillContent) error {
	return r.check(content)
}

func TestSkillContentGuardBeforeExpansion(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"local", "remote", "inline"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			body := "untrusted !`echo must-not-run`"
			path := filepath.Join(t.TempDir(), "SKILL.md")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			skill := skills.Skill{Name: "test", FilePath: path, Local: source == "local", Context: "fork"}
			if source == "inline" {
				skill.FilePath = ""
				skill.InlineContent = body
			}
			st := New([]skills.Skill{skill}, "")
			calls := 0
			rt := &checkingRuntime{check: func(content tools.SkillContent) error {
				calls++
				assert.Equal(t, tools.SkillContent{Name: "test", Source: source, Path: skill.FilePath, Content: body}, content)
				return errors.New("denied")
			}}
			content, err := st.ReadSkillContent(t.Context(), "test", rt)
			require.ErrorContains(t, err, "denied")
			assert.Empty(t, content)
			assert.Empty(t, rt.runs)
			prepared, result, err := st.PrepareForkSubSession(t.Context(), RunSkillArgs{Name: "test"}, rt)
			require.NoError(t, err)
			assert.Nil(t, prepared)
			require.NotNil(t, result)
			assert.True(t, result.IsError)
			assert.NotContains(t, result.Output, body)
			assert.Empty(t, rt.runs)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestSkillGuardConsumesCheckedBytes(t *testing.T) {
	t.Parallel()
	for _, supporting := range []bool{false, true} {
		t.Run(map[bool]string{false: "body", true: "supporting"}[supporting], func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "SKILL.md")
			require.NoError(t, os.WriteFile(path, []byte("approved text"), 0o644))
			st := New([]skills.Skill{{Name: "test", FilePath: path, BaseDir: dir, Local: true}}, dir)
			calls := 0
			rt := &checkingRuntime{check: func(content tools.SkillContent) error {
				calls++
				assert.Equal(t, "approved text", content.Content)
				assert.Equal(t, path, content.Path)
				require.NoError(t, os.WriteFile(path, []byte("replacement text"), 0o644))
				return nil
			}}
			var content string
			var err error
			if supporting {
				content, err = st.ReadSkillFile(t.Context(), "test", "SKILL.md", rt)
			} else {
				content, err = st.ReadSkillContent(t.Context(), "test", rt)
			}
			require.NoError(t, err)
			assert.Equal(t, "approved text", content)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestReadSkillFileHandlerChecksContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "resource.md"), []byte("untrusted"), 0o644))
	st := New([]skills.Skill{{Name: "test", BaseDir: dir, Files: []string{"SKILL.md", "resource.md"}}}, dir)
	list, err := st.Tools(t.Context())
	require.NoError(t, err)
	rt := &checkingRuntime{check: func(content tools.SkillContent) error {
		assert.Equal(t, "untrusted", content.Content)
		return errors.New("denied")
	}}
	result, err := list[1].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"skill_name":"test","path":"resource.md"}`}}, rt)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.NotContains(t, result.Output, "untrusted")
}
