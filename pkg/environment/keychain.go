package environment

import "context"

// KeychainProvider is a provider that retrieves secrets using the macOS keychain
// via the `security` command-line tool.
type KeychainProvider struct {
	// binaryPath is the absolute path to the `security` binary, resolved at
	// construction time to avoid TOCTOU races and PATH hijacking (CWE-426).
	binaryPath string
}

type KeychainNotAvailableError struct{}

func (KeychainNotAvailableError) Error() string {
	return "security command is not available (macOS keychain access)"
}

// NewKeychainProvider creates a new KeychainProvider instance.
// It verifies that the `security` command is available on the system and
// stores the resolved absolute path for later use.
func NewKeychainProvider() (*KeychainProvider, error) {
	path, err := lookupBinary("security", KeychainNotAvailableError{})
	if err != nil {
		return nil, err
	}
	return &KeychainProvider{binaryPath: path}, nil
}

// Get retrieves the value of a secret by its service name from the macOS keychain.
// It uses the `security find-generic-password -w -s <name>` command to fetch the password.
func (p *KeychainProvider) Get(ctx context.Context, name string) (string, bool) {
	return runCommand(ctx, "keychain", p.binaryPath, "find-generic-password", "-w", "-s", name)
}
