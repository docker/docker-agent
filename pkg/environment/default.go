package environment

import (
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// NewDefaultProvider creates a provider chain with OS env, run secrets,
// credential helper (if configured), Docker Desktop, pass, and keychain providers.
//
// When running inside a Docker sandbox (detected via SANDBOX_VM_ID), a
// [SandboxTokenProvider] is prepended so that DOCKER_TOKEN is read from the
// JSON file written by the host-side token writer.
func NewDefaultProvider() Provider {
	var providers []Provider

	// Inside a sandbox the Docker Desktop backend API is unreachable and
	// any DOCKER_TOKEN env var is a stale one-shot value.
	// Workaround: Prepend a file-based provider that reads the continuously-refreshed token.
	// The host writes the token file into the config directory (mounted read-only
	// into the sandbox), so we must read from GetConfigDir — not GetDataDir.
	if InSandbox() {
		providers = append(providers,
			NewSandboxTokenProvider(SandboxTokensFilePath(paths.GetConfigDir())),
		)
	}

	providers = append(providers,
		NewOsEnvProvider(),
		NewRunSecretsProvider(),
	)

	// Add credential helper provider if configured
	if cfg, err := userconfig.Load(); err == nil && cfg.CredentialHelper != nil && cfg.CredentialHelper.Command != "" {
		providers = append(providers, NewCredentialHelperProvider(cfg.CredentialHelper.Command, cfg.CredentialHelper.Args...))
	}

	// Docker Desktop provider comes after credential helper
	providers = append(providers, NewDockerDesktopProvider())

	// Append pass provider at the end if available
	if passProvider, err := NewPassProvider(); err == nil {
		providers = append(providers, passProvider)
	}

	// Append keychain provider if available
	if keychainProvider, err := NewKeychainProvider(); err == nil {
		providers = append(providers, keychainProvider)
	}

	return NewMultiProvider(providers...)
}
