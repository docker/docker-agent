package runtime

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker-agent/pkg/chat"
)

// Generated-media filename markers: an explicitly image-output-capable model
// is asked, via a provider-scoped system instruction (see
// pkg/model/provider/gemini/image_output_instruction.go), to emit one exact
//
//	[media-file: relative/path]
//
// line per generated image, in response order. handleStream filters those
// lines out of the visible and persisted assistant text and pairs the
// extracted paths positionally with the accumulated media blobs. A marker
// path is untrusted model input: it flows into [chat.MediaDelta.RequestedPath]
// and through the existing workspacemedia classification, escape-confirmation,
// and never-overwrite policy — the parser itself never touches the filesystem.

const (
	mediaFileMarkerPrefix = "[media-file: "
	mediaFileMarkerSuffix = "]"

	// maxMediaFileMarkerLineBytes bounds a marker line (line terminator
	// excluded); longer lines are ordinary text. This also caps how much
	// text the filter may withhold while a line could still become a marker.
	maxMediaFileMarkerLineBytes = 512
)

// parseMediaFileMarkerLine reports whether line — one complete line without
// its trailing "\n" but possibly with a trailing "\r" — is exactly a
// media-file marker, returning the raw requested path. The grammar is
// deliberately strict (exact lowercase prefix at column zero, closing bracket
// at end of line, no path-edge whitespace, no control characters, valid
// UTF-8, bounded length) so ordinary prose is virtually never swallowed.
func parseMediaFileMarkerLine(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	if len(line) > maxMediaFileMarkerLineBytes {
		return "", false
	}
	rest, ok := strings.CutPrefix(line, mediaFileMarkerPrefix)
	if !ok {
		return "", false
	}
	path, ok := strings.CutSuffix(rest, mediaFileMarkerSuffix)
	if !ok || path == "" {
		return "", false
	}
	if strings.TrimSpace(path) != path {
		return "", false
	}
	if !utf8.ValidString(path) {
		return "", false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return path, true
}

// mediaFileMarkerFilter incrementally strips exact media-file marker lines
// from streamed assistant text, robust to chunks split at any byte boundary.
// Push returns the chunk's visible text — it withholds only bytes that could
// still become a marker line — and Finish flushes the final unterminated
// line, honoring an end-of-stream marker. Extracted paths accumulate in
// paths in response order.
type mediaFileMarkerFilter struct {
	// candidate buffers the current line while it can still become a marker.
	candidate []byte
	// passthrough is set once the current line has diverged from the marker
	// grammar; its remaining bytes then stream through until the newline.
	passthrough bool
	paths       []string
}

func (f *mediaFileMarkerFilter) Push(chunk string) string {
	if chunk == "" {
		return ""
	}
	var out strings.Builder
	for i := range len(chunk) {
		b := chunk[i]
		if f.passthrough {
			out.WriteByte(b)
			if b == '\n' {
				f.passthrough = false
			}
			continue
		}
		f.candidate = append(f.candidate, b)
		if b == '\n' {
			f.flushLine(&out)
			continue
		}
		if !canBecomeMediaFileMarker(f.candidate) {
			out.Write(f.candidate)
			f.candidate = f.candidate[:0]
			f.passthrough = true
		}
	}
	return out.String()
}

// flushLine consumes the buffered complete line (ending in "\n"): a valid
// marker is recorded, anything else is emitted verbatim.
func (f *mediaFileMarkerFilter) flushLine(out *strings.Builder) {
	line := strings.TrimSuffix(string(f.candidate), "\n")
	if path, ok := parseMediaFileMarkerLine(line); ok {
		f.paths = append(f.paths, path)
	} else {
		out.Write(f.candidate)
	}
	f.candidate = f.candidate[:0]
}

// Finish resolves the final unterminated line: an exact end-of-stream marker
// is recorded, anything else is returned as visible text.
func (f *mediaFileMarkerFilter) Finish() string {
	if len(f.candidate) == 0 {
		return ""
	}
	line := string(f.candidate)
	f.candidate = nil
	if path, ok := parseMediaFileMarkerLine(line); ok {
		f.paths = append(f.paths, path)
		return ""
	}
	return line
}

// canBecomeMediaFileMarker reports whether the partial line (no newline yet)
// could still grow into a valid marker line; false diverges the filter to
// passthrough so the withheld bytes are released immediately.
func canBecomeMediaFileMarker(line []byte) bool {
	// +1 tolerates a trailing '\r' still awaiting its '\n' at the bound.
	if len(line) > maxMediaFileMarkerLineBytes+1 {
		return false
	}
	if len(line) <= len(mediaFileMarkerPrefix) {
		return string(line) == mediaFileMarkerPrefix[:len(line)]
	}
	if !bytes.HasPrefix(line, []byte(mediaFileMarkerPrefix)) {
		return false
	}
	for i := len(mediaFileMarkerPrefix); i < len(line); i++ {
		b := line[i]
		if b == '\r' {
			// A carriage return can only precede the terminating newline.
			return i == len(line)-1
		}
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// applyMediaFileRequestedPaths pairs extracted marker paths with the turn's
// media blobs by position: marker i names blob i, overriding any
// provider-supplied requested path for that slot. Blobs beyond the last
// marker keep their existing naming fallback (provider display name, then
// the generic generated-N); extra markers are already stripped from the text
// and are simply ignored.
func applyMediaFileRequestedPaths(media []chat.MediaDelta, paths []string) {
	for i := range media {
		if i >= len(paths) {
			return
		}
		media[i].RequestedPath = paths[i]
	}
}
