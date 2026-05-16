package environment

import "context"

// CredentialHelperProvider retrieves Docker credentials using an external CLI command
// configured in the user's global config file.
type CredentialHelperProvider struct {
	command string
	args    []string
}

// NewCredentialHelperProvider creates a new CredentialHelperProvider instance.
// The command parameter is the shell command to execute to retrieve the Docker token.
func NewCredentialHelperProvider(command string, args ...string) *CredentialHelperProvider {
	return &CredentialHelperProvider{command: command, args: args}
}

func (p *CredentialHelperProvider) Get(ctx context.Context, name string) (string, bool) {
	if name != DockerDesktopTokenEnv {
		return "", false
	}

	value, found := runCommand(ctx, "credential helper", p.command, p.args...)
	if !found || value == "" {
		return "", false
	}

	return value, true
}
