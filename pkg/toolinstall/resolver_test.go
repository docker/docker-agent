package toolinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- EnsureCommand tests ---

func TestEnsureCommand_FoundInPath(t *testing.T) {
	command := "ls"
	if _, err := exec.LookPath(command); err != nil {
		t.Skipf("skipping: %q not in PATH", command)
	}

	result, err := EnsureCommand(t.Context(), command, "")
	require.NoError(t, err)
	assert.Equal(t, command, result)
}

func TestEnsureCommand_DisabledGlobally(t *testing.T) {
	t.Setenv("DOCKER_AGENT_AUTO_INSTALL", "false")
	result, err := EnsureCommand(t.Context(), "nonexistent-command", "")
	require.NoError(t, err)
	assert.Equal(t, "nonexistent-command", result)
}

func TestEnsureCommand_DisabledGlobally_CaseInsensitive(t *testing.T) {
	t.Setenv("DOCKER_AGENT_AUTO_INSTALL", "False")
	result, err := EnsureCommand(t.Context(), "nonexistent-command", "")
	require.NoError(t, err)
	assert.Equal(t, "nonexistent-command", result)
}

func TestEnsureCommand_DisabledPerToolset(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"false", "False", "off", "OFF", " off "} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			result, err := EnsureCommand(t.Context(), "nonexistent-command", value)
			require.NoError(t, err)
			assert.Equal(t, "nonexistent-command", result)
		})
	}
}

func TestEnsureCommand_SoftFail(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())
	t.Setenv("DOCKER_AGENT_AUTO_INSTALL", "")

	_, err := EnsureCommand(t.Context(), "nonexistent-tool", "invalid-ref")
	require.Error(t, err)
}

func TestEnsureCommand_FoundInBinDir(t *testing.T) {
	toolsDir := t.TempDir()
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", toolsDir)
	t.Setenv("DOCKER_AGENT_AUTO_INSTALL", "")

	binDir := filepath.Join(toolsDir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	fakeBin := filepath.Join(binDir, "my-tool")
	require.NoError(t, os.WriteFile(fakeBin, []byte("#!/bin/sh\necho test"), 0o755))

	result, err := EnsureCommand(t.Context(), "my-tool", "")
	require.NoError(t, err)
	assert.Equal(t, fakeBin, result)
}

func TestEnsureCommand_NonExecutableInBinDirIsSkipped(t *testing.T) {
	toolsDir := t.TempDir()
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", toolsDir)
	t.Setenv("DOCKER_AGENT_AUTO_INSTALL", "")

	binDir := filepath.Join(toolsDir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "not-executable"), []byte("data"), 0o644))

	// Falls through to auto-install → fails → returns error.
	_, err := EnsureCommand(t.Context(), "not-executable", "")
	require.Error(t, err)
}

// --- resolve tests ---

func TestResolve_FoundInPath(t *testing.T) {
	t.Parallel()

	command := "ls"
	if _, err := exec.LookPath(command); err != nil {
		t.Skipf("skipping: %q not in PATH", command)
	}

	path, err := resolve(t.Context(), command, "")
	require.NoError(t, err)
	assert.NotEmpty(t, path)
}

func TestResolve_FoundInBinDir(t *testing.T) {
	toolsDir := t.TempDir()
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", toolsDir)

	binDir := filepath.Join(toolsDir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	fakeBin := filepath.Join(binDir, "my-custom-tool")
	require.NoError(t, os.WriteFile(fakeBin, []byte("#!/bin/sh\necho ok"), 0o755))

	path, err := resolve(t.Context(), "my-custom-tool", "")
	require.NoError(t, err)
	assert.Equal(t, fakeBin, path)
}

func TestResolve_NotFoundAnywhere(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	_, err := resolve(t.Context(), "definitely-nonexistent-tool-xyz123", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "looking up command")
}

func TestResolve_InvalidAquaRef(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	_, err := resolve(t.Context(), "sometool", "invalid-ref")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing aqua reference")
}

// --- parseAquaRef tests ---

func TestParseAquaRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ref         string
		wantOwner   string
		wantRepo    string
		wantVersion string
		wantErr     bool
	}{
		{"owner/repo only", "cli/cli", "cli", "cli", "", false},
		{"owner/repo@version", "cli/cli@v2.50.0", "cli", "cli", "v2.50.0", false},
		{"with spaces", "  junegunn/fzf@0.50.0  ", "junegunn", "fzf", "0.50.0", false},
		{"no slash", "justarepo", "", "", "", true},
		{"empty owner", "/repo", "", "", "", true},
		{"empty repo", "owner/", "", "", "", true},
		{"empty string", "", "", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			owner, repo, version, err := parseAquaRef(tt.ref)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
			assert.Equal(t, tt.wantVersion, version)
		})
	}
}

// --- extractVersionPrefix tests ---

func TestExtractVersionPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filter string
		want   string
	}{
		{`Version startsWith "gopls/"`, "gopls/"},
		{`Version startsWith 'cmd/'`, "cmd/"},
		{"", ""},
		{`Version contains "v1"`, ""},
		{`  Version startsWith  "tools/"  `, "tools/"},
	}

	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, extractVersionPrefix(tt.filter))
		})
	}
}
