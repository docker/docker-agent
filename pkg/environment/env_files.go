package environment

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/path"
)

type KeyValuePair struct {
	Key   string
	Value string
}

func AbsolutePaths(parentDir string, relOrAbsPaths []string) ([]string, error) {
	var absPaths []string

	for _, relOrAbsPath := range relOrAbsPaths {
		absPath, err := AbsolutePath(parentDir, relOrAbsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve path %s: %w", relOrAbsPath, err)
		}
		absPaths = append(absPaths, absPath)
	}

	return absPaths, nil
}

func AbsolutePath(parentDir, relOrAbsPath string) (string, error) {
	p := relOrAbsPath
	if strings.HasPrefix(p, "~") {
		expanded, err := path.ExpandHomeDir(p)
		if err != nil {
			return "", err
		}
		if expanded == p && p != "~" {
			return "", fmt.Errorf("unsupported tilde expansion format: %s", p)
		}
		p = expanded
	}

	// For absolute paths (including tilde-expanded ones), validate against directory traversal
	if filepath.IsAbs(p) {
		if strings.Contains(relOrAbsPath, "..") {
			return "", errors.New("invalid environment file path: path contains directory traversal sequences")
		}
		return p, nil
	}

	validatedPath, err := path.ValidatePathInDirectory(p, parentDir)
	if err != nil {
		return "", fmt.Errorf("invalid environment file path: %w", err)
	}

	return validatedPath, nil
}

func ReadEnvFiles(absolutePaths []string) ([]KeyValuePair, error) {
	if len(absolutePaths) == 0 {
		return nil, nil
	}

	var allLines []KeyValuePair

	for _, absolutePath := range absolutePaths {
		lines, err := ReadEnvFile(absolutePath)
		if err != nil {
			return nil, err
		}
		allLines = append(allLines, lines...)
	}

	return allLines, nil
}

func ReadEnvFile(absolutePath string) ([]KeyValuePair, error) {
	buf, err := os.ReadFile(absolutePath)
	if err != nil {
		return nil, err
	}

	var lines []KeyValuePair

	for line := range strings.SplitSeq(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid env file line: %s", line)
		}

		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
			v = strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`)
		}

		lines = append(lines, KeyValuePair{
			Key:   k,
			Value: v,
		})
	}

	return lines, nil
}
