package root

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompleteAgentFilename(t *testing.T) {
	// Note: These tests change working directory so they cannot run in parallel

	tests := []struct {
		name              string
		setup             func(t *testing.T, dir string)
		toComplete        string
		wantCompletions   []string
		wantNoSpace       bool
		wantNoFileComp    bool
		useRelativePrefix bool // if true, prefix completions with "./"
	}{
		{
			name: "completes yaml files with prefix",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, dir, "agent.yaml")
				writeFile(t, dir, "agent2.yaml")
				writeFile(t, dir, "other.yaml")
				writeFile(t, dir, "readme.md")
			},
			toComplete:        "./ag",
			wantCompletions:   []string{"./agent.yaml", "./agent2.yaml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "completes yml files",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, dir, "config.yml")
				writeFile(t, dir, "config.yaml")
			},
			toComplete:        "./conf",
			wantCompletions:   []string{"./config.yaml", "./config.yml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "completes directories without trailing space",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))
			},
			toComplete:        "./sub",
			wantCompletions:   []string{"./subdir/"},
			wantNoSpace:       true, // directory completion should NOT add space
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "completes both files and directories",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, dir, "myagent.yaml")
				require.NoError(t, os.Mkdir(filepath.Join(dir, "mydir"), 0o755))
			},
			toComplete:        "./my",
			wantCompletions:   []string{"./myagent.yaml", "./mydir/"},
			wantNoSpace:       false, // multiple completions, no NoSpace
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "single directory completion sets NoSpace",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(dir, "onlydir"), 0o755))
			},
			toComplete:        "./only",
			wantCompletions:   []string{"./onlydir/"},
			wantNoSpace:       true,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "excludes non-yaml files",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, dir, "agent.yaml")
				writeFile(t, dir, "agent.json")
				writeFile(t, dir, "agent.txt")
			},
			toComplete:        "./agent",
			wantCompletions:   []string{"./agent.yaml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "handles directory traversal",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				subdir := filepath.Join(dir, "configs")
				require.NoError(t, os.Mkdir(subdir, 0o755))
				writeFile(t, subdir, "dev.yaml")
				writeFile(t, subdir, "prod.yaml")
			},
			toComplete:        "./configs/d",
			wantCompletions:   []string{"./configs/dev.yaml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "lists directory contents with trailing slash",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				subdir := filepath.Join(dir, "agents")
				require.NoError(t, os.Mkdir(subdir, 0o755))
				writeFile(t, subdir, "one.yaml")
				writeFile(t, subdir, "two.yaml")
			},
			toComplete:        "./agents/",
			wantCompletions:   []string{"./agents/one.yaml", "./agents/two.yaml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "handles case-insensitive yaml extension",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, dir, "agent.YAML")
				writeFile(t, dir, "agent2.YML")
			},
			toComplete:        "./agent",
			wantCompletions:   []string{"./agent.YAML", "./agent2.YML"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "returns empty for non-existent directory",
			setup: func(t *testing.T, _ string) {
				t.Helper()
			},
			toComplete:      "./nonexistent/ag",
			wantCompletions: nil,
			wantNoSpace:     false,
			wantNoFileComp:  true,
		},
		{
			name: "handles empty prefix in subdirectory",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				subdir := filepath.Join(dir, "empty")
				require.NoError(t, os.Mkdir(subdir, 0o755))
				writeFile(t, subdir, "a.yaml")
				writeFile(t, subdir, "b.yml")
			},
			toComplete:        "./empty/",
			wantCompletions:   []string{"./empty/a.yaml", "./empty/b.yml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
		{
			name: "preserves original prefix in completions",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, dir, "test.yaml")
			},
			toComplete:        "./te",
			wantCompletions:   []string{"./test.yaml"},
			wantNoSpace:       false,
			wantNoFileComp:    true,
			useRelativePrefix: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp directory and change to it
			tmpDir := t.TempDir()
			tt.setup(t, tmpDir)

			// Change working directory
			t.Chdir(tmpDir)

			completions, directive := completeAgentFilename(tt.toComplete)

			assert.ElementsMatch(t, tt.wantCompletions, completions)

			if tt.wantNoFileComp {
				assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp,
					"expected NoFileComp directive to be set")
			}

			if tt.wantNoSpace {
				assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoSpace,
					"expected NoSpace directive to be set for single directory completion")
			} else if len(completions) > 0 {
				// Only check NoSpace is NOT set if we have completions
				// For single file completions, NoSpace should not be set
				isSingleDir := len(completions) == 1 && completions[0] != "" &&
					completions[0][len(completions[0])-1] == filepath.Separator
				if !isSingleDir {
					assert.Equal(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoSpace,
						"expected NoSpace directive to NOT be set")
				}
			}
		})
	}
}

func TestCompleteAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		toComplete     string
		wantFilesystem bool // if true, should delegate to filesystem completion
	}{
		{
			name:           "path starting with dot delegates to filesystem",
			toComplete:     "./agent",
			wantFilesystem: true,
		},
		{
			name:           "path starting with slash delegates to filesystem",
			toComplete:     "/etc/agent",
			wantFilesystem: true,
		},
		{
			name:           "plain text tries alias completion",
			toComplete:     "myalias",
			wantFilesystem: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, directive := completeAlias(tt.toComplete)

			// Both paths should set NoFileComp
			assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp,
				"expected NoFileComp directive to be set")
		})
	}
}

func TestCompleteAliasIncludesYAMLFiles(t *testing.T) {
	// This test changes the working directory so it cannot run in parallel

	// Create temp directory with YAML files
	tmpDir := t.TempDir()
	writeFile(t, tmpDir, "golang_developer.yaml")
	writeFile(t, tmpDir, "gopher.yaml")
	writeFile(t, tmpDir, "other.yaml")
	writeFile(t, tmpDir, "readme.md") // not a yaml file

	// Change working directory
	t.Chdir(tmpDir)

	// Test that completeAlias includes YAML files starting with "go"
	completions, directive := completeAlias("go")

	// Should include yaml files matching "go" prefix
	assert.Contains(t, completions, "golang_developer.yaml")
	assert.Contains(t, completions, "gopher.yaml")
	assert.NotContains(t, completions, "other.yaml") // doesn't match prefix
	assert.NotContains(t, completions, "readme.md")  // not a yaml file

	// Should set NoFileComp
	assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp,
		"expected NoFileComp directive to be set")
}

func TestCompleteRunExec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		toComplete     string
		wantNoFileComp bool
	}{
		{
			name:           "first arg completes agent file",
			args:           []string{},
			toComplete:     "./",
			wantNoFileComp: true,
		},
		{
			name:           "third arg and beyond returns no completions",
			args:           []string{"agent.yaml", "message"},
			toComplete:     "anything",
			wantNoFileComp: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := &cobra.Command{}
			_, directive := completeRunExec(cmd, tt.args, tt.toComplete)

			if tt.wantNoFileComp {
				assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp,
					"expected NoFileComp directive to be set")
			}
		})
	}
}

func TestCompleteTheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		toComplete string
		wantSome   []string
		wantNone   []string
	}{
		{
			name:       "empty prefix lists default and built-ins",
			toComplete: "",
			wantSome:   []string{"default", "nord", "dracula"},
		},
		{
			name:       "prefix filters to matching themes",
			toComplete: "gruvbox",
			wantSome:   []string{"gruvbox-dark", "gruvbox-light"},
			wantNone:   []string{"default", "nord"},
		},
		{
			name:       "non-matching prefix yields no themes",
			toComplete: "this-theme-does-not-exist",
			wantNone:   []string{"default", "nord"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			completions, directive := completeTheme(nil, nil, tt.toComplete)

			for _, want := range tt.wantSome {
				assert.Contains(t, completions, want)
			}
			for _, notWant := range tt.wantNone {
				assert.NotContains(t, completions, notWant)
			}

			assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp,
				"expected NoFileComp directive to be set")
		})
	}
}

func TestCompleteAlias_NoDirectoriesForPlainPrefix(t *testing.T) {
	// This test changes the working directory so it cannot run in parallel

	tmpDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "subdir"), 0o755))
	writeFile(t, tmpDir, "agent.yaml")

	t.Chdir(tmpDir)

	completions, directive := completeAlias("")

	assert.NotContains(t, completions, "subdir/", "directories should not appear in plain-prefix completion")
	assert.Contains(t, completions, "agent.yaml")
	assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp)
}

func TestCompleteAlias_NoDotfileYAML(t *testing.T) {
	// This test changes the working directory so it cannot run in parallel

	tmpDir := t.TempDir()
	writeFile(t, tmpDir, ".golangci.yml")
	writeFile(t, tmpDir, "agent.yaml")

	t.Chdir(tmpDir)

	completions, directive := completeAlias("")

	assert.NotContains(t, completions, ".golangci.yml", "dotfile YAMLs should not appear in completion")
	assert.Contains(t, completions, "agent.yaml")
	assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp)
}

func TestCompleteAlias_PathPrefixStillDrillsDown(t *testing.T) {
	// This test changes the working directory so it cannot run in parallel

	tmpDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "subdir"), 0o755))

	t.Chdir(tmpDir)

	completions, directive := completeAlias("./")

	assert.Contains(t, completions, "./subdir/", "path drill-down with './' prefix must still include directories")
	assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp)
}

func TestCompleteAlias_SortedAliases(t *testing.T) {
	// This test changes the working directory so it cannot run in parallel

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Write a userconfig with aliases out of alphabetical order.
	// Set HOME so paths.GetConfigDir resolves to our temp directory.
	t.Setenv("HOME", tmpDir)
	cfgDir := filepath.Join(tmpDir, ".config", "cagent")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	cfgContent := `aliases:
  zeta:
    path: /z.yaml
  alpha:
    path: /a.yaml
  mid:
    path: /m.yaml
`
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgContent), 0o644))

	completions, directive := completeAlias("")

	// Extract only the alias keys (strip tab-separated description)
	var aliasNames []string
	for _, c := range completions {
		parts := strings.SplitN(c, "\t", 2)
		name := parts[0]
		// Only include aliases we created (alpha, mid, zeta)
		if name == "alpha" || name == "mid" || name == "zeta" {
			aliasNames = append(aliasNames, name)
		}
	}

	assert.Equal(t, []string{"alpha", "mid", "zeta"}, aliasNames, "aliases should appear in alphabetical order")
	assert.NotEqual(t, cobra.ShellCompDirective(0), directive&cobra.ShellCompDirectiveNoFileComp)
}

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o644))
}
