package toolinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		data     templateData
		expected string
	}{
		{
			"basic template",
			"tool_{{.Version}}_{{.OS}}_{{.Arch}}.{{.Format}}",
			templateData{Version: "1.0.0", OS: "linux", Arch: "amd64", Format: "tar.gz"},
			"tool_1.0.0_linux_amd64.tar.gz",
		},
		{
			"with macOS replacement",
			"gh_{{.Version}}_{{.OS}}_{{.Arch}}.{{.Format}}",
			templateData{Version: "2.50.0", OS: "macOS", Arch: "arm64", Format: "tar.gz"},
			"gh_2.50.0_macOS_arm64.tar.gz",
		},
		{
			"trimV function",
			"tool_{{trimV .Version}}_{{.OS}}.{{.Format}}",
			templateData{Version: "v1.2.3", OS: "linux", Arch: "amd64", Format: "tar.gz"},
			"tool_1.2.3_linux.tar.gz",
		},
		{
			"no template markers",
			"static-name.tar.gz",
			templateData{Version: "1.0.0", OS: "linux", Arch: "amd64", Format: "tar.gz"},
			"static-name.tar.gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := renderTemplate(tt.template, tt.data)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRenderTemplate_Invalid(t *testing.T) {
	t.Parallel()

	_, err := renderTemplate("{{.Invalid", templateData{})
	require.Error(t, err)
}

func TestWriteRawBinary(t *testing.T) {
	t.Parallel()

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "mytool")
	content := "#!/bin/sh\necho hello"

	err := writeRawBinary(strings.NewReader(content), destPath)
	require.NoError(t, err)

	data, err := os.ReadFile(destPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	info, err := os.Stat(destPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "binary should be executable")
}

func TestWriteRawBinary_ErrorOnBadPath(t *testing.T) {
	t.Parallel()

	err := writeRawBinary(strings.NewReader("data"), "/nonexistent/dir/binary")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating raw binary")
}

func TestExtractTarGz(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("#!/bin/sh\necho hello")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "tool_1.0.0_linux/bin/mytool",
		Mode: 0o755,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	files := []PackageFile{{Name: "mytool", Src: "tool_{{.Version}}_{{.OS}}/bin/mytool"}}
	data := templateData{Version: "1.0.0", OS: "linux", Arch: "amd64"}

	require.NoError(t, extractTarGz(&buf, destDir, files, data))

	extracted, err := os.ReadFile(filepath.Join(destDir, "mytool"))
	require.NoError(t, err)
	assert.Equal(t, content, extracted)
}

func TestExtractZip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	content := []byte("#!/bin/sh\necho hello")
	fw, err := zw.Create("tool_1.0.0/bin/mytool")
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	destDir := t.TempDir()
	files := []PackageFile{{Name: "mytool", Src: "tool_{{.Version}}/bin/mytool"}}
	data := templateData{Version: "1.0.0", OS: "linux", Arch: "amd64"}

	require.NoError(t, extractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), destDir, files, data))

	extracted, err := os.ReadFile(filepath.Join(destDir, "mytool"))
	require.NoError(t, err)
	assert.Equal(t, content, extracted)
}

func TestBuildFileMap(t *testing.T) {
	t.Parallel()

	files := []PackageFile{{Name: "gh", Src: "gh_{{.Version}}_{{.OS}}/bin/gh"}}
	data := templateData{Version: "2.50.0", OS: "macOS", Arch: "arm64"}

	m, err := buildFileMap(files, data)
	require.NoError(t, err)
	assert.Equal(t, "gh", m["gh_2.50.0_macOS/bin/gh"])
}

func TestMatchFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		entry    string
		fileMap  map[string]string
		wantName string
		wantOK   bool
	}{
		{"exact match", "a/b/gh", map[string]string{"a/b/gh": "gh"}, "gh", true},
		{"basename match", "other/path/gh", map[string]string{"a/b/gh": "gh"}, "gh", true},
		{"no match", "other", map[string]string{"a/b/gh": "gh"}, "", false},
		{"empty map extracts all", "some/binary", map[string]string{}, "binary", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, ok := matchFile(tt.entry, tt.fileMap)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantName, name)
			}
		})
	}
}

func TestSafePath(t *testing.T) {
	t.Parallel()

	destDir := t.TempDir()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple name", "mytool", false},
		{"nested path", "bin/mytool", false},
		{"dot-dot traversal", "../../etc/passwd", true},
		{"hidden traversal", "foo/../../etc/passwd", true},
		{"dot prefix", "../mytool", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := safePath(destDir, tt.input)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errPathTraversal)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, result, destDir)
		})
	}
}

func TestExtractTarGz_PathTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("malicious")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "evil",
		Mode: 0o755,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	// Map entry to a traversal dest name.
	files := []PackageFile{{Name: "../../etc/passwd", Src: "evil"}}
	err = extractTarGz(&buf, destDir, files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errPathTraversal)
}

func TestExtractZip_PathTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	content := []byte("malicious")
	fw, err := zw.Create("evil")
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	destDir := t.TempDir()
	// Map entry to a traversal dest name.
	files := []PackageFile{{Name: "../../etc/passwd", Src: "evil"}}
	err = extractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), destDir, files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errPathTraversal)
}

// withFileLimit lowers the per-file extraction cap for the duration of
// a test, restoring it on cleanup.
func withFileLimit(t *testing.T, n int64) {
	t.Helper()
	prev := maxFileUncompressed
	maxFileUncompressed = n
	t.Cleanup(func() { maxFileUncompressed = prev })
}

func TestExtractTarGz_TooLarge(t *testing.T) {
	withFileLimit(t, 32)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Header advertises a small size but the body is larger; the
	// LimitReader-based check must catch the overflow regardless.
	content := bytes.Repeat([]byte("A"), 1024)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "tool/bin/mytool",
		Mode: 0o755,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	files := []PackageFile{{Name: "mytool", Src: "tool/bin/mytool"}}
	err = extractTarGz(&buf, t.TempDir(), files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractTooLarge)
}

func TestExtractZip_TooLarge(t *testing.T) {
	withFileLimit(t, 32)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create("tool/bin/mytool")
	require.NoError(t, err)
	_, err = fw.Write(bytes.Repeat([]byte("A"), 1024))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	files := []PackageFile{{Name: "mytool", Src: "tool/bin/mytool"}}
	err = extractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), t.TempDir(), files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractTooLarge)
}

func TestWriteRawBinary_TooLarge(t *testing.T) {
	withFileLimit(t, 32)

	dest := filepath.Join(t.TempDir(), "big")
	err := writeRawBinary(strings.NewReader(strings.Repeat("A", 1024)), dest)
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractTooLarge)
}
