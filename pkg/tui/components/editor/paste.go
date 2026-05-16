package editor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// validateFilePath checks that a path is safe: no path traversal, no symlinks.
func validateFilePath(path string) (os.FileInfo, error) {
	if strings.Contains(path, "..") {
		return nil, os.ErrPermission
	}

	clean := filepath.Clean(path)

	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrPermission
	}
	return info, nil
}

// Supported file extensions for drag-and-drop attachments
var supportedFileExtensions = []string{
	// Images
	".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg",
	// PDFs
	".pdf",
	// Text files (future)
	// ".txt", ".md", ".json", ".yaml", ".yml", ".toml",
}

// ParsePastedFiles attempts to parse pasted content as file paths.
// It handles different terminal formats:
// - Unix: space-separated with backslash escaping
// - Windows Terminal: quote-wrapped paths
// - Single file: just the path
//
// Returns nil if the content doesn't look like file paths.
func ParsePastedFiles(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	// NOTE: Rio terminal on Windows adds NULL chars for some reason.
	s = strings.ReplaceAll(s, "\x00", "")

	// Try simple stat first - if all lines are valid files, use them
	if attemptStatAll(s) {
		return strings.Split(s, "\n")
	}

	// Detect Windows Terminal format (quote-wrapped)
	if os.Getenv("WT_SESSION") != "" {
		return windowsTerminalParsePastedFiles(s)
	}

	// Default to Unix format (space-separated with backslash escaping)
	return unixParsePastedFiles(s)
}

// attemptStatAll tries to stat each line as a file path.
// Returns true if ALL lines exist as regular files (not directories or symlinks).
func attemptStatAll(s string) bool {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return false
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		info, err := validateFilePath(line)
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// windowsTerminalParsePastedFiles parses Windows Terminal format.
// Windows Terminal wraps file paths in quotes: "C:\path\to\file.png"
func windowsTerminalParsePastedFiles(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}

	var (
		paths    []string
		current  strings.Builder
		inQuotes = false
	)

	for i := range len(s) {
		ch := s[i]

		switch {
		case ch == '"':
			if inQuotes {
				// End of quoted section
				if current.Len() > 0 {
					paths = append(paths, current.String())
					current.Reset()
				}
				inQuotes = false
			} else {
				// Start of quoted section
				inQuotes = true
			}
		case inQuotes:
			current.WriteByte(ch)
		case ch != ' ' && ch != '\n' && ch != '\r':
			// Text outside quotes is not allowed
			return nil
		}
	}

	// Add any remaining content if quotes were properly closed
	if current.Len() > 0 && !inQuotes {
		paths = append(paths, current.String())
	}

	// If quotes were not closed, return nil (malformed input)
	if inQuotes {
		return nil
	}

	return paths
}

// unixParsePastedFiles parses Unix terminal format.
// Unix terminals use space-separated paths with backslash escaping.
// Example: /path/to/file1.png /path/to/my\ file\ with\ spaces.jpg
func unixParsePastedFiles(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}

	var (
		paths   []string
		current strings.Builder
		escaped = false
	)

	for i := range len(s) {
		ch := s[i]

		switch {
		case escaped:
			// After a backslash, add the character as-is (including space)
			current.WriteByte(ch)
			escaped = false
		case ch == '\\':
			if i == len(s)-1 {
				// Trailing backslash is malformed input; strip it
				break
			}
			escaped = true
		case ch == ' ' || ch == '\n' || ch == '\r':
			// Space/newline separates paths (unless escaped)
			if current.Len() > 0 {
				paths = append(paths, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}

	// Handle trailing backslash if present
	if escaped {
		current.WriteByte('\\')
	}

	// Add the last path if any
	if current.Len() > 0 {
		paths = append(paths, current.String())
	}

	return paths
}

// IsSupportedFileType checks if a file has a supported extension.
func IsSupportedFileType(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return slices.Contains(supportedFileExtensions, ext)
}
