//go:build !darwin

package sandbox

func storedPathCase(path string) (string, error) {
	return path, nil
}
