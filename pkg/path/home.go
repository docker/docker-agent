package path

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ExpandHomeDir expands a leading home-directory reference in a path.
// It expands "~", "~/...", and "~\\..." on Windows. Other tilde forms,
// such as "~user/...", are returned unchanged.
func ExpandHomeDir(path string) (string, error) {
	if !isHomeDirPath(path) {
		return path, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		return "", errors.New("failed to get user home directory")
	}

	if path == "~" {
		return filepath.Clean(homeDir), nil
	}

	return filepath.Join(homeDir, path[2:]), nil
}

func isHomeDirPath(path string) bool {
	return path == "~" || strings.HasPrefix(path, "~/") || (filepath.Separator == '\\' && strings.HasPrefix(path, `~\`))
}
