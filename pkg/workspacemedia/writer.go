// Package workspacemedia writes model-generated media into the user's
// workspace as ordinary, visible files — the same way generated code or
// text lands there. It is a pure naming/collision layer: the caller hands
// it a workspace root, a requested relative path (or a generic fallback
// name such as "generated"), the bytes, and the MIME type; it returns the
// exact workspace-relative path it wrote.
//
// Guarantees:
//   - Never overwrites. The final name is claimed with O_CREATE|O_EXCL and
//     an existing entry of any kind (including a symlink) means "taken":
//     the writer retries with a dash suffix (name-1.ext, name-2.ext, ...),
//     which also makes concurrent same-name writers safe.
//   - Atomic publish. Bytes go to a sibling temp file renamed over the
//     claimed name, so a reader observes either the empty claim or the
//     full content, never a partial write. A failed write removes the
//     empty claim instead of leaving a zero-byte "generated" file behind.
//   - Workspace containment. Every directory and file operation goes
//     through os.Root, so absolute paths, ".." traversal, invalid or
//     Windows-reserved segments, and symlink escapes are rejected with
//     [ErrPathEscape] rather than followed.
//   - The final filename's extension always agrees with the MIME type when
//     the type is known; a conflicting requested extension is corrected
//     and reported via [Result] so the caller can show a notice.
//
// Files use ordinary user-file modes (0o755 directories, 0o644 files, both
// umask-masked). On Windows modes are ignored and entries inherit the
// parent directory's ACLs — same convention as pkg/atomicfile.
package workspacemedia

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"slices"
	"strings"
)

// ErrPathEscape classifies a requested path the writer refuses to touch:
// absolute, containing "..", an empty/invalid/Windows-reserved segment, or
// resolving outside the workspace root through a symlinked parent.
// Classification only — the caller decides how to react (e.g. ask the user
// to confirm an out-of-workspace target, or redirect to a safe name).
var ErrPathEscape = errors.New("path escapes the workspace or has invalid segments")

// Result describes a completed write.
type Result struct {
	// RelPath is the exact final path written, relative to the workspace
	// root and slash-separated. Persist this verbatim.
	RelPath string

	// ExtensionCorrected reports that the requested filename's extension
	// conflicted with the MIME-derived one and was replaced. Callers should
	// surface a notice (e.g. "saved as sunshine.png — the provider returned
	// PNG data"). RequestedExtension holds the original, with leading dot.
	ExtensionCorrected bool
	RequestedExtension string
}

// maxNameAttempts bounds the dash-suffix collision retry so a pathological
// directory cannot loop forever; exhaustion surfaces as [ErrNameExhausted].
const maxNameAttempts = 10000

// ErrNameExhausted classifies collision-suffix exhaustion: every candidate
// name up to maxNameAttempts already exists. Match with errors.Is when the
// failure must be explained without echoing the requested path.
var ErrNameExhausted = errors.New("no free filename after exhausting collision suffixes")

// Write stores data under workspaceRoot at requestedPath, sanitized and
// collision-avoided per the package contract, and returns the exact
// workspace-relative path written. Prompt-directed subdirectories in
// requestedPath are created as needed. A rejected path returns an error
// matching [ErrPathEscape], collision-suffix exhaustion one matching
// [ErrNameExhausted]; any other failure (unwritable directory, full
// disk, ...) is returned as-is for the caller to surface.
func Write(workspaceRoot, requestedPath string, data []byte, mimeType string) (Result, error) {
	return write(workspaceRoot, requestedPath, bytes.NewReader(data), mimeType)
}

func write(workspaceRoot, requestedPath string, r io.Reader, mimeType string) (Result, error) {
	dir, base, requestedExt, err := splitRequestedPath(requestedPath)
	if err != nil {
		return Result{}, err
	}
	ext, corrected := finalExtension(requestedExt, mimeType)

	root, err := os.OpenRoot(workspaceRoot)
	if err != nil {
		return Result{}, fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()

	if dir != "" {
		switch err := root.MkdirAll(dir, 0o755); {
		case err == nil:
		case isPathEscape(err):
			return Result{}, escapeError(requestedPath, err)
		case errors.Is(err, fs.ErrExist):
			// os.Root.MkdirAll reports an existing symlink as ErrExist instead
			// of traversing it. Let the OpenFile claim below decide: it resolves
			// symlinks, allows in-root targets, and classifies escapes.
		default:
			return Result{}, fmt.Errorf("create directory %q: %w", dir, err)
		}
	}

	rel, err := claimAndPublish(root, dir, base, ext, r)
	if err != nil {
		return Result{}, err
	}
	res := Result{RelPath: rel}
	if corrected {
		res.ExtensionCorrected = true
		res.RequestedExtension = requestedExt
	}
	return res, nil
}

// claimAndPublish reserves the first free candidate name with
// O_CREATE|O_EXCL — the claim is what serializes concurrent writers and
// keeps an existing file (or symlink) from ever being replaced — then
// atomically publishes r's content into it.
func claimAndPublish(root *os.Root, dir, base, ext string, r io.Reader) (string, error) {
	for n := range maxNameAttempts {
		name := base + ext
		if n > 0 {
			name = fmt.Sprintf("%s-%d%s", base, n, ext)
		}
		rel := path.Join(dir, name)

		claim, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		switch {
		case errors.Is(err, fs.ErrExist):
			continue
		case isPathEscape(err):
			return "", escapeError(rel, err)
		case err != nil:
			return "", fmt.Errorf("claim %q: %w", rel, err)
		}
		claim.Close()

		if err := publish(root, dir, rel, r); err != nil {
			// The claim is still the empty placeholder; leaving it around
			// would look like a zero-byte generated file.
			_ = root.Remove(rel)
			return "", err
		}
		return rel, nil
	}
	return "", fmt.Errorf("%w: %q after %d attempts", ErrNameExhausted, path.Join(dir, base+ext), maxNameAttempts)
}

// publish writes r to a sibling temp file, syncs it, and renames it over
// the claimed name. os.Root.Rename replaces the destination atomically on
// POSIX; on Windows it renames with POSIX semantics (see
// pkg/atomicfile/write_windows.go, which this mirrors inside an os.Root).
func publish(root *os.Root, dir, claimed string, r io.Reader) error {
	tmp, f, err := createTemp(root, dir)
	if err != nil {
		return err
	}
	// The rename consumes the temp name on success, so this only collects
	// the temp file after a failure.
	defer func() { _ = root.Remove(tmp) }()

	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("write %q: %w", tmp, err)
	}
	// Sync before the rename so a crash cannot publish a torn file.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flush %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", tmp, err)
	}
	if err := root.Rename(tmp, claimed); err != nil {
		return fmt.Errorf("publish %q: %w", claimed, err)
	}
	return nil
}

// createTemp is os.CreateTemp confined to root (os.Root has no CreateTemp
// as of Go 1.26). The generic random name never collides in practice; the
// small retry is only for completeness.
func createTemp(root *os.Root, dir string) (string, *os.File, error) {
	for range 3 {
		name := path.Join(dir, ".media-"+rand.Text()+".tmp")
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create temp file: %w", err)
		}
		return name, f, nil
	}
	return "", nil, errors.New("create temp file: name collisions")
}

// splitRequestedPath validates and sanitizes the requested path, returning
// the slash-joined parent directory (possibly ""), the filename without
// extension, and the requested extension (with leading dot, possibly "").
func splitRequestedPath(requested string) (dir, base, ext string, err error) {
	if isAbsolutePath(requested) {
		return "", "", "", escapeError(requested, errors.New("absolute path"))
	}
	// Accept both separators: model-provided paths may be Windows-style.
	segments := strings.FieldsFunc(requested, func(r rune) bool { return r == '/' || r == '\\' })

	var cleaned []string
	for _, seg := range segments {
		switch seg {
		case ".":
			continue
		case "..":
			return "", "", "", escapeError(requested, errors.New(`".." segment`))
		}
		s := sanitizeSegment(seg)
		switch {
		case s == "":
			return "", "", "", escapeError(requested, fmt.Errorf("invalid segment %q", seg))
		case isReservedName(s):
			return "", "", "", escapeError(requested, fmt.Errorf("reserved name %q", s))
		}
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		return "", "", "", escapeError(requested, errors.New("empty path"))
	}

	base = cleaned[len(cleaned)-1]
	dir = path.Join(cleaned[:len(cleaned)-1]...)
	ext = path.Ext(base)
	if ext == base {
		// Dotfile-style name (".name"): the whole segment is the base.
		ext = ""
	}
	base = strings.TrimSuffix(base, ext)
	// Re-trim: stripping the extension can expose trailing dots/spaces
	// ("photo..png" -> "photo."), which Windows silently drops.
	if base = strings.TrimRight(base, ". "); base == "" {
		return "", "", "", escapeError(requested, errors.New("empty filename"))
	}
	return dir, base, ext, nil
}

// isAbsolutePath detects rooted paths without filepath.IsAbs, which only
// recognizes drive-letter paths when compiled for Windows.
func isAbsolutePath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	if len(p) >= 2 && p[1] == ':' &&
		(('a' <= p[0] && p[0] <= 'z') || ('A' <= p[0] && p[0] <= 'Z')) {
		return true
	}
	return false
}

// sanitizeSegment replaces characters that are unrepresentable in Windows
// file names (and control characters) with '-', and trims trailing dots
// and spaces, which Windows silently strips — that would break the "exact
// final path" contract.
func sanitizeSegment(seg string) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"|?*`, r) {
			return '-'
		}
		return r
	}, seg)
	s = strings.TrimRight(s, ". ")
	return strings.TrimLeft(s, " ")
}

// windowsReservedNames lists device names Windows refuses as file names,
// with or without an extension (CON.png is as unusable as CON). Rejected
// on every platform so a generated tree stays usable on Windows checkouts.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

func isReservedName(segment string) bool {
	name, _, _ := strings.Cut(segment, ".")
	return windowsReservedNames[strings.ToUpper(name)]
}

// finalExtension picks the filename extension: the MIME-derived one when
// the type is known, otherwise the requested one (".bin" when neither
// exists). corrected is true only when a requested extension conflicted
// with the MIME type and was replaced — the caller should tell the user.
func finalExtension(requestedExt, mimeType string) (ext string, corrected bool) {
	if mt, _, err := mime.ParseMediaType(mimeType); err == nil {
		mimeType = mt
	}
	actual := extensionForMIME(mimeType)
	switch {
	case actual == "":
		if requestedExt == "" {
			return ".bin", false
		}
		return requestedExt, false
	case requestedExt == "":
		return actual, false
	case extensionMatchesMIME(requestedExt, mimeType, actual):
		return requestedExt, false
	default:
		return actual, true
	}
}

// knownExtensions pins the extension for MIME types generated media
// commonly uses. mime.ExtensionsByType returns OS-dependent, sometimes
// surprising results (e.g. "image/jpeg" resolving to ".jfif" ahead of
// ".jpg" on some systems), so common types are pinned and the system mime
// database is only a fallback.
var knownExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// extensionForMIME maps a normalized MIME type to an extension (with
// leading dot), or "" when the type is empty or unknown.
func extensionForMIME(mimeType string) string {
	if ext, ok := knownExtensions[mimeType]; ok {
		return ext
	}
	if mimeType == "" {
		return ""
	}
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(exts) == 0 {
		return ""
	}
	return exts[0]
}

// extensionMatchesMIME reports whether requestedExt is an acceptable
// spelling for mimeType (e.g. ".jpeg" for image/jpeg), so valid variants
// are kept as requested instead of being noisily "corrected".
func extensionMatchesMIME(requestedExt, mimeType, actual string) bool {
	req := strings.ToLower(requestedExt)
	if req == actual {
		return true
	}
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil {
		return false
	}
	return slices.Contains(exts, req)
}

func escapeError(requested string, cause error) error {
	return fmt.Errorf("%w: %q: %w", ErrPathEscape, requested, cause)
}

// isPathEscape reports whether err is os.Root rejecting a path that
// resolves outside its root via a symlink — the one escape vector the
// lexical checks in splitRequestedPath cannot see. os.Root exports no
// sentinel for it as of Go 1.27, so this walks nested PathErrors and matches
// the documented message ("path escapes from parent"); prefer errors.Is
// against a stdlib sentinel if a future release adds one.
func isPathEscape(err error) bool {
	for err != nil {
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) || pathErr.Err == nil {
			return false
		}
		if pathErr.Err.Error() == "path escapes from parent" {
			return true
		}
		err = pathErr.Err
	}
	return false
}
