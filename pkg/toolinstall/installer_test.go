package toolinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveForPlatform_Replacements(t *testing.T) {
	t.Parallel()

	pkg := &Package{
		Replacements: map[string]string{
			"darwin": "macOS",
			"linux":  "linux",
			"amd64":  "x86_64",
		},
	}

	pc := resolveForPlatform(pkg, "v2.50.0")
	assert.Equal(t, "2.50.0", pc.TemplateData.Version)
	assert.NotEmpty(t, pc.TemplateData.OS)
	assert.NotEmpty(t, pc.TemplateData.Arch)
}

func TestResolveForPlatform_VersionPrefixPreservesV(t *testing.T) {
	t.Parallel()

	pc := resolveForPlatform(&Package{VersionPrefix: "v"}, "v2.50.0")
	assert.Equal(t, "v2.50.0", pc.TemplateData.Version)
}

func TestResolveForPlatform_Defaults(t *testing.T) {
	t.Parallel()

	pkg := &Package{
		Format: "tar.gz",
		Asset:  "tool_{{.Version}}.tar.gz",
		Files:  []PackageFile{{Name: "tool", Src: "tool"}},
	}

	pc := resolveForPlatform(pkg, "v1.0.0")
	assert.Equal(t, "tar.gz", pc.Format)
	assert.Equal(t, "tool_{{.Version}}.tar.gz", pc.Asset)
	require.Len(t, pc.Files, 1)
	assert.Equal(t, "1.0.0", pc.TemplateData.Version)
	assert.Equal(t, "tar.gz", pc.TemplateData.Format)
	assert.NotEmpty(t, pc.TemplateData.OS)
	assert.NotEmpty(t, pc.TemplateData.Arch)
}

func TestEnsureSymlink(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryPath := filepath.Join(ToolsDir(), "packages", "cli", "cli", "v1", "gh")
	require.NoError(t, os.MkdirAll(filepath.Dir(binaryPath), 0o755))
	require.NoError(t, os.WriteFile(binaryPath, []byte("#!/bin/sh\necho test"), 0o755))

	require.NoError(t, ensureSymlink("gh", binaryPath))

	target, err := os.Readlink(filepath.Join(BinDir(), "gh"))
	require.NoError(t, err)
	assert.Equal(t, binaryPath, target)

	// Idempotent.
	require.NoError(t, ensureSymlink("gh", binaryPath))
}

func TestInstall_AlreadyInstalled_GitHubRelease(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	pkg := &Package{
		RepoOwner: "cli",
		RepoName:  "cli",
		Files:     []PackageFile{{Name: "gh"}},
	}

	pkgDir := PackageDir("cli", "cli", "v2.50.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	binaryPath := filepath.Join(pkgDir, "gh")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	result, err := (&Registry{httpClient: http.DefaultClient}).Install(t.Context(), pkg, "v2.50.0")
	require.NoError(t, err)
	assert.Equal(t, binaryPath, result)
}

func TestInstall_AlreadyInstalled_GoPackage(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	pkg := &Package{
		Type:          "go_install",
		RepoOwner:     "golang",
		RepoName:      "tools",
		GoInstallPath: "golang.org/x/tools/gopls",
		Files:         []PackageFile{{Name: "gopls"}},
	}

	require.NoError(t, os.MkdirAll(BinDir(), 0o755))
	binaryPath := filepath.Join(BinDir(), "gopls")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	result, err := (&Registry{httpClient: http.DefaultClient}).Install(t.Context(), pkg, "v0.21.1")
	require.NoError(t, err)
	assert.Equal(t, binaryPath, result)
}

func TestInstall_RawFormat(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := "#!/bin/sh\necho hello"

	pkg := &Package{
		RepoOwner: "test",
		RepoName:  "raw-tool",
		Format:    "raw",
		Asset:     "raw-tool",
		Files:     []PackageFile{{Name: "raw-tool"}},
	}

	registry := &Registry{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(binaryContent)),
				}
			}),
		},
	}

	result, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)

	data, err := os.ReadFile(result)
	require.NoError(t, err)
	assert.Equal(t, binaryContent, string(data))

	info, err := os.Stat(result)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "binary should be executable")

	// Verify symlink was created.
	link := filepath.Join(BinDir(), "raw-tool")
	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, result, target)
}

// roundTripFunc adapts a function to http.RoundTripper for test HTTP mocking.
type roundTripFunc func(*http.Request) *http.Response

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

// mockRegistry creates a Registry whose HTTP client returns the given body for every request.
func mockRegistry(body []byte) *Registry {
	return &Registry{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
				}
			}),
		},
	}
}

// assertInstalledBinary checks that a binary exists at path with the expected
// content, is executable, and has a symlink in BinDir().
func assertInstalledBinary(t *testing.T, binaryPath, expectedContent, binaryName string) {
	t.Helper()

	// Verify binary content.
	data, err := os.ReadFile(binaryPath)
	require.NoError(t, err)
	assert.Equal(t, expectedContent, string(data))

	// Verify executable permissions.
	info, err := os.Stat(binaryPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "binary should be executable")

	// Verify symlink in BinDir.
	link := filepath.Join(BinDir(), binaryName)
	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, binaryPath, target)
}

// --- End-to-end Install tests: full download → extract → symlink flow ---

func TestInstallE2E_TarGz(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := "#!/bin/sh\necho hello from mytool"

	// Build a real tar.gz archive containing the binary at the expected path.
	// The asset template is "mytool_{{.Version}}_{{.OS}}_{{.Arch}}.tar.gz"
	// and the src template is "mytool_{{.Version}}_{{.OS}}_{{.Arch}}/mytool".
	entryName := "mytool_1.0.0_" + runtime.GOOS + "_" + runtime.GOARCH + "/mytool"
	archive := buildTarGz(t, entryName, []byte(binaryContent))

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "mytool",
		Format:    "tar.gz",
		Asset:     "mytool_{{.Version}}_{{.OS}}_{{.Arch}}.tar.gz",
		Files: []PackageFile{{
			Name: "mytool",
			Src:  "mytool_{{.Version}}_{{.OS}}_{{.Arch}}/mytool",
		}},
	}

	result, err := mockRegistry(archive).Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, binaryContent, "mytool")
}

func TestInstallE2E_Zip(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := "#!/bin/sh\necho hello from ziptool"

	entryName := "ziptool_1.0.0/ziptool"
	archive := buildZip(t, entryName, []byte(binaryContent))

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "ziptool",
		Format:    "zip",
		Asset:     "ziptool_{{.Version}}.zip",
		Files: []PackageFile{{
			Name: "ziptool",
			Src:  "ziptool_{{.Version}}/ziptool",
		}},
	}

	result, err := mockRegistry(archive).Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, binaryContent, "ziptool")
}

func TestInstallE2E_Raw(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := "#!/bin/sh\necho hello from rawtool"

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "rawtool",
		Format:    "raw",
		Asset:     "rawtool",
		Files:     []PackageFile{{Name: "rawtool"}},
	}

	result, err := mockRegistry([]byte(binaryContent)).Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, binaryContent, "rawtool")
}

func TestInstallE2E_TarGz_NoFilesList(t *testing.T) {
	// When Files is empty, extractTarGz extracts all entries by basename.
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := "#!/bin/sh\necho hello from notool"
	archive := buildTarGz(t, "anytool", []byte(binaryContent))

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "anytool",
		Format:    "tar.gz",
		Asset:     "anytool.tar.gz",
		// No Files — the repo name is used as binary name.
	}

	result, err := mockRegistry(archive).Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, binaryContent, "anytool")
}

func TestInstallE2E_DownloadFailure(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "badtool",
		Format:    "tar.gz",
		Asset:     "badtool.tar.gz",
		Files:     []PackageFile{{Name: "badtool"}},
	}

	registry := &Registry{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}
			}),
		},
	}

	_, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}

func TestInstallE2E_IdempotentReinstall(t *testing.T) {
	// Verify that calling Install twice returns the same path without error.
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := "#!/bin/sh\necho idempotent"
	archive := buildTarGz(t, "idem", []byte(binaryContent))

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "idem",
		Format:    "tar.gz",
		Asset:     "idem.tar.gz",
	}

	registry := mockRegistry(archive)

	result1, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)

	// Second install should hit the "already installed" path.
	result2, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, result1, result2)
}

// --- Test archive builders ---

func buildTarGz(t *testing.T, entryName string, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: entryName,
		Mode: 0o755,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	return buf.Bytes()
}

func buildZip(t *testing.T, entryName string, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	fw, err := zw.Create(entryName)
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}
