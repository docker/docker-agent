package workspacemedia

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// WriteExternal stores data at the user-confirmed absolute target with the
// same guarantees as [Write]: the final name is claimed with
// O_CREATE|O_EXCL (dash-suffixed on collision, never overwriting), bytes are
// published atomically, and a conflicting extension is corrected to match
// the MIME type. Missing parent directories are created. Result.RelPath
// carries the ABSOLUTE final path written.
//
// Unlike [Write] there is no containment root: the target was explicitly
// confirmed by the user, so symlinked parents are followed as any user path
// would be. Only the basename's extension is adjusted; everything else must
// be written exactly as confirmed, which is why a non-absolute or unclean
// target — something the confirmation flow could never have shown — is
// rejected with [ErrPathEscape], as is a target that already exists as a
// directory (the confirmation flow pre-resolves directory targets to a
// file inside them).
func WriteExternal(confirmedPath string, data []byte, mimeType string) (Result, error) {
	return writeExternal(confirmedPath, bytes.NewReader(data), mimeType)
}

func writeExternal(confirmedPath string, r io.Reader, mimeType string) (Result, error) {
	if !filepath.IsAbs(confirmedPath) {
		return Result{}, escapeError(confirmedPath, errors.New("external target must be absolute"))
	}
	if filepath.Clean(confirmedPath) != confirmedPath {
		return Result{}, escapeError(confirmedPath, errors.New("external target must be a clean path"))
	}
	dir, base := filepath.Dir(confirmedPath), filepath.Base(confirmedPath)
	if base == "." || base == string(filepath.Separator) || strings.TrimRight(base, ". ") == "" {
		return Result{}, escapeError(confirmedPath, errors.New("external target has no filename"))
	}
	// The confirmation flow resolves an existing-directory target to a file
	// inside it before asking the user (see pkg/runtime.externalMediaTarget),
	// so a directory here means the target changed after confirmation.
	// Dash-suffixing next to it would silently write a sibling the user
	// never confirmed; fail loudly instead.
	if info, err := os.Stat(confirmedPath); err == nil && info.IsDir() {
		return Result{}, escapeError(confirmedPath, errors.New("external target is an existing directory"))
	}

	requestedExt := path.Ext(base)
	if requestedExt == base {
		// Dotfile-style name (".name"): the whole segment is the base.
		requestedExt = ""
	}
	base = strings.TrimSuffix(base, requestedExt)
	ext, corrected := finalExtension(requestedExt, mimeType)

	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // ordinary user directories, same 0o755 convention as Write
		return Result{}, fmt.Errorf("create directory %q: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Result{}, fmt.Errorf("open target directory: %w", err)
	}
	defer root.Close()

	rel, err := claimAndPublish(root, "", base, ext, r)
	if err != nil {
		return Result{}, err
	}
	res := Result{RelPath: filepath.Join(dir, rel)}
	if corrected {
		res.ExtensionCorrected = true
		res.RequestedExtension = requestedExt
	}
	return res, nil
}
