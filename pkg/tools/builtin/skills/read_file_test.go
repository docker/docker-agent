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

func TestReadSkillFileSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := filepath.Join(dir, "skill")
	references := filepath.Join(base, "references")
	outside := filepath.Join(dir, "skill-other")
	require.NoError(t, os.MkdirAll(references, 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(references, "FORMS.md"), []byte("internal text"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "FORMS.md"), []byte("external text"), 0o644))

	for _, tt := range []struct {
		name   string
		target string
		file   string
		allow  bool
	}{
		{name: "relative-internal-file", target: "references/FORMS.md", allow: true},
		{name: "relative-internal-directory", target: "references", file: "FORMS.md", allow: true},
		{name: "relative-external-file", target: "../skill-other/FORMS.md"},
		{name: "relative-external-directory", target: "../skill-other", file: "FORMS.md"},
		{name: "absolute-internal-file", target: filepath.Join(references, "FORMS.md")},
		{name: "absolute-internal-directory", target: references, file: "FORMS.md"},
		{name: "absolute-external-file", target: filepath.Join(outside, "FORMS.md")},
		{name: "absolute-external-directory", target: outside, file: "FORMS.md"},
		{name: "dangling", target: "missing.md"},
		{name: "loop", target: "loop"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := os.Symlink(filepath.FromSlash(tt.target), filepath.Join(base, tt.name)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			path := filepath.Join(tt.name, tt.file)
			st := New([]skills.Skill{{Name: "test", BaseDir: base, Local: true}}, dir)
			calls := 0
			rt := &checkingRuntime{check: func(content tools.SkillContent) error {
				calls++
				assert.Equal(t, tools.SkillContent{
					Name: "test", Source: "local", Path: filepath.Join(base, path), Content: "internal text",
				}, content)
				return nil
			}}

			content, err := st.ReadSkillFile(t.Context(), "test", filepath.ToSlash(path), rt)
			if tt.allow {
				require.NoError(t, err)
				assert.Equal(t, "internal text", content)
				assert.Equal(t, 1, calls)
			} else {
				require.ErrorContains(t, err, "reading file:")
				assert.Empty(t, content)
				assert.Zero(t, calls, "failed reads must not reach the content guard")
			}
			assert.Empty(t, rt.runs)
		})
	}
}

func TestReadSkillFileReadErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "resource.md")
	require.NoError(t, os.WriteFile(file, []byte("text"), 0o644))
	for _, tt := range []struct {
		name     string
		base     string
		path     string
		notExist bool
	}{
		{name: "missing-root", base: filepath.Join(dir, "missing"), path: "resource.md", notExist: true},
		{name: "root-is-file", base: file, path: "resource.md"},
		{name: "missing-file", base: dir, path: "missing.md", notExist: true},
		{name: "file-is-directory", base: dir, path: "."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := New([]skills.Skill{{Name: "test", BaseDir: tt.base}}, dir)
			rt := &checkingRuntime{check: func(tools.SkillContent) error {
				t.Error("failed read reached the content guard")
				return nil
			}}
			content, err := st.ReadSkillFile(t.Context(), "test", tt.path, rt)
			require.ErrorContains(t, err, "reading file:")
			if tt.notExist {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			assert.Empty(t, content)
		})
	}
}

func TestReadSkillFileCancellationAfterRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "resource.md"), []byte("text"), 0o644))
	st := New([]skills.Skill{{Name: "test", BaseDir: dir}}, dir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rt := &checkingRuntime{check: func(tools.SkillContent) error {
		t.Error("canceled read reached the content guard")
		return nil
	}}

	content, err := st.ReadSkillFile(ctx, "test", "resource.md", rt)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, content)
	content, err = st.ReadSkillFile(ctx, "test", "missing.md", rt)
	require.ErrorIs(t, err, os.ErrNotExist, "read errors must still precede cancellation")
	assert.Empty(t, content)
}

func TestReadSkillFileSymlinkGuard(t *testing.T) {
	t.Parallel()

	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "denied"}[denied], func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, "references"), 0o755))
			target := filepath.Join(dir, "resource.md")
			path := filepath.Join(dir, "references", "link.md")
			body := "approved text !`echo must-not-run`"
			require.NoError(t, os.WriteFile(target, []byte(body), 0o644))
			if err := os.Symlink(filepath.FromSlash("../resource.md"), path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			st := New([]skills.Skill{{Name: "test", BaseDir: dir, Local: true}}, dir)
			denial := errors.New("denied")
			calls := 0
			rt := &checkingRuntime{check: func(content tools.SkillContent) error {
				calls++
				assert.Equal(t, tools.SkillContent{Name: "test", Source: "local", Path: path, Content: body}, content)
				require.NoError(t, os.WriteFile(target, []byte("replacement text"), 0o644))
				if denied {
					return denial
				}
				return nil
			}}

			content, err := st.ReadSkillFile(t.Context(), "test", "references/link.md", rt)
			if denied {
				require.ErrorIs(t, err, denial)
				assert.Empty(t, content)
			} else {
				require.NoError(t, err)
				assert.Equal(t, body, content, "return the checked bytes without reopening the path")
			}
			assert.Equal(t, 1, calls)
			assert.Empty(t, rt.runs, "supporting files must not expand commands")
		})
	}
}
