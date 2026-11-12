package toolinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// templateData holds the variables available in aqua asset name templates.
type templateData struct {
	Version string
	OS      string
	Arch    string
	Format  string
}

var templateFuncs = template.FuncMap{
	"trimV": func(s string) string { return strings.TrimPrefix(s, "v") },
}

// limits holds the conservative bounds that defend against zip-bomb /
// tar-bomb attacks from adversaries controlling a GitHub release
// referenced by a tool resolver. The defaults are deliberately generous
// for legitimate CLI releases, which typically weigh single-digit MB
// compressed.
//
// Carrying the bounds in a value (rather than package globals) lets each
// caller — and each test — own an independent copy. Tests can shrink a
// limit on their own instance and run with t.Parallel() without racing or
// clobbering a shared global.
type limits struct {
	maxArchiveCompressed   int64
	maxArchiveUncompressed int64
	maxFileUncompressed    int64
	maxArchiveEntries      int
}

// defaultLimits returns the production extraction bounds.
func defaultLimits() limits {
	return limits{
		maxArchiveCompressed:   1 << 30,   // 1 GiB
		maxArchiveUncompressed: 2 << 30,   // 2 GiB
		maxFileUncompressed:    500 << 20, // 500 MiB
		maxArchiveEntries:      100_000,
	}
}

// errExtractTooLarge is returned when an archive (or a single entry
// within it) exceeds the configured extraction size or entry-count
// limit. It is the sentinel for zip-bomb / tar-bomb defenses.
var errExtractTooLarge = errors.New("archive exceeds extraction size limit")

// renderTemplate renders a Go template string with the given data.
func renderTemplate(tmplStr string, data templateData) (string, error) {
	tmpl, err := template.New("asset").Funcs(templateFuncs).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parsing template %q: %w", tmplStr, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template %q: %w", tmplStr, err)
	}

	return buf.String(), nil
}

// extractRelease extracts files from a release asset stream based on format.
// For tar.gz, the response body is streamed directly through gzip → tar.
// For zip, the body is spooled to a temporary file (zip requires random access).
// Raw/single-binary formats are handled by the caller before reaching this function.
func (l limits) extractRelease(body io.ReadCloser, root *os.Root, format string, files []PackageFile, tmplData templateData) error {
	switch format {
	case "tar.gz", "tgz":
		return l.extractTarGz(body, root, files, tmplData)
	case "zip":
		return l.extractZipFromStream(body, root, files, tmplData)
	default:
		return fmt.Errorf("unsupported archive format: %s", format)
	}
}

// writeRawBinary writes a raw (non-archived) binary stream beneath root
// with executable permissions. The body is bounded by maxFileUncompressed
// to avoid an attacker-controlled release asset from filling the disk.
func (l limits) writeRawBinary(r io.Reader, root *os.Root, name string) error {
	destPath, err := safePath(root.Name(), name)
	if err != nil {
		return err
	}
	f, err := root.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating raw binary %s: %w", destPath, err)
	}

	n, copyErr := io.Copy(f, io.LimitReader(r, l.maxFileUncompressed+1))
	if copyErr == nil && n > l.maxFileUncompressed {
		copyErr = errExtractTooLarge
	}
	closeErr := f.Close()

	if copyErr != nil {
		return fmt.Errorf("writing raw binary %s: %w", destPath, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing raw binary %s: %w", destPath, closeErr)
	}

	return nil
}

// extractTarGz extracts files from a tar.gz archive.
// It reads from the provided reader in a streaming fashion (gzip → tar)
// without buffering the entire archive in memory.
func (l limits) extractTarGz(r io.Reader, root *os.Root, files []PackageFile, tmplData templateData) error {
	gzReader, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("extracting tar.gz: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	fileMap, err := buildFileMap(files, tmplData)
	if err != nil {
		return err
	}

	var totalBytes int64
	var entries int
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("extracting tar.gz: %w", err)
		}

		entries++
		if entries > l.maxArchiveEntries {
			return errExtractTooLarge
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		destName, ok := matchFile(header.Name, fileMap)
		if !ok {
			continue
		}

		// header.Size is attacker-controlled, but a header that
		// already advertises a too-large size lets us fail without
		// spending CPU on decompression. The LimitReader below
		// guards against headers that lie.
		if header.Size > l.maxFileUncompressed {
			return errExtractTooLarge
		}

		destPath, err := safePath(root.Name(), destName)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return err
		}

		f, err := root.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}

		n, copyErr := io.Copy(f, io.LimitReader(tarReader, l.maxFileUncompressed+1))
		f.Close()
		if copyErr != nil {
			return copyErr
		}
		if n > l.maxFileUncompressed {
			return errExtractTooLarge
		}
		totalBytes += n
		if totalBytes > l.maxArchiveUncompressed {
			return errExtractTooLarge
		}
	}

	return nil
}

// extractZip extracts files from a zip archive.
// It requires random access via io.ReaderAt; callers should provide either
// an *os.File (spooled to a temp file) or a *bytes.Reader.
func (l limits) extractZip(ra io.ReaderAt, size int64, root *os.Root, files []PackageFile, tmplData templateData) error {
	reader, err := zip.NewReader(ra, size)
	if err != nil {
		return fmt.Errorf("extracting zip: %w", err)
	}

	if len(reader.File) > l.maxArchiveEntries {
		return errExtractTooLarge
	}

	fileMap, err := buildFileMap(files, tmplData)
	if err != nil {
		return err
	}

	var totalBytes int64
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}

		destName, ok := matchFile(f.Name, fileMap)
		if !ok {
			continue
		}

		// UncompressedSize64 comes from the central directory and
		// is attacker-controlled, but lets us reject obvious bombs
		// without spending CPU on decompression first.
		if f.UncompressedSize64 > uint64(l.maxFileUncompressed) { //nolint:gosec // maxFileUncompressed is a positive constant
			return errExtractTooLarge
		}

		destPath, err := safePath(root.Name(), destName)
		if err != nil {
			return err
		}

		n, err := l.extractZipFile(root, f, destPath)
		if err != nil {
			return err
		}
		totalBytes += n
		if totalBytes > l.maxArchiveUncompressed {
			return errExtractTooLarge
		}
	}

	return nil
}

func (l limits) extractZipFile(root *os.Root, f *zip.File, destPath string) (int64, error) {
	if err := root.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, err
	}

	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	outFile, err := root.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	n, err := io.Copy(outFile, io.LimitReader(rc, l.maxFileUncompressed+1))
	if err != nil {
		return n, err
	}
	if n > l.maxFileUncompressed {
		return n, errExtractTooLarge
	}
	return n, nil
}

// extractZipFromStream spools an io.Reader to a temporary file and then
// extracts the zip archive. This avoids holding the entire archive in memory
// while satisfying zip's requirement for random access (io.ReaderAt).
func (l limits) extractZipFromStream(r io.Reader, root *os.Root, files []PackageFile, tmplData templateData) error {
	tmpFile, err := os.CreateTemp("", "cagent-zip-*.zip")
	if err != nil {
		return fmt.Errorf("creating temp file for zip: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	size, err := io.Copy(tmpFile, io.LimitReader(r, l.maxArchiveCompressed+1))
	if err != nil {
		return fmt.Errorf("spooling zip to temp file: %w", err)
	}
	if size > l.maxArchiveCompressed {
		return errExtractTooLarge
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking temp file: %w", err)
	}

	return l.extractZip(tmpFile, size, root, files, tmplData)
}

// buildFileMap builds a map from rendered src paths to destination binary names.
func buildFileMap(files []PackageFile, data templateData) (map[string]string, error) {
	m := make(map[string]string, len(files))
	for _, f := range files {
		src := f.Src
		if src != "" {
			rendered, err := renderTemplate(src, data)
			if err != nil {
				return nil, fmt.Errorf("rendering file src template: %w", err)
			}
			src = rendered
		}
		name := f.Name
		if name == "" {
			name = filepath.Base(src)
		}
		m[src] = executableName(name)
	}
	return m, nil
}

// matchFile checks if an archive entry matches any expected file.
// An empty fileMap means extract everything.
func matchFile(entryName string, fileMap map[string]string) (string, bool) {
	if len(fileMap) == 0 {
		return executableName(filepath.Base(entryName)), true
	}

	for src, dest := range fileMap {
		if entryName == src || filepath.Base(entryName) == filepath.Base(src) {
			return dest, true
		}
	}

	return "", false
}

// errPathTraversal is returned when an archive entry attempts to write
// outside the destination directory (Zip Slip / Tar Slip attack).
var errPathTraversal = errors.New("archive entry attempts path traversal")

// safePath validates that joining destDir with name stays within destDir.
// Returns the cleaned root-relative path or an error on path traversal.
func safePath(destDir, name string) (string, error) {
	destPath := filepath.Join(destDir, name)
	cleanDest := filepath.Clean(destPath)
	cleanDir := filepath.Clean(destDir) + string(os.PathSeparator)

	if !strings.HasPrefix(cleanDest, cleanDir) {
		return "", fmt.Errorf("%w: %q resolves to %q (outside %q)", errPathTraversal, name, cleanDest, destDir)
	}

	return filepath.Rel(destDir, cleanDest)
}
