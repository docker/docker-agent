package sandbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

func TestCheckAvailable(t *testing.T) {
	tests := []struct {
		name      string
		script    string // empty means no fake binary (docker not found)
		wantErr   string
		wantNoErr bool
	}{
		{
			name:    "no docker installed",
			wantErr: "--sandbox requires Docker Desktop",
		},
		{
			name:    "docker without sandbox support",
			script:  "#!/bin/sh\nexit 1\n",
			wantErr: "--sandbox requires Docker Desktop with sandbox support",
		},
		{
			name:      "docker with sandbox support",
			script:    "#!/bin/sh\nexit 0\n",
			wantNoErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			if tt.script != "" {
				writeMockScript(t, fakeDir, "docker", tt.script)
			}
			t.Setenv("PATH", fakeDir)

			backend := sandbox.NewBackend(false)
			err := backend.CheckAvailable(t.Context())
			if tt.wantNoErr {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestForWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wd       string
		wantName string
	}{
		{
			name:     "matching workspace",
			json:     `{"sandboxes":[{"name":"my-sandbox","workspaces":["/my/project"]}]}`,
			wd:       "/my/project",
			wantName: "my-sandbox",
		},
		{
			name: "no match",
			json: `{"sandboxes":[{"name":"other","workspaces":["/other/project"]}]}`,
			wd:   "/my/project",
		},
		{
			name: "empty list",
			json: `{"sandboxes":[]}`,
			wd:   "/my/project",
		},
		{
			name:     "multiple sandboxes",
			json:     `{"sandboxes":[{"name":"a","workspaces":["/a"]},{"name":"b","workspaces":["/b"]}]}`,
			wd:       "/b",
			wantName: "b",
		},
		{
			name: "legacy vms key still resolves a match",
			// Older docker sandbox versions wrap the list under "vms".
			// The lookup falls back to that key (with a warning logged
			// at runtime) so users on outdated CLIs keep getting
			// sandbox reuse instead of accumulating duplicates.
			json:     `{"vms":[{"name":"my-sandbox","workspaces":["/my/project"]}]}`,
			wd:       "/my/project",
			wantName: "my-sandbox",
		},
	}

	// Write the fake "docker" executable once and have it cat a data
	// file the subtests rewrite. Re-creating the script per subtest
	// would pay the macOS cold-exec penalty (~0.2s) every time, since
	// the OS validates each freshly written binary on first run.
	fakeDir := t.TempDir()
	dataFile := filepath.Join(fakeDir, "ls.json")
	script := fmt.Sprintf("cat %q", dataFile)
	writeMockScript(t, fakeDir, "docker", script)
	// Prepend (not replace) so the fake "docker" wins while the script
	// can still resolve "cat".
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(dataFile, []byte(tt.json), 0o600))

			backend := sandbox.NewBackend(false)
			got := backend.ForWorkspace(t.Context(), tt.wd)
			if tt.wantName == "" {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantName, got.Name)
			}
		})
	}
}

func TestExisting_HasWorkspace(t *testing.T) {
	t.Parallel()

	s := &sandbox.Existing{
		Name:       "test",
		Workspaces: []string{"/workspace", "/extra:ro"},
	}

	assert.True(t, s.HasWorkspace("/workspace"))
	assert.True(t, s.HasWorkspace("/extra"), "should match ignoring :ro suffix")
	assert.False(t, s.HasWorkspace("/other"))
}

func TestNewBackend_PrefersSbx(t *testing.T) {
	fakeDir := t.TempDir()
	writeMockScript(t, fakeDir, "sbx", "exit 0")
	t.Setenv("PATH", fakeDir)

	// When sbx is available and preferred, CheckAvailable uses sbx.
	backend := sandbox.NewBackend(true)
	err := backend.CheckAvailable(t.Context())
	require.NoError(t, err)
}

func TestNewBackend_FallsBackToDocker(t *testing.T) {
	fakeDir := t.TempDir()
	// Only docker is available, no sbx.
	writeMockScript(t, fakeDir, "docker", "exit 0")
	t.Setenv("PATH", fakeDir)

	backend := sandbox.NewBackend(true)
	err := backend.CheckAvailable(t.Context())
	require.NoError(t, err)
}

func TestForWorkspace_SbxBackend(t *testing.T) {
	fakeDir := t.TempDir()
	jsonData := `{"sandboxes":[{"name":"my-sbx","workspaces":["/my/project"]}]}`
	dataFile := filepath.Join(fakeDir, "ls.json")
	require.NoError(t, os.WriteFile(dataFile, []byte(jsonData), 0o600))
	script := fmt.Sprintf("cat %q", dataFile)
	writeMockScript(t, fakeDir, "sbx", script)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := sandbox.NewBackend(true)
	got := backend.ForWorkspace(t.Context(), "/my/project")
	require.NotNil(t, got)
	assert.Equal(t, "my-sbx", got.Name)
}

func TestExtraWorkspace(t *testing.T) {
	t.Parallel()
	t.Run("empty ref", func(t *testing.T) {
		assert.Empty(t, sandbox.ExtraWorkspace("/workspace", ""))
	})

	t.Run("built-in name", func(t *testing.T) {
		assert.Empty(t, sandbox.ExtraWorkspace("/workspace", "default"))
	})

	t.Run("OCI reference", func(t *testing.T) {
		assert.Empty(t, sandbox.ExtraWorkspace("/workspace", "docker.io/myorg/agent:latest"))
	})

	t.Run("yaml outside workspace", func(t *testing.T) {
		agentDir := t.TempDir()
		agent := filepath.Join(agentDir, "agent.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

		got := sandbox.ExtraWorkspace(t.TempDir(), agent)
		assert.Equal(t, agentDir, got)
	})

	t.Run("yaml inside workspace", func(t *testing.T) {
		wd := t.TempDir()
		sub := filepath.Join(wd, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))
		agent := filepath.Join(sub, "agent.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

		assert.Empty(t, sandbox.ExtraWorkspace(wd, agent))
	})

	t.Run("alias points to file outside workspace", func(t *testing.T) {
		// Regression: ExtraWorkspace used to call filepath.Abs("gopher")
		// directly and miss the alias hop, returning "". The sandbox
		// would then launch without the alias's target YAML mounted
		// and the in-sandbox docker-agent could not read it.
		agentDir := t.TempDir()
		agent := filepath.Join(agentDir, "gopher.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

		writeAlias(t, "gopher", agent)

		got := sandbox.ExtraWorkspace(t.TempDir(), "gopher")
		assert.Equal(t, agentDir, got)
	})

	t.Run("alias points to OCI reference", func(t *testing.T) {
		// OCI-backed aliases have nothing on the host filesystem to
		// mount; ExtraWorkspace returns "".
		writeAlias(t, "remote", "docker.io/myorg/agent:latest")

		assert.Empty(t, sandbox.ExtraWorkspace(t.TempDir(), "remote"))
	})
}

// writeAlias points the docker-agent config dir at a fresh tempdir
// and writes a single-alias config.yaml inside it. The override is
// reverted via t.Cleanup.
func writeAlias(t *testing.T, name, path string) {
	t.Helper()

	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	content := "aliases:\n  " + name + ":\n    path: " + path + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600))
}

func TestAllowHosts_RejectsCommaOrWhitespaceEntries(t *testing.T) {
	t.Parallel()

	// Smuggling additional rules through a single argument by
	// embedding a comma (or whitespace) in a hostname must fail
	// loudly: the sbx backend joins the list with commas before
	// forwarding it to the policy engine, and the inner CLI
	// otherwise has no way to distinguish a typo from an attack.
	backend := sandbox.NewBackend(false) // docker backend; sbx behaves the same
	cases := []string{
		"good.example.com,evil.example.com",
		"good.example.com evil.example.com",
		"good.example.com\tother",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			err := backend.AllowHosts(t.Context(), "sandbox-x", []string{host})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "comma or whitespace")
		})
	}
}

func TestAllowHosts_SkipsEmptyEntries(t *testing.T) {
	// Empty / whitespace-only entries are silently dropped; if every
	// requested host is empty we must end up calling no command at
	// all — turn that into an observable check by pointing PATH at
	// a fake "docker" that fails loudly when invoked.
	fakeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"),
		[]byte("#!/bin/sh\necho 'AllowHosts must not call docker for empty inputs' >&2\nexit 99\n"),
		0o755))
	t.Setenv("PATH", fakeDir)

	backend := sandbox.NewBackend(false)
	require.NoError(t, backend.AllowHosts(t.Context(), "sandbox-x", []string{"", "   ", "\t"}))
}

// writeMockScript writes a mock executable script to the given directory.
// On Windows, it converts the POSIX shell script to a basic .bat script.
//
// Limitations:
//   - String-replace of "cat " -> "type " could mis-translate if a future script
//     embeds "cat" in a different context (e.g. a filename).
//   - The "#!/bin/sh\n" prefix strip is vestigial as some callers omit it.
//   - Replaces "exit 0" with "exit /b 0" but leaves plain "exit" untouched.
//
// This helper covers exactly the mock scripts currently used by the sandbox tests.
func writeMockScript(t *testing.T, dir, name, script string) {
	t.Helper()
	script = strings.TrimPrefix(script, "#!/bin/sh\n")

	if runtime.GOOS == "windows" {
		name += ".bat"
		script = strings.ReplaceAll(script, "cat ", "type ")
		script = strings.ReplaceAll(script, "exit 0", "exit /b 0")
		script = "@echo off\n" + script
	} else {
		script = "#!/bin/sh\n" + script
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
}
