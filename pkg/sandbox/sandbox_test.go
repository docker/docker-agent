package sandbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
				require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"), []byte(tt.script), 0o755))
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
			json:     `{"vms":[{"name":"my-sandbox","workspaces":["/my/project"]}]}`,
			wd:       "/my/project",
			wantName: "my-sandbox",
		},
		{
			name: "no match",
			json: `{"vms":[{"name":"other","workspaces":["/other/project"]}]}`,
			wd:   "/my/project",
		},
		{
			name: "empty list",
			json: `{"vms":[]}`,
			wd:   "/my/project",
		},
		{
			name:     "multiple sandboxes",
			json:     `{"vms":[{"name":"a","workspaces":["/a"]},{"name":"b","workspaces":["/b"]}]}`,
			wd:       "/b",
			wantName: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\necho '%s'\n", tt.json)
			require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"), []byte(script), 0o755))
			t.Setenv("PATH", fakeDir)

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
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "sbx"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeDir)

	// When sbx is available and preferred, CheckAvailable uses sbx.
	backend := sandbox.NewBackend(true)
	err := backend.CheckAvailable(t.Context())
	require.NoError(t, err)
}

func TestNewBackend_FallsBackToDocker(t *testing.T) {
	fakeDir := t.TempDir()
	// Only docker is available, no sbx.
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeDir)

	backend := sandbox.NewBackend(true)
	err := backend.CheckAvailable(t.Context())
	require.NoError(t, err)
}

func TestForWorkspace_SbxBackend(t *testing.T) {
	fakeDir := t.TempDir()
	jsonData := `{"sandboxes":[{"name":"my-sbx","workspaces":["/my/project"]}]}`
	script := fmt.Sprintf("#!/bin/sh\necho '%s'\n", jsonData)
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "sbx"), []byte(script), 0o755))
	t.Setenv("PATH", fakeDir)

	backend := sandbox.NewBackend(true)
	got := backend.ForWorkspace(t.Context(), "/my/project")
	require.NotNil(t, got)
	assert.Equal(t, "my-sbx", got.Name)
}

func TestExtraWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		wd       string
		agentRef string
		setup    func(t *testing.T) // create files if needed
		want     string
	}{
		{
			name: "empty ref",
			wd:   "/workspace",
			want: "",
		},
		{
			name:     "yaml outside workspace",
			wd:       "/workspace",
			agentRef: "/other/dir/agent.yaml",
			want:     "/other/dir",
		},
		{
			name:     "yaml inside workspace",
			wd:       "/workspace",
			agentRef: "/workspace/sub/agent.yaml",
			want:     "",
		},
		{
			name:     "yaml in workspace root",
			wd:       "/workspace",
			agentRef: "/workspace/agent.yaml",
			want:     "",
		},
		{
			name:     "built-in name",
			wd:       "/workspace",
			agentRef: "default",
			want:     "",
		},
		{
			name:     "OCI reference",
			wd:       "/workspace",
			agentRef: "docker.io/myorg/agent:latest",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			got := sandbox.ExtraWorkspace(tt.wd, tt.agentRef)
			assert.Equal(t, tt.want, got)
		})
	}
}
