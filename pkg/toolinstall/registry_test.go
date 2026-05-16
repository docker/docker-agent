package toolinstall

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRegistryYAML = `packages:
  - type: github_release
    repo_owner: cli
    repo_name: cli
    description: "GitHub's official CLI"
    asset: "gh_{{.Version}}_{{.OS}}_{{.Arch}}.{{.Format}}"
    format: tar.gz
    files:
      - name: gh
        src: "gh_{{.Version}}_{{.OS}}_{{.Arch}}/bin/gh"
    overrides:
      - goos: windows
        format: zip
    replacements:
      amd64: amd64
      arm64: arm64
      darwin: macOS
      linux: linux
      windows: windows
  - type: github_release
    repo_owner: junegunn
    repo_name: fzf
    description: "A command-line fuzzy finder"
    asset: "fzf-{{.Version}}-{{.OS}}_{{.Arch}}.{{.Format}}"
    format: tar.gz
    files:
      - name: fzf
  - type: github_release
    repo_owner: BurntSushi
    repo_name: ripgrep
    description: "ripgrep recursively searches directories"
    asset: "ripgrep-{{.Version}}-{{.OS}}_{{.Arch}}.{{.Format}}"
    format: tar.gz
    files:
      - name: rg
  - type: go_build
    repo_owner: golang
    repo_name: tools
    description: "Go tools including gopls"
    version_filter: 'Version startsWith "gopls/"'
    path: golang.org/x/tools/gopls
    files:
      - name: gopls
`

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/registry.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testRegistryYAML))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newTestRegistry(t *testing.T, server *httptest.Server) *Registry {
	t.Helper()

	return &Registry{
		httpClient: server.Client(),
		baseURL:    server.URL,
		cacheDir:   filepath.Join(t.TempDir(), "registry-cache"),
	}
}

func TestRegistry_LookupByName(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t, newTestServer(t))

	pkg, err := registry.LookupByName(t.Context(), "cli/cli")
	require.NoError(t, err)
	assert.Equal(t, "github_release", pkg.Type)
	assert.Equal(t, "cli", pkg.RepoOwner)
	assert.Equal(t, "cli", pkg.RepoName)
	assert.Equal(t, "tar.gz", pkg.Format)
	require.Len(t, pkg.Files, 1)
	assert.Equal(t, "gh", pkg.Files[0].Name)
	require.Len(t, pkg.Overrides, 1)
	assert.Equal(t, "macOS", pkg.Replacements["darwin"])
}

func TestRegistry_LookupByName_InvalidFormat(t *testing.T) {
	t.Parallel()

	_, err := newTestRegistry(t, newTestServer(t)).LookupByName(t.Context(), "invalid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected owner/repo format")
}

func TestRegistry_LookupByName_NotFound(t *testing.T) {
	t.Parallel()

	_, err := newTestRegistry(t, newTestServer(t)).LookupByName(t.Context(), "nonexistent/package")
	require.Error(t, err)
}

func TestRegistry_LookupByCommand_ByRepoName(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t, newTestServer(t))

	pkg, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, "junegunn", pkg.RepoOwner)
	assert.Equal(t, "fzf", pkg.RepoName)
}

func TestRegistry_LookupByCommand_BinaryNameDiffersFromRepo(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t, newTestServer(t))

	// "gopls" binary comes from "golang/tools" — found via files[].name scan.
	pkg, err := registry.LookupByCommand(t.Context(), "gopls")
	require.NoError(t, err)
	assert.Equal(t, "golang", pkg.RepoOwner)
	assert.Equal(t, "tools", pkg.RepoName)
	assert.Equal(t, "go_build", pkg.Type)
	assert.Equal(t, "golang.org/x/tools/gopls", pkg.GoInstallPath)
	require.Len(t, pkg.Files, 1)
	assert.Equal(t, "gopls", pkg.Files[0].Name)
}

func TestRegistry_LookupByCommand_NotFound(t *testing.T) {
	t.Parallel()

	_, err := newTestRegistry(t, newTestServer(t)).LookupByCommand(t.Context(), "nonexistent-tool")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no aqua package found")
}

func TestRegistry_Caching(t *testing.T) {
	t.Parallel()

	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.yaml", func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		_, _ = w.Write([]byte(testRegistryYAML))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	registry := &Registry{
		httpClient: server.Client(),
		baseURL:    server.URL,
		cacheDir:   filepath.Join(t.TempDir(), "cache"),
	}

	_, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	_, err = registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, 2, callCount) // No in-memory cache; fetched again.
}

func TestRegistry_CacheFallback(t *testing.T) {
	t.Parallel()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cachePath := filepath.Join(cacheDir, "registry.yaml")
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.WriteFile(cachePath, []byte(testRegistryYAML), 0o644))

	// Server always returns 500.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	registry := &Registry{
		httpClient: server.Client(),
		baseURL:    server.URL,
		cacheDir:   cacheDir,
	}

	// Should fall back to stale cache.
	pkg, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, "junegunn", pkg.RepoOwner)
}

func TestRegistry_SendsAuthHeader(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test_registry_token")

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.yaml", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testRegistryYAML))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	registry := &Registry{
		httpClient: server.Client(),
		baseURL:    server.URL,
		cacheDir:   filepath.Join(t.TempDir(), "cache"),
	}

	_, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, "Bearer ghp_test_registry_token", gotAuth)
}

func TestRegistry_NoAuthHeaderWithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.yaml", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testRegistryYAML))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	registry := &Registry{
		httpClient: server.Client(),
		baseURL:    server.URL,
		cacheDir:   filepath.Join(t.TempDir(), "cache"),
	}

	_, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Empty(t, gotAuth)
}

func TestRegistry_DiskCacheFallback(t *testing.T) {
	t.Parallel()

	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.yaml", func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		_, _ = w.Write([]byte(testRegistryYAML))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	registry := &Registry{
		httpClient: server.Client(),
		baseURL:    server.URL,
		cacheDir:   filepath.Join(t.TempDir(), "cache"),
	}

	// First lookup: fetches from remote and writes to disk cache.
	_, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Stop the server to simulate remote failure.
	server.Close()

	// Second lookup: remote fails, falls back to disk cache.
	pkg, err := registry.LookupByCommand(t.Context(), "fzf")
	require.NoError(t, err)
	assert.Equal(t, "junegunn", pkg.RepoOwner)
}

// --- GitHub auth tests ---

func TestGithubToken_FromGITHUB_TOKEN(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	t.Setenv("GH_TOKEN", "")
	assert.Equal(t, "ghp_test123", githubToken())
}

func TestGithubToken_FromGH_TOKEN(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "ghp_fallback456")
	assert.Equal(t, "ghp_fallback456", githubToken())
}

func TestGithubToken_GITHUB_TOKEN_TakesPrecedence(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_primary")
	t.Setenv("GH_TOKEN", "ghp_secondary")
	assert.Equal(t, "ghp_primary", githubToken())
}

func TestGithubToken_Empty(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	assert.Empty(t, githubToken())
}

func TestSetGitHubAuth_WithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test_token")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/test/test", http.NoBody)
	require.NoError(t, err)

	setGitHubAuth(req)
	assert.Equal(t, "Bearer ghp_test_token", req.Header.Get("Authorization"))
}

func TestSetGitHubAuth_WithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/test/test", http.NoBody)
	require.NoError(t, err)

	setGitHubAuth(req)
	assert.Empty(t, req.Header.Get("Authorization"))
}

// --- Package type tests ---

func TestPackage_BinaryName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "gh", (&Package{
		RepoName: "cli",
		Files:    []PackageFile{{Name: "gh"}},
	}).BinaryName())

	assert.Equal(t, "fzf", (&Package{
		RepoName: "fzf",
	}).BinaryName())
}

func TestPackage_IsGoPackage(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"go_install", "go", "go_build"} {
		assert.True(t, (&Package{Type: typ}).IsGoPackage(), typ)
	}
	assert.False(t, (&Package{Type: "github_release"}).IsGoPackage())
	assert.False(t, (&Package{}).IsGoPackage())
}

func TestProvidesCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pkg     *Package
		command string
		want    bool
	}{
		{"match in files", &Package{Files: []PackageFile{{Name: "gopls"}}}, "gopls", true},
		{"no match in files", &Package{Files: []PackageFile{{Name: "gopls"}}}, "other", false},
		{"no files", &Package{RepoName: "fzf"}, "fzf", false},
		{"second file matches", &Package{Files: []PackageFile{{Name: "gh"}, {Name: "gh-cli"}}}, "gh-cli", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, providesCommand(tt.pkg, tt.command))
		})
	}
}
