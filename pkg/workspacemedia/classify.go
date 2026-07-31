package workspacemedia

import (
	"slices"
	"strings"
)

// PathClass is the outcome of classifying a model-requested target path
// before any I/O. Classification is purely lexical: a PathWorkspaceRelative
// path can still be refused at write time when a symlinked parent resolves
// outside the root (surfaced as [ErrPathEscape] by [Write]).
type PathClass int

const (
	// PathInvalid marks a path that names nothing usable: empty, only
	// separators/dots, an invalid or Windows-reserved final segment. It is
	// not confirmable — callers should fall back to a safe generated name.
	PathInvalid PathClass = iota

	// PathWorkspaceRelative marks a path contained under the workspace
	// root, directly writable via [Write].
	PathWorkspaceRelative

	// PathEscaping marks a path that targets a location outside the
	// workspace: absolute, traversing above the root via "..", or rooted at
	// a home directory via a leading "~" segment. Callers must obtain an
	// explicit user confirmation before writing there.
	PathEscaping
)

// ClassifyRequestedPath classifies requested and, for PathWorkspaceRelative,
// returns the normalized slash-separated relative path to hand to [Write]
// (both separator styles accepted; interior "." and ".." segments resolved
// lexically, so "a/../b.png" is contained rather than escaping). For every
// other class the second return is "".
func ClassifyRequestedPath(requested string) (PathClass, string) {
	if isAbsolutePath(requested) {
		return PathEscaping, ""
	}
	segments := splitPathSegments(requested)
	if len(segments) > 0 && strings.HasPrefix(segments[0], "~") {
		return PathEscaping, ""
	}

	var stack []string
	for _, seg := range segments {
		switch seg {
		case ".":
		case "..":
			if len(stack) == 0 {
				return PathEscaping, ""
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	cleaned := strings.Join(stack, "/")
	if _, _, _, err := splitRequestedPath(cleaned); err != nil {
		return PathInvalid, ""
	}
	return PathWorkspaceRelative, cleaned
}

// RequestedBasename returns the final meaningful segment of a requested
// path — the name to redirect to when the full path is refused — or ""
// when none exists (empty, separators only, or only "."/".." segments).
func RequestedBasename(requested string) string {
	segments := splitPathSegments(requested)
	for _, seg := range slices.Backward(segments) {
		if seg != "." && seg != ".." {
			return seg
		}
	}
	return ""
}

// splitPathSegments splits on both separator styles, mirroring
// splitRequestedPath: model-provided paths may be Windows-style.
func splitPathSegments(requested string) []string {
	return strings.FieldsFunc(requested, func(r rune) bool { return r == '/' || r == '\\' })
}
