package kit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/promptfiles"
)

func TestBuild_RejectsPromptFileTraversal(t *testing.T) {
	isolateEnv(t)
	host := t.TempDir()
	home := filepath.Join(host, "parent", "home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(host, "victim.txt"), []byte("replacement"), 0o600))
	workspace := t.TempDir()
	cache := t.TempDir()
	victim := filepath.Join(cache, "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("unchanged"), 0o600))
	configPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`agents:
  root:
    model: openai/gpt-5
    instruction: hello
    add_prompt_files: ["../../victim.txt"]
`), 0o600))

	_, err := Build(t.Context(), Options{
		AgentRef: configPath, HostHome: home, HostCwd: workspace, Workspace: workspace, CacheDir: cache,
	})
	require.ErrorContains(t, err, "must be a local relative path")
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(data))
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "victim.txt", entries[0].Name())
}

func TestStagePromptFiles_WorkspaceSymlinksNotStaged(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"workspace", "home"} {
		for _, linkType := range []string{"absolute", "relative", "chain", "directory"} {
			t.Run(location+"/"+linkType, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				workspace := filepath.Join(home, "project")
				require.NoError(t, os.Mkdir(workspace, 0o700))
				targetDir := filepath.Join(home, "private")
				if location == "workspace" {
					targetDir = filepath.Join(workspace, "instructions")
				}
				require.NoError(t, os.Mkdir(targetDir, 0o700))
				target := filepath.Join(targetDir, "AGENTS.md")
				require.NoError(t, os.WriteFile(target, []byte("private document\n"), 0o600))

				name := "AGENTS.md"
				link := filepath.Join(workspace, name)
				switch linkType {
				case "relative":
					rel, err := filepath.Rel(workspace, target)
					require.NoError(t, err)
					target = rel
				case "chain":
					require.NoError(t, os.Symlink(target, filepath.Join(workspace, "alias")))
					target = "alias"
				case "directory":
					target = targetDir
					link = filepath.Join(workspace, "linked")
					name = filepath.Join("linked", "AGENTS.md")
				}
				require.NoError(t, os.Symlink(target, link))

				kitDir := t.TempDir()
				cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{name}}}}
				entries, redactions, err := stagePromptFiles(kitDir, cfg, workspace, home, workspace)
				require.NoError(t, err)
				assert.Equal(t, []Entry{{Source: filepath.Join(workspace, name)}}, entries)
				assert.Empty(t, redactions)
				assert.NoFileExists(t, filepath.Join(kitDir, promptfiles.KitSubdir, name))
			})
		}
	}
}

func TestStagePromptFiles_WorkspaceSymlinkKeepsHomePromptFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	workspace := filepath.Join(home, "project")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	privateFile := filepath.Join(home, "private.txt")
	require.NoError(t, os.WriteFile(privateFile, []byte("private document\n"), 0o600))
	workspacePrompt := filepath.Join(workspace, "AGENTS.md")
	require.NoError(t, os.Symlink(privateFile, workspacePrompt))
	homePrompt := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(homePrompt, []byte("home instructions\n"), 0o600))

	kitDir := t.TempDir()
	cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{"AGENTS.md"}}}}
	entries, _, err := stagePromptFiles(kitDir, cfg, workspace, home, workspace)
	require.NoError(t, err)
	target := filepath.Join(promptfiles.KitSubdir, "AGENTS.md")
	assert.Equal(t, []Entry{{Source: workspacePrompt}, {Source: homePrompt, Target: target}}, entries)
	data, err := os.ReadFile(filepath.Join(kitDir, target))
	require.NoError(t, err)
	assert.Equal(t, "home instructions\n", string(data))
}

func TestStagePromptFiles_HomeSymlinkIntoWorkspaceNotStaged(t *testing.T) {
	t.Parallel()
	home, workspace := t.TempDir(), t.TempDir()
	target := filepath.Join(workspace, "instructions.md")
	require.NoError(t, os.WriteFile(target, []byte("workspace instructions\n"), 0o600))
	homePrompt := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.Symlink(target, homePrompt))

	kitDir := t.TempDir()
	cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{"AGENTS.md"}}}}
	entries, _, err := stagePromptFiles(kitDir, cfg, workspace, home, workspace)
	require.NoError(t, err)
	assert.Equal(t, []Entry{{Source: homePrompt}}, entries)
	assert.NoFileExists(t, filepath.Join(kitDir, promptfiles.KitSubdir, "AGENTS.md"))
}

func TestStagePromptFiles_WorkspacePrefixSiblingIsStaged(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	workspace, home := filepath.Join(base, "project"), filepath.Join(base, "project-home")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	require.NoError(t, os.Mkdir(home, 0o700))
	homePrompt := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(homePrompt, []byte("home instructions\n"), 0o600))

	kitDir := t.TempDir()
	cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{"AGENTS.md"}}}}
	entries, _, err := stagePromptFiles(kitDir, cfg, workspace, home, workspace)
	require.NoError(t, err)
	target := filepath.Join(promptfiles.KitSubdir, "AGENTS.md")
	assert.Equal(t, []Entry{{Source: homePrompt, Target: target}}, entries)
	data, err := os.ReadFile(filepath.Join(kitDir, target))
	require.NoError(t, err)
	assert.Equal(t, "home instructions\n", string(data))
}

func TestStagePromptFiles_NestedName(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	name := filepath.Join("instructions", "AGENTS.md")
	require.NoError(t, os.Mkdir(filepath.Join(home, "instructions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte("instructions"), 0o600))
	kitDir := t.TempDir()
	cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{name}}}}
	entries, _, err := stagePromptFiles(kitDir, cfg, t.TempDir(), home, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, filepath.Join(promptfiles.KitSubdir, name), entries[0].Target)
	data, err := os.ReadFile(filepath.Join(kitDir, entries[0].Target))
	require.NoError(t, err)
	assert.Equal(t, "instructions", string(data))
}

func TestStagePromptFiles_RejectsDestinationSymlinks(t *testing.T) {
	t.Parallel()
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint("directory=", directory), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("replacement"), 0o600))
			outside := t.TempDir()
			victim := filepath.Join(outside, "AGENTS.md")
			require.NoError(t, os.WriteFile(victim, []byte("unchanged"), 0o600))
			kitDir := t.TempDir()
			link := filepath.Join(kitDir, promptfiles.KitSubdir)
			target := outside
			if !directory {
				require.NoError(t, os.Mkdir(link, 0o700))
				link = filepath.Join(link, "AGENTS.md")
				target = victim
			}
			require.NoError(t, os.Symlink(target, link))
			cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{"AGENTS.md"}}}}
			_, _, err := stagePromptFiles(kitDir, cfg, t.TempDir(), home, "")
			require.Error(t, err)
			data, err := os.ReadFile(victim)
			require.NoError(t, err)
			assert.Equal(t, "unchanged", string(data))
		})
	}
}

func TestStagePromptFiles_RejectsNonLocalNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "../AGENTS.md", filepath.Join(t.TempDir(), "AGENTS.md")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &latestcfg.Config{Agents: latestcfg.Agents{{Name: "root", AddPromptFiles: []string{name}}}}
			_, _, err := stagePromptFiles(t.TempDir(), cfg, t.TempDir(), t.TempDir(), "")
			require.ErrorContains(t, err, "must be a local relative path")
		})
	}
}
