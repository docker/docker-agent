package environment

import "context"

// PassProvider is a provider that retrieves secrets using the `pass` password
// manager.
type PassProvider struct {
	// binaryPath is the absolute path to the `pass` binary, resolved at
	// construction time to avoid TOCTOU races and PATH hijacking (CWE-426).
	binaryPath string
}

type PassNotAvailableError struct{}

func (PassNotAvailableError) Error() string {
	return "pass is not installed"
}

// NewPassProvider creates a new PassProvider instance.
// It verifies that `pass` is available and stores the resolved absolute path.
func NewPassProvider() (*PassProvider, error) {
	path, err := lookupBinary("pass", PassNotAvailableError{})
	if err != nil {
		return nil, err
	}
	return &PassProvider{binaryPath: path}, nil
}

// Get retrieves the value of a secret by its name using the `pass` CLI.
// The name corresponds to the path in the `pass` store.
func (p *PassProvider) Get(ctx context.Context, name string) (string, bool) {
	return runCommand(ctx, "pass", p.binaryPath, "show", name)
}
