package path

import "os"

// ExpandPath expands shell-like patterns in a file path:
//   - ~ or ~/ at the start is replaced with the user's home directory
//   - Environment variables like ${HOME} or $HOME are expanded
func ExpandPath(p string) string {
	if p == "" {
		return p
	}

	// Expand environment variables
	p = os.ExpandEnv(p)

	if expanded, err := ExpandHomeDir(p); err == nil {
		return expanded
	}

	return p
}
