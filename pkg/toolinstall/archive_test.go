package toolinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	destPath := filepath.Join(destDir, executableName("mytool"))
	content := "#!/bin/sh\necho hello"

	err := defaultLimits().writeRawBinary(strings.NewReader(content), openExtractionRoot(t, destDir), executableName("mytool"))
	require.NoError(t, err)
	data, err := os.ReadFile(destPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	assertExecutable(t, destPath)
}

func TestWriteRawBinary_ErrorOnBadPath(t *testing.T) {
	t.Parallel()

	err := defaultLimits().writeRawBinary(strings.NewReader("data"), openExtractionRoot(t, t.TempDir()), "nonexistent/dir/binary")
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

	require.NoError(t, defaultLimits().extractTarGz(&buf, openExtractionRoot(t, destDir), files, data))

	extracted, err := os.ReadFile(filepath.Join(destDir, executableName("mytool")))
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

	require.NoError(t, defaultLimits().extractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), openExtractionRoot(t, destDir), files, data))

	extracted, err := os.ReadFile(filepath.Join(destDir, executableName("mytool")))
	require.NoError(t, err)
	assert.Equal(t, content, extracted)
}

func TestBuildFileMap(t *testing.T) {
	t.Parallel()

	files := []PackageFile{{Name: "gh", Src: "gh_{{.Version}}_{{.OS}}/bin/gh"}}
	data := templateData{Version: "2.50.0", OS: "macOS", Arch: "arm64"}

	m, err := buildFileMap(files, data)
	require.NoError(t, err)
	assert.Equal(t, executableName("gh"), m["gh_2.50.0_macOS/bin/gh"])
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
				wantName := tt.wantName
				if len(tt.fileMap) == 0 {
					wantName = executableName(wantName)
				}
				assert.Equal(t, wantName, name)
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
			assert.Equal(t, filepath.Clean(tt.input), result)
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
	err = defaultLimits().extractTarGz(&buf, openExtractionRoot(t, destDir), files, templateData{})
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
	err = defaultLimits().extractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), openExtractionRoot(t, destDir), files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errPathTraversal)
}

// limitsWithFileCap returns extraction limits with a lowered per-file cap.
// Each test gets its own value, so tests that exercise the cap can run with
// t.Parallel() — there is no shared global to clobber or restore.
func limitsWithFileCap(n int64) limits {
	l := defaultLimits()
	l.maxFileUncompressed = n
	return l
}

func TestExtractTarGz_TooLarge(t *testing.T) {
	t.Parallel()

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
	err = limitsWithFileCap(32).extractTarGz(&buf, openExtractionRoot(t, t.TempDir()), files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractTooLarge)
}

func TestExtractZip_TooLarge(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create("tool/bin/mytool")
	require.NoError(t, err)
	_, err = fw.Write(bytes.Repeat([]byte("A"), 1024))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	files := []PackageFile{{Name: "mytool", Src: "tool/bin/mytool"}}
	err = limitsWithFileCap(32).extractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), openExtractionRoot(t, t.TempDir()), files, templateData{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractTooLarge)
}

func TestWriteRawBinary_TooLarge(t *testing.T) {
	t.Parallel()

	root := openExtractionRoot(t, t.TempDir())
	err := limitsWithFileCap(32).writeRawBinary(strings.NewReader(strings.Repeat("A", 1024)), root, "big")
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractTooLarge)
}

func openExtractionRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	return root
}

func extractTestBinary(t *testing.T, root *os.Root, format, name string) error {
	t.Helper()
	const content = "new binary"
	files := []PackageFile{{Name: name, Src: "tool"}}
	switch format {
	case "tar.gz":
		body := buildTarGz(t, "tool", []byte(content))
		return defaultLimits().extractRelease(io.NopCloser(bytes.NewReader(body)), root, format, files, templateData{})
	case "zip":
		body := buildZip(t, "tool", []byte(content))
		return defaultLimits().extractRelease(io.NopCloser(bytes.NewReader(body)), root, format, files, templateData{})
	default:
		return defaultLimits().writeRawBinary(strings.NewReader(content), root, executableName(name))
	}
}

func extractionSymlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		require.NoError(t, err)
	}
}

func TestExtractionRejectsEscapingSymlinks(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"tar.gz", "zip", "raw"} {
		for _, parent := range []bool{false, true} {
			for _, relative := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/parent=%t/relative=%t", format, parent, relative), func(t *testing.T) {
					t.Parallel()
					base := t.TempDir()
					dest := filepath.Join(base, "dest")
					outside := filepath.Join(base, "outside")
					require.NoError(t, os.MkdirAll(dest, 0o755))
					require.NoError(t, os.MkdirAll(outside, 0o755))
					victim := filepath.Join(outside, executableName("tool"))
					require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
					name := "tool"
					target := victim
					link := filepath.Join(dest, executableName(name))
					if parent {
						name = filepath.Join("sub", name)
						target = outside
						link = filepath.Join(dest, "sub")
					}
					if relative {
						var err error
						target, err = filepath.Rel(filepath.Dir(link), target)
						require.NoError(t, err)
					}
					extractionSymlink(t, target, link)

					err := extractTestBinary(t, openExtractionRoot(t, dest), format, name)
					require.Error(t, err)
					got, err := os.ReadFile(victim)
					require.NoError(t, err)
					assert.Equal(t, "untouched", string(got))
				})
			}
		}
	}
}

func TestExtractionAllowsContainedSymlinks(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"tar.gz", "zip", "raw"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			dest := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dest, "real"), 0o755))
			extractionSymlink(t, "real", filepath.Join(dest, "sub"))
			extractionSymlink(t, executableName("target"), filepath.Join(dest, "real", executableName("tool")))
			require.NoError(t, extractTestBinary(t, openExtractionRoot(t, dest), format, filepath.Join("sub", "tool")))
			got, err := os.ReadFile(filepath.Join(dest, "real", executableName("target")))
			require.NoError(t, err)
			assert.Equal(t, "new binary", string(got))
		})
	}
}

func TestExtractionRetainsRootAfterReplacement(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows prevents renaming an open directory")
	}
	for _, format := range []string{"tar.gz", "zip", "raw"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			dest := filepath.Join(base, "dest")
			moved := filepath.Join(base, "moved")
			outside := filepath.Join(base, "outside")
			require.NoError(t, os.Mkdir(dest, 0o755))
			require.NoError(t, os.Mkdir(outside, 0o755))
			root := openExtractionRoot(t, dest)
			require.NoError(t, os.Rename(dest, moved))
			extractionSymlink(t, outside, dest)

			require.NoError(t, extractTestBinary(t, root, format, "tool"))
			got, err := os.ReadFile(filepath.Join(moved, executableName("tool")))
			require.NoError(t, err)
			assert.Equal(t, "new binary", string(got))
			entries, err := os.ReadDir(outside)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestExtractionDoesNotCreateOutsideDirectories(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			dest := t.TempDir()
			outside := t.TempDir()
			extractionSymlink(t, outside, filepath.Join(dest, "sub"))
			err := extractTestBinary(t, openExtractionRoot(t, dest), format, filepath.Join("sub", "new", "tool"))
			require.Error(t, err)
			entries, err := os.ReadDir(outside)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}
